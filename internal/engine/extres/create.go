package extres

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type createTokenCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateToken builds `create token [--ttl <dur>] [--description <d>]
// [--cluster <c>]` (mutating): the ext Token store creates a derived bearer
// token (spec.kind stays empty - only Rancher's own login flow sets
// "session"). The store ignores metadata.name/generateName and always
// generates one, so neither is set here.
func NewCreateToken(clients engine.ClusterClientFactory) runtime.Command {
	return &createTokenCmd{clients: clients}
}

func (createTokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create token",
		Usage:       "Create a token on the server",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: "ttl", Type: "string", Desc: "token expiration, e.g. 30d, 12h (omit for server default)"},
			{Name: "description", Type: "string", Desc: "description of the token, to identify it"},
			{Name: "cluster", Type: "string", Desc: "scope the token to this cluster `id|name`"},
		},
		Examples: []string{
			"rancher create token --ttl 90d --description ci --apply --yes",
		},
		Destructive:  "true", // the token kind is Sensitive
		Auth:         true,
		OutputPolicy: runtime.OnceOnlyOutputPolicy(),
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeText,
	}
}

func (c createTokenCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	ttl, err := ParseTTL("ttl", in.String("ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	ttlMs, err := TTLMilliseconds("ttl", ttl, in.String("ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}

	spec := map[string]any{}
	if desc := in.String("description"); desc != "" {
		spec["description"] = desc
	}
	if ttlMs > 0 {
		spec["ttl"] = ttlMs
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
	eff, _, err := engine.PlanCreate(ctx, client, tokenKind.GVR, "", obj, tokenKind.Redaction(), engine.LocalCluster, tokenKind.Name, "Token", tokenKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, tokenKind.GVR, "", obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		name := created.GetName()
		recovery := fmt.Sprintf("delete token %s, then create again", name)
		bearer, _, _ := unstructured.NestedString(created.Object, "status", "bearerToken")
		if bearer == "" {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				"the server created the token but returned no bearerToken value").
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "Token", ResourceName: name, Recovery: recovery,
				})
		}
		expiresAt, _, _ := unstructured.NestedString(created.Object, "status", "expiresAt")
		bearerLine, shownOnce := runtime.OnceOnlyLine("bearer token", bearer, "bearer token")
		lines := []string{bearerLine}
		if expiresAt != "" {
			lines = append(lines, "expires: "+output.Printable(expiresAt))
		}
		res := runtime.MutationText("token", output.Printable(name), "created", lines...)
		res.Kind = "TokenCreated"
		res.Data = map[string]string{
			"name":        name,
			"bearerToken": bearer,
			"expiresAt":   expiresAt,
		}
		res.OnceOnlyNotice = shownOnce
		res.CommittedContext = &runtime.CommittedContext{ResourceKind: "Token", ResourceName: name, Recovery: recovery}
		return res, nil
	}
	return eff, exec, nil
}
