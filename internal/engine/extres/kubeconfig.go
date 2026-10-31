package extres

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type createKubeconfigCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateKubeconfig builds `create kubeconfig [--clusters a,b|*|local]
// [--current-context <c>] [--ttl <dur>] [--description <d>]
// [--include-default-entry true|false]` (mutating): a SEPARATE command from
// the local, exec-credential `kubeconfig` - this one creates a server-side,
// embedded, revocable kubeconfig via the ext Kubeconfig store. The store
// ignores metadata.name/generateName exactly like Token, so neither is set.
func NewCreateKubeconfig(clients engine.ClusterClientFactory) runtime.Command {
	return &createKubeconfigCmd{clients: clients}
}

func (createKubeconfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create kubeconfig",
		Usage:       "Create a kubeconfig with an embedded token",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: "clusters", Type: "string", Desc: "comma-separated cluster `id|name` list, or `*` for all accessible"},
			{Name: "current-context", Type: "string", Desc: "cluster to use as the kubeconfig's current context"},
			{Name: "ttl", Type: "string", Desc: "expiration of the tokens in the kubeconfig, e.g. 30d, 12h (omit for server default)"},
			{Name: "description", Type: "string", Desc: "description of the kubeconfig, to identify it"},
			// A bool flag cannot distinguish omitted from false; the server
			// field is a *bool defaulting true, so tri-state is a string enum.
			{Name: "include-default-entry", Type: "string", Enum: []string{"true", "false"}, Desc: "include the Rancher server's default entry (server default true; pass false, with --clusters, to leave it out)"},
		},
		Examples: []string{
			"rancher create kubeconfig --clusters prod,staging --ttl 30d --apply > ci.yaml",
		},
		Destructive: "true", // the kubeconfig kind is Sensitive
		Auth:        true,
		// RawBytes writes verbatim regardless of -o (renderResult never
		// looks at the format for a RawBytes payload), so every format
		// other than the bare table default would silently accept and then
		// ignore whatever the operator asked for. OutputPolicy accepts only
		// "table" - the one format this command actually honors - and
		// rejects everything else, including "name", pre-Plan.
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table"}},
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeBytes,
	}
}

// resolveClusterToken resolves one --clusters/--current-context entry
// through engine.ResolveClusterArgument, so an id-wins warning names no
// flag; the literal passthroughs "*" (all accessible) and "local" are
// never resolved, since neither is a real cluster name/id.
func resolveClusterToken(ctx context.Context, clients engine.ClusterClientFactory, tok string) (string, error) {
	switch tok {
	case "*", "local":
		return tok, nil
	default:
		return engine.ResolveClusterArgument(ctx, clients, tok)
	}
}

func (c createKubeconfigCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	var clusterList []string
	hasWildcard := false
	if raw := in.String("clusters"); raw != "" {
		for tok := range strings.SplitSeq(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			resolved, err := resolveClusterToken(ctx, c.clients, tok)
			if err != nil {
				return safety.Effect{}, nil, err
			}
			if resolved == "*" {
				hasWildcard = true
			}
			clusterList = append(clusterList, resolved)
		}
	}

	currentContext := ""
	if raw := in.String("current-context"); raw != "" {
		resolved, err := resolveClusterToken(ctx, c.clients, raw)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		if !hasWildcard && !slices.Contains(clusterList, resolved) {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--current-context %q must be one of the clusters listed in --clusters", raw))
		}
		currentContext = resolved
	}

	includeDefaultEntry := in.String("include-default-entry")
	if includeDefaultEntry == "false" && len(clusterList) == 0 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"--include-default-entry=false requires --clusters, or omit --include-default-entry to keep the default entry")
	}

	ttl, err := ParseTTL("ttl", in.String("ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	ttlSecs, err := TTLSeconds("ttl", ttl, in.String("ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Kubeconfig",
		"metadata":   map[string]any{},
		"spec":       map[string]any{},
	}}
	if len(clusterList) > 0 {
		_ = unstructured.SetNestedStringSlice(obj.Object, clusterList, "spec", "clusters")
	}
	if currentContext != "" {
		_ = unstructured.SetNestedField(obj.Object, currentContext, "spec", "currentContext")
	}
	if desc := in.String("description"); desc != "" {
		_ = unstructured.SetNestedField(obj.Object, desc, "spec", "description")
	}
	if ttlSecs > 0 {
		_ = unstructured.SetNestedField(obj.Object, ttlSecs, "spec", "ttl")
	}
	switch includeDefaultEntry {
	case "true":
		_ = unstructured.SetNestedField(obj.Object, true, "spec", "includeDefaultEntry")
	case "false":
		_ = unstructured.SetNestedField(obj.Object, false, "spec", "includeDefaultEntry")
	}

	kubeconfigKind, _ := kinds.Lookup("kubeconfig")
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff, _, err := engine.PlanCreate(ctx, client, kubeconfigKind.GVR, "", obj, kubeconfigKind.Redaction(), engine.LocalCluster, kubeconfigKind.Name, "Kubeconfig", kubeconfigKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, refuseUnsupportedTokenLookup(ctx, c.clients, err)
	}

	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, kubeconfigKind.GVR, "", obj, false)
		if err != nil {
			return runtime.Result{}, refuseUnsupportedTokenLookup(ctx, c.clients, err)
		}
		name := created.GetName()
		recovery := fmt.Sprintf("delete kubeconfig %s, then create again", name)
		value, found, _ := unstructured.NestedString(created.Object, "status", "value")
		if !found || value == "" {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric,
				"the server created the kubeconfig but returned no content").
				WithDetails(clierr.CommittedContextDetails{
					Committed: true, ResourceKind: "Kubeconfig", ResourceName: name, Recovery: recovery,
				})
		}
		header := fmt.Sprintf("# Kubeconfig %q - shown once; revoke with: rancher delete kubeconfig %s\n", name, name)
		return runtime.Result{
			RawBytes:         append([]byte(header), []byte(value)...),
			CommittedContext: &runtime.CommittedContext{ResourceKind: "Kubeconfig", ResourceName: name, Recovery: recovery},
		}, nil
	}
	return eff, exec, nil
}
