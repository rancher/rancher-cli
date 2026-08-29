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

type rotateSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewRotateSecret builds `oidc-client rotate-secret <client> [secret-name]`
// (mutating): sets the regenerate annotation, waits for the controller to
// replace the named secret's value, and prints the new value once.
// Destructive + --yes. Omitting secret-name is legal only when the client
// has exactly one secret.
func NewRotateSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &rotateSecretCmd{clients: clients}
}

func (rotateSecretCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "oidc-client rotate-secret",
		Usage: "Regenerate a client secret on an OIDC client",
		Desc: "Sets the regenerate annotation, waits for the controller to replace the named secret's " +
			"value, and prints the new value once. secret-name may be omitted only when the client has " +
			"exactly one secret.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "client", Type: "string", Required: true, Desc: "OIDC client name"},
			{Name: "secret-name", Type: "string", Desc: "secret to regenerate (required when the client has more than one)"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "timeout", Type: "string", Desc: "wait `timeout` for the controller to regenerate the secret, e.g. 90s (default 60s)"},
		},
		Examples:     []string{"rancher oidc-client rotate-secret my-app client-secret-1 --apply --yes"},
		Destructive:  "true",
		Network:      true,
		Auth:         true,
		Capability:   capability(),
		OutputPolicy: runtime.OnceOnlyOutputPolicy(),
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeText,
	}
}

func (c *rotateSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	clientName := args[0]
	requested := ""
	if len(args) > 1 {
		requested = args[1]
	}
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

	secrets, _, _ := unstructured.NestedMap(orig.Object, "status", "clientSecrets")
	secretName, err := resolveSecretName(clientName, secrets, requested, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	// Plan-time internal correlation read — held in memory, never printed:
	// this doubles as rotate's delivery preflight (a caller who cannot read
	// the backing Secret now cannot be delivered the new value either), so
	// a FORBIDDEN/NOT_FOUND here is an ordinary pre-mutation refusal, no
	// committed-mutation details.
	oldValue, err := readClientSecret(ctx, c.clients, clientID, secretName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	patched, eff, err := planAnnotationUpdate(ctx, client, k, orig, annotationSecretRegenerate, secretName)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = append(eff.Warnings, fmt.Sprintf("regenerates secret %s on %s; the current value stops working", secretName, clientName))

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, patched, clientID, clientName, secretName, oldValue, timeout)
	}
	return eff, exec, nil
}

func (c *rotateSecretCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, patched *unstructured.Unstructured, clientID, clientName, secretName, oldValue string, timeout time.Duration) (runtime.Result, error) {
	if _, err := client.Update(ctx, k.GVR, "", patched, false); err != nil {
		return runtime.Result{}, err
	}
	getRecovery := recoveryHint(clientName)

	var newValue string
	check := func(ctx context.Context, obj *unstructured.Unstructured) waitOutcome {
		if annotationPresent(obj, annotationSecretRegenerate) {
			return waitOutcome{} // not yet consumed
		}
		v, err := readClientSecret(ctx, c.clients, clientID, secretName)
		if err != nil {
			if isTransientSecretReadError(err) {
				return waitOutcome{} // transient; retry on the next tick
			}
			return waitOutcome{terminal: committedPartialSuccess(err, "OIDCClient", clientName, secretName, getRecovery)}
		}
		// Full-value comparison, never lastFiveCharacters: a five-character
		// suffix can collide across a rotation, which would strand a
		// consumed rotation in a timeout — the highest-cost partial
		// success this predicate exists to avoid.
		if v == oldValue {
			return waitOutcome{terminal: rotateUnchangedValueError(clientName, secretName, getRecovery)}
		}
		newValue = v
		return waitOutcome{done: true, value: secretName}
	}
	_, err := waitForOIDCClient(ctx, client, k.GVR, clientName, timeout, check,
		func(last *unstructured.Unstructured) *clierr.Error {
			// Rotate's timeout IS retryable: its stated recovery is
			// literally rotating the same key again.
			return clierr.Retryable(clierr.CodeGeneric,
				fmt.Sprintf("timed out waiting for oidc-client %s to regenerate secret %s", clientName, secretName)).
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName,
					Recovery: "rotate the same name again once the controller is healthy",
				})
		})
	if err != nil {
		return runtime.Result{}, err
	}

	secretLine, shownOnce := runtime.OnceOnlyLine("client secret", newValue, "client secret")
	res := runtime.MutationText("oidcclient", clientName, "patched",
		"secret "+secretName+" rotated",
		secretLine,
	)
	res.Kind = "OIDCClientSecretRotated"
	res.Data = map[string]any{"clientID": clientID, "secretName": secretName, "clientSecret": newValue}
	res.OnceOnlyNotice = shownOnce
	res.CommittedContext = &runtime.CommittedContext{
		ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName, Recovery: getRecovery,
	}
	return res, nil
}

// isTransientSecretReadError reports whether err should be tolerated and
// retried on the next poll tick, mirroring waitForOIDCClient's own
// contract for its OIDCClient Get (wait.go: "a transient Get error
// retries ... exactly like waitFor"): a *clierr.Error explicitly
// classified Retryable, or any error that never went through clierr's
// classification at all (a raw transport error), is transient. A
// *clierr.Error that is NOT retryable (RBAC, NOT_FOUND) is a definitive,
// terminal failure — waiting for the deadline could never fix it.
func isTransientSecretReadError(err error) bool {
	ce, ok := err.(*clierr.Error)
	if !ok {
		return true
	}
	return ce.Retryable
}

// rotateUnchangedValueError is rotate's terminal error for the (should-be
// impossible with full-value comparison) case where the controller
// consumed the regenerate annotation but the backing value did not change.
func rotateUnchangedValueError(clientName, secretName, recovery string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("oidc-client %s: secret %s did not change even though the controller consumed the rotation request", clientName, secretName)).
		WithDetails(clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName, Recovery: recovery,
		})
}
