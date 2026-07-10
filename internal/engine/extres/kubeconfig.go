package extres

import (
	"context"
	"fmt"
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
// the local, exec-credential `kubeconfig` — this one mints a server-side,
// embedded, revocable kubeconfig via the ext Kubeconfig store. The store
// ignores metadata.name/generateName exactly like Token, so neither is set.
func NewCreateKubeconfig(clients engine.ClusterClientFactory) runtime.Command {
	return &createKubeconfigCmd{clients: clients}
}

func (createKubeconfigCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create kubeconfig",
		Usage:       "Mint a server-side, embedded kubeconfig",
		CommandType: runtime.Mutating,
		Flags: []runtime.ArgSpec{
			{Name: "clusters", Type: "string"},
			{Name: "current-context", Type: "string"},
			{Name: "ttl", Type: "string"},
			{Name: "description", Type: "string"},
			// A bool flag cannot distinguish omitted from false; the server
			// field is a *bool defaulting true, so tri-state is a string enum.
			{Name: "include-default-entry", Type: "string", Enum: []string{"true", "false"}},
		},
		Examples: []string{
			"rancher create kubeconfig --clusters prod,staging --ttl 30d --apply > ci.yaml",
		},
		Destructive: "true", // the kubeconfig kind is Sensitive
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

// resolveClusterToken resolves one --clusters/--current-context entry: the
// literal passthroughs "*" (all accessible) and "local" are never sent
// through ResolveCluster, since neither is a real cluster name/id.
func resolveClusterToken(ctx context.Context, clients engine.ClusterClientFactory, tok string) (string, error) {
	switch tok {
	case "*", "local":
		return tok, nil
	default:
		return clients.ResolveCluster(ctx, tok)
	}
}

func (c createKubeconfigCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	var clusterList []string
	hasWildcard := false
	if raw := in.String("clusters"); raw != "" {
		for _, tok := range strings.Split(raw, ",") {
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
		if !hasWildcard && !containsString(clusterList, resolved) {
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("--current-context %q must name one of --clusters", raw)).
				WithHint("pass --current-context as one of the values given to --clusters")
		}
		currentContext = resolved
	}

	includeDefaultEntry := in.String("include-default-entry")
	if includeDefaultEntry == "false" && len(clusterList) == 0 {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"--include-default-entry=false requires --clusters").
			WithHint("pass --clusters, or omit --include-default-entry to keep the default entry")
	}

	ttl, err := ParseTTL(in.String("ttl"))
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
	if ttl > 0 {
		_ = unstructured.SetNestedField(obj.Object, int64(ttl.Seconds()), "spec", "ttl")
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
	eff, _, err := engine.PlanCreate(ctx, client, kubeconfigKind.GVR, "", obj, engine.LocalCluster, "Kubeconfig")
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// Creating a Sensitive kind is destructive and must gate on --yes exactly
	// as patching one does (secret.go:141/184, resources/create.go:122) — the
	// shared PlanCreate has no notion of Sensitive, so the caller sets it.
	eff.Destructive = kubeconfigKind.Sensitive

	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, kubeconfigKind.GVR, "", obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		name := created.GetName()
		value, _, _ := unstructured.NestedString(created.Object, "status", "value")
		header := fmt.Sprintf("# ext kubeconfig %q — shown exactly once; revoke with: rancher delete kubeconfig %s\n", name, name)
		return runtime.Result{RawBytes: append([]byte(header), []byte(value)...)}, nil
	}
	return eff, exec, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
