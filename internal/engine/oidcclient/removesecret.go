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

type removeSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewRemoveSecret builds `oidc-client remove-secret <client> <secret-name>`
// (mutating): sets the remove annotation and waits for the controller to
// drop the named secret. Destructive + --yes. Delivers no once-only value
// (ordinary Delivery), unlike add-secret/rotate-secret.
func NewRemoveSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &removeSecretCmd{clients: clients}
}

func (removeSecretCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "oidc-client remove-secret",
		Usage:       "Remove a client secret from an OIDC client",
		Desc:        "Sets the remove annotation and waits for the controller to drop the named secret.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "client", Type: "string", Required: true, Desc: "OIDC client name"},
			{Name: "secret-name", Type: "string", Required: true, Desc: "secret to remove"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "timeout", Type: "string", Desc: "wait `timeout` for the controller to remove the secret, e.g. 90s (default 60s)"},
		},
		Examples:    []string{"rancher oidc-client remove-secret my-app client-secret-2 --apply --yes"},
		Destructive: "true",
		Network:     true,
		Auth:        true,
		Capability:  capability(),
		Shape:       runtime.ShapeText,
	}
}

func (c *removeSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	clientName, secretName := args[0], args[1]
	timeout, err := parseWaitTimeout(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	client, k, orig, err := fetchOIDCClient(ctx, c.clients, clientName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	secrets, _, _ := unstructured.NestedMap(orig.Object, "status", "clientSecrets")
	if _, err := resolveSecretName(clientName, secrets, secretName, false); err != nil {
		return safety.Effect{}, nil, err
	}
	clientID, _, _ := unstructured.NestedString(orig.Object, "status", "clientID")

	patched, eff, err := planAnnotationUpdate(ctx, client, k, orig, annotationSecretRemove, secretName)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = append(eff.Warnings, fmt.Sprintf("removes secret %s from %s; anything using it stops working", secretName, clientName))

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, patched, clientID, clientName, secretName, timeout)
	}
	return eff, exec, nil
}

func (c *removeSecretCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, patched *unstructured.Unstructured, clientID, clientName, secretName string, timeout time.Duration) (runtime.Result, error) {
	if _, err := client.Update(ctx, k.GVR, "", patched, false); err != nil {
		return runtime.Result{}, err
	}

	check := func(_ context.Context, obj *unstructured.Unstructured) waitOutcome {
		if annotationPresent(obj, annotationSecretRemove) {
			return waitOutcome{} // not yet consumed
		}
		secrets, _, _ := unstructured.NestedMap(obj.Object, "status", "clientSecrets")
		if _, stillThere := secrets[secretName]; stillThere {
			return waitOutcome{} // consumed but status not yet caught up
		}
		return waitOutcome{done: true}
	}
	// remove-secret's timeout is NOT retryable: the stated recovery is a
	// read first, per its contract - Plan-time validation (kept strict)
	// would fail a naive re-run whose first attempt landed after the last
	// poll.
	_, err := waitForOIDCClient(ctx, client, k.GVR, clientName, timeout, check,
		func(last *unstructured.Unstructured) *clierr.Error {
			return clierr.New(clierr.CodeGeneric,
				fmt.Sprintf("timed out waiting for oidc-client %s to remove secret %s", clientName, secretName)).
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "OIDCClient", ResourceName: clientName, SecretName: secretName,
					Recovery: recoveryHint(clientName) + " first; the removal landed iff the key is gone; re-run only while the key remains",
				})
		})
	if err != nil {
		return runtime.Result{}, err
	}

	res := runtime.MutationText("oidcclient", clientName, "patched",
		"secret "+secretName+" removed - anything using it stops working")
	res.Kind = "OIDCClientSecretRemoved"
	res.Data = map[string]any{"clientID": clientID, "secretName": secretName}
	return res, nil
}
