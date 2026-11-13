package oidcclient

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Annotation keys the oidc-provider controller watches on an OIDCClient to
// drive the secret lifecycle (observed against real Rancher v2.14 servers,
// both editions): create's value is the literal "true"; regenerate/remove's
// value is the target secret's key name. The controller consumes (removes)
// the annotation once it has processed the request.
const (
	annotationSecretCreate     = "cattle.io/oidc-client-secret-create"
	annotationSecretRegenerate = "cattle.io/oidc-client-secret-regenerate"
	annotationSecretRemove     = "cattle.io/oidc-client-secret-remove"
)

// fetchOIDCClient resolves the oidcclient kind row, a local-cluster client,
// and the named OIDCClient in one call - the front half every
// secret-lifecycle verb's Plan shares before its own validation (which
// varies per verb: add-secret and rotate-secret both guard on
// status.clientID being set, rotate-secret and remove-secret both resolve a
// secret name, only rotate-secret does a correlation read) runs.
func fetchOIDCClient(ctx context.Context, clients engine.ClusterClientFactory, clientName string) (engine.ClusterClient, kinds.Kind, *unstructured.Unstructured, error) {
	k := oidcClientKind()
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil, kinds.Kind{}, nil, err
	}
	orig, err := client.Get(ctx, k.GVR, "", clientName)
	if err != nil {
		return nil, kinds.Kind{}, nil, err
	}
	return client, k, orig, nil
}

// errNotProvisioned is add-secret's and rotate-secret's shared Plan-time
// guard error for a client whose status.clientID is empty - a manual status
// patch could produce this even when status.clientSecrets is populated, so
// neither verb may skip straight to a Secret read on the strength of the
// secrets map alone.
func errNotProvisioned(clientName string) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		clierr.Text(fmt.Sprintf("oidc-client %s has not been provisioned by the controller; check the oidc-provider feature", clientName),
			clierr.Lead(", then "), recoveryCommand(clientName)))
}

// planAnnotationUpdate sets annotationKey to annotationValue on a deep copy
// of orig and Plans the update - the get-object → set-annotation →
// DiffObjects → PlanUpdate sequence every secret-lifecycle verb shares once
// its own Plan-time validation has resolved the annotation value. Returns
// the patched object too: execute needs the exact object client.Update will
// send.
func planAnnotationUpdate(ctx context.Context, client engine.ClusterClient, k kinds.Kind, orig *unstructured.Unstructured, annotationKey, annotationValue string) (*unstructured.Unstructured, safety.Effect, error) {
	patched := orig.DeepCopy()
	_ = unstructured.SetNestedField(patched.Object, annotationValue, "metadata", "annotations", annotationKey)
	patched.SetResourceVersion(orig.GetResourceVersion())
	diff := engine.DiffObjects(orig.Object, patched.Object)
	eff, _, err := engine.PlanUpdate(ctx, client, k.GVR, "", patched, k.Redaction(), engine.LocalCluster, k.Name, "OIDCClient", diff, k.Sensitive)
	return patched, eff, err
}

// preflightReadSecret GETs the backing Secret for clientID and discards the
// result - add-secret's delivery preflight: a caller who cannot read the
// backing Secret cannot be delivered the new value either, so this fails
// BEFORE any dry-run/write, an ordinary pre-mutation RBAC/NOT_FOUND with no
// committed-mutation details.
func preflightReadSecret(ctx context.Context, clients engine.ClusterClientFactory, clientID string) error {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return err
	}
	_, err = client.Get(ctx, secretGVR, secretNamespace, clientID)
	return err
}

// secretKeySet returns the status.clientSecrets key set of obj.
func secretKeySet(obj map[string]any) map[string]bool {
	secrets, _, _ := unstructured.NestedMap(obj, "status", "clientSecrets")
	out := make(map[string]bool, len(secrets))
	for k := range secrets {
		out[k] = true
	}
	return out
}

// setDelta compares two secret key sets: newKeys are present in current but
// not baseline, missing are present in baseline but not current. Both
// results are sorted for stable error messages.
func setDelta(baseline, current map[string]bool) (newKeys, missing []string) {
	for k := range current {
		if !baseline[k] {
			newKeys = append(newKeys, k)
		}
	}
	for k := range baseline {
		if !current[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(newKeys)
	sort.Strings(missing)
	return newKeys, missing
}

// annotationPresent reports whether obj's metadata.annotations carries key.
func annotationPresent(obj *unstructured.Unstructured, key string) bool {
	anns, _, _ := unstructured.NestedStringMap(obj.Object, "metadata", "annotations")
	_, ok := anns[key]
	return ok
}

// toSecretCandidates builds the clierr.Candidate list for a secret-name
// VALIDATION error's "must be one of" list.
func toSecretCandidates(names []string) []clierr.Candidate {
	out := make([]clierr.Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, clierr.Candidate{ID: n, Name: n})
	}
	return out
}

// resolveSecretName validates a requested secret name against the Plan-time
// status.clientSecrets set. An explicitly named secret must exist - the
// controller SILENTLY SKIPS an unknown name while still consuming the
// annotation, which would wait to a timeout that misblames the controller,
// so this check runs strictly before any write, listing the actual names
// as candidates. Omission is legal only when allowOmit is true AND exactly
// one secret exists; an implicit pick would kill a live credential the
// operator did not choose.
func resolveSecretName(clientName string, secrets map[string]any, requested string, allowOmit bool) (string, error) {
	names := slices.Sorted(maps.Keys(secrets))

	if requested != "" {
		if slices.Contains(names, requested) {
			return requested, nil
		}
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("oidc-client %s has no secret named %q", clientName, requested)).
			WithCandidates(toSecretCandidates(names))
	}
	if !allowOmit {
		return "", clierr.New(clierr.CodeValidation, "secret-name is required").
			WithCandidates(toSecretCandidates(names))
	}
	switch len(names) {
	case 0:
		return "", clierr.New(clierr.CodeValidation, fmt.Sprintf("oidc-client %s has no secrets", clientName))
	case 1:
		return names[0], nil
	default:
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("oidc-client %s has %d secrets; name one", clientName, len(names))).
			WithCandidates(toSecretCandidates(names))
	}
}
