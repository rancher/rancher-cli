package oidcclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type addSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewAddSecret builds `oidc-client add-secret <client>` (mutating): sets
// the create annotation, waits for the controller to generate a new secret,
// and prints it once. Destructive + --yes (generate-and-reveal, the create
// token precedent).
func NewAddSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &addSecretCmd{clients: clients}
}

func (addSecretCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "oidc-client add-secret",
		Usage:       "Add a new client secret to an OIDC client",
		Desc:        "Sets the cattle.io/oidc-client-secret-create annotation on the client, waits for Rancher's controller to generate a new client secret, and prints it once.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "client", Type: "string", Required: true, Desc: "OIDC client name"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "timeout", Type: "string", Desc: "wait `timeout` for the controller to add the secret, e.g. 90s (default 60s)"},
		},
		Examples:     []string{"rancher oidc-client add-secret my-app --apply --yes"},
		Destructive:  "true",
		Auth:         true,
		Capability:   capability(),
		OutputPolicy: runtime.OnceOnlyOutputPolicy(),
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeText,
	}
}

func (c *addSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	clientName := in.Args()[0]
	timeout, err := parseWaitTimeout(in)
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
	// dry-run/write - a manage-oidc-clients-only persona could not be
	// delivered the new value either way, so refuse pre-mutation rather
	// than generate an undeliverable secret.
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
		newKeys, missing := setDelta(baseline, secretKeySet(obj.Object))
		switch {
		case len(newKeys) == 0 && len(missing) == 0:
			// Rancher's controller removes the annotation in one write and
			// updates status in a later one, so a consumed request with an
			// unchanged key set is the state between the two.
			return waitOutcome{}
		case len(newKeys) != 1 || len(missing) > 0:
			return waitOutcome{terminal: addSecretConcurrentChange(clientName, newKeys, missing)}
		}
		return waitOutcome{done: true, value: newKeys[0]}
	}
	// A new key may have landed before the wait ended, so the timeout and
	// an aborted wait share the recovery that says how to find and rotate it.
	recovery := "check " + getRecovery + "; if a new key landed its value was never shown - rotate that key by name"
	secretName, err := waitForOIDCClient(ctx, client, k.GVR, clientName, timeout, check,
		func(last *unstructured.Unstructured) *clierr.Error {
			msg := fmt.Sprintf("timed out waiting for oidc-client %s to add a secret", clientName)
			if last != nil && !annotationPresent(last, annotationSecretCreate) {
				msg += ": the cattle.io/oidc-client-secret-create annotation was removed but no new client secret appeared"
			}
			return clierr.New(clierr.CodeGeneric, msg).
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName,
					Recovery: recovery,
				})
		},
		func(err error) *clierr.Error {
			return committedPartialSuccess(err, "OIDCClient", clientName, "", recovery)
		})
	if err != nil {
		return runtime.Result{}, err
	}

	plaintext, err := readClientSecret(ctx, c.clients, clientID, secretName)
	if err != nil {
		return runtime.Result{}, committedPartialSuccess(err, "OIDCClient", clientName, secretName, getRecovery)
	}

	secretLine, shownOnce := runtime.OnceOnlyLine("client secret", plaintext, "client secret")
	res := runtime.MutationText("oidcclient", clientName, "patched",
		"secret "+output.Printable(secretName)+" added",
		secretLine,
	)
	res.Kind = "OIDCClientSecretAdded"
	res.Data = map[string]any{"clientID": clientID, "secretName": secretName, "clientSecret": plaintext}
	res.OnceOnlyNotice = shownOnce
	res.CommittedContext = &runtime.CommittedContext{
		ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName, Recovery: getRecovery,
	}
	return res, nil
}

// addSecretConcurrentChange builds add-secret's terminal, non-retryable
// error for a key-set change other than exactly one new key - a bare "one
// new key" check would bless a concurrent removal (baseline {1} -> observed
// {2} has exactly one new key too), so a missing baseline key or a second
// new key must fail immediately rather than be misread as success. The
// message names the keys that appeared and the keys that disappeared.
func addSecretConcurrentChange(clientName string, newKeys, missing []string) *clierr.Error {
	var msg string
	if len(newKeys) == 0 {
		msg = fmt.Sprintf("oidc-client %s: add-secret expected exactly one new client secret, but none was added and some were removed (removed: %s); another change to this client, or Rancher's controller handling the annotation twice, can do this",
			clientName, strings.Join(missing, ", "))
	} else {
		change := []string{"added: " + strings.Join(newKeys, ", ")}
		if len(missing) > 0 {
			change = append(change, "removed: "+strings.Join(missing, ", "))
		}
		msg = fmt.Sprintf("oidc-client %s: its client secrets changed by more than the one new secret add-secret asked for (%s); another change to this client, or Rancher's controller handling the annotation twice, can do this",
			clientName, strings.Join(change, "; "))
	}
	return clierr.New(clierr.CodeGeneric, msg).
		WithDetails(clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName,
			Recovery: recoveryHint(clientName),
		})
}
