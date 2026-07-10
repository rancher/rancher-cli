package extres

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type createTokenCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateToken builds `create token [--ttl <dur>] [--description <d>]
// [--cluster <c>]` (mutating): the ext Token store mints a derived bearer
// token (spec.kind stays empty — only Rancher's own login flow sets
// "session"). The store ignores metadata.name/generateName and always
// generates one, so neither is set here.
func NewCreateToken(clients engine.ClusterClientFactory) runtime.Command {
	return &createTokenCmd{clients: clients}
}

func (createTokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create token",
		Usage:       "Mint a server-side derived token",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: "ttl", Type: "string"},
			{Name: "description", Type: "string"},
			{Name: "cluster", Type: "string"},
		},
		Examples: []string{
			"rancher create token --ttl 90d --description ci --apply --yes",
		},
		Destructive: "true", // the token kind is Sensitive
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c createTokenCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	ttl, err := ParseTTL(in.String("ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}

	spec := map[string]any{}
	if desc := in.String("description"); desc != "" {
		spec["description"] = desc
	}
	if ttl > 0 {
		spec["ttl"] = ttl.Milliseconds()
	}
	if clusterFlag := in.String("cluster"); clusterFlag != "" {
		id, err := c.clients.ResolveCluster(ctx, clusterFlag)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		spec["clusterName"] = id
	}

	tokenKind, _ := kinds.Lookup("token")
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   map[string]any{},
		"spec":       spec,
	}}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, _, err := engine.PlanCreate(ctx, client, tokenKind.GVR, "", obj, engine.LocalCluster, "Token")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// Creating a Sensitive kind is destructive and must gate on --yes exactly
	// as patching one does (secret.go:141/184, resources/create.go:122) — the
	// shared PlanCreate has no notion of Sensitive, so the caller sets it.
	eff.Destructive = tokenKind.Sensitive

	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, tokenKind.GVR, "", obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		bearer, _, _ := unstructured.NestedString(created.Object, "status", "bearerToken")
		expiresAt, _, _ := unstructured.NestedString(created.Object, "status", "expiresAt")
		return runtime.Result{
			Kind: "TokenCreated",
			Data: map[string]string{
				"name":        created.GetName(),
				"bearerToken": bearer,
				"expiresAt":   expiresAt,
			},
			Warnings: []string{"this token value is shown exactly once — store it now"},
		}, nil
	}
	return eff, exec, nil
}
