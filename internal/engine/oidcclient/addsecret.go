package oidcclient

import (
	"context"
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type addSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewAddSecret builds `oidc-client add-secret <client>` (mutating): sets
// the create annotation, waits for the controller to mint a new secret,
// and prints it once. Destructive + --yes (mint-and-reveal, the create
// token precedent).
func NewAddSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &addSecretCmd{clients: clients}
}

func (addSecretCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "oidc-client add-secret",
		Usage:       "Add a new client secret to an OIDC client",
		Desc:        "Sets the create annotation, waits for the controller to generate a new secret, and prints it once.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "client", Type: "string", Required: true, Desc: "OIDC client name"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "timeout", Type: "string", Desc: "wait `timeout` for the controller to add the secret, e.g. 90s (default 60s)"},
		},
		Examples:     []string{"rancher oidc-client add-secret my-app --apply --yes"},
		Destructive:  "true",
		Network:      true,
		Auth:         true,
		Capability:   capability(),
		OutputPolicy: runtime.OnceOnlyOutputPolicy(),
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeText,
	}
}

func (c *addSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	clientName := in.Args()[0]
	timeout, err := parseWaitTimeout(in.String("timeout"))
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, k, orig, err := fetchOIDCClient(ctx, c.clients, clientName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	clientID, _, _ := unstructured.NestedString(orig.Object, "status", "clientID")
	if clientID == "" {
		return safety.Effect{}, nil, errNotProvisioned(clientName)
	}

	// Delivery preflight: verify the backing Secret is readable BEFORE any
	// dry-run/write — a manage-oidc-clients-only persona could not be
	// delivered the new value either way, so refuse pre-mutation rather
	// than mint an undeliverable secret.
	if err := preflightReadSecret(ctx, c.clients, clientID); err != nil {
		return safety.Effect{}, nil, err
	}

	baseline := secretKeySet(orig.Object)

	patched, eff, err := planAnnotationUpdate(ctx, client, k, orig, annotationSecretCreate, "true")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = append(eff.Warnings, fmt.Sprintf("creates a new client secret on %s", clientName))

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, patched, clientID, clientName, baseline, timeout)
	}
	return eff, exec, nil
}

func (c *addSecretCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, patched *unstructured.Unstructured, clientID, clientName string, baseline map[string]bool, timeout time.Duration) (runtime.Result, error) {
	if _, err := client.Update(ctx, k.GVR, "", patched, false); err != nil {
		return runtime.Result{}, err
	}
	getRecovery := recoveryHint(clientName)

	check := func(_ context.Context, obj *unstructured.Unstructured) waitOutcome {
		if annotationPresent(obj, annotationSecretCreate) {
			return waitOutcome{} // not yet consumed
		}
		current := secretKeySet(obj.Object)
		newKeys, missing := setDelta(baseline, current)
		if len(missing) > 0 || len(newKeys) != 1 {
			return waitOutcome{terminal: addSecretAmbiguousShape(clientName, baseline, current)}
		}
		return waitOutcome{done: true, value: newKeys[0]}
	}
	secretName, err := waitForOIDCClient(ctx, client, k.GVR, clientName, timeout, check,
		func(last *unstructured.Unstructured) *clierr.Error {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("timed out waiting for oidc-client %s to add a secret", clientName)).
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName,
					Recovery: "check " + getRecovery + "; if a new key landed its value was never shown — rotate that key by name",
				})
		})
	if err != nil {
		return runtime.Result{}, err
	}

	plaintext, err := readClientSecret(ctx, c.clients, clientID, secretName)
	if err != nil {
		return runtime.Result{}, committedPartialSuccess(err, "OIDCClient", clientName, secretName, getRecovery)
	}

	return runtime.Result{
		Kind:     "OIDCClientSecretAdded",
		Data:     map[string]any{"clientID": clientID, "secretName": secretName, "clientSecret": plaintext},
		Warnings: []string{"this client secret is shown exactly once — store it now"},
		CommittedContext: &runtime.CommittedContext{
			ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName, Recovery: getRecovery,
		},
	}, nil
}

// addSecretAmbiguousShape builds add-secret's terminal, non-retryable error
// for any key-set transition other than exactly-baseline-plus-one-new-key
// — a bare "one new key" check would bless a concurrent removal
// (baseline {1} -> observed {2} has exactly one new key too), so a missing
// baseline key, a second new key, or any other shape must all fail
// immediately rather than be misread as success.
func addSecretAmbiguousShape(clientName string, baseline, current map[string]bool) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("oidc-client %s: observed an unexpected secret-set change while adding a secret (baseline %v, observed %v) — a concurrent actor changed this client while add-secret was waiting",
			clientName, sortedKeysOf(baseline), sortedKeysOf(current))).
		WithDetails(clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName,
			Recovery: recoveryHint(clientName),
		})
}
