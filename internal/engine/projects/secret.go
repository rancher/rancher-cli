package projects

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

// projectScopedSecretLabel marks a secret Rancher propagates into every
// namespace of the project named by its value (bare short project id).
const projectScopedSecretLabel = "management.cattle.io/project-scoped-secret"

// parseLiteralPairs parses a comma-separated k=v list for --from-literal.
// Unlike quota pairs, an empty value is valid (e.g. an intentionally blank
// secret entry); only a missing "=" or empty key is malformed. Values
// containing a comma can't be expressed this way — the VALIDATION hint
// points at create resource -f instead.
func parseLiteralPairs(s string) (map[string]any, error) {
	data := map[string]any{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, clierr.New(clierr.CodeValidation, fmt.Sprintf("malformed --from-literal pair %q", pair)).
				WithHint("pass key=value pairs; for values containing a comma use `create resource -f` instead")
		}
		data[k] = v
	}
	return data, nil
}

type createSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateSecret builds `create secret <name> [--from-literal k=v[,k=v...]]
// [--type <t>]` (mutating) with two routing modes: bare (--namespace
// required, ordinary namespaced Secret on the target cluster) and
// project-scoped (--project + --project-scoped, written on the management
// cluster into the project's backing namespace — Rancher's own
// GetProjectBackingNamespace fallback semantics, spec PC-9).
func NewCreateSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &createSecretCmd{clients: clients}
}

func (createSecretCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "create secret",
		Usage:       "Create a secret, optionally project-scoped",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			{Name: "namespace", Type: "string"},
			{Name: "cluster", Type: "string"},
			{Name: "project", Type: "string"},
			{Name: "project-scoped", Type: "bool"},
			{Name: "from-literal", Type: "string"},
			{Name: "type", Type: "string"},
		},
		Examples: []string{
			"rancher create secret app-secret --namespace team-a --cluster c-x --from-literal key=v1,other=v2 --apply",
			"rancher create secret shared-cred --project p-x --project-scoped --cluster c-x --from-literal token=abc --apply",
		},
		Destructive: "false",
		Toolset:     "core",
		Network:     true,
		Auth:        true,
	}
}

func (c createSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required").WithHint("provide <name>")
	}
	name := args[0]

	projectScoped := in.Bool("project-scoped")
	projectFlag := in.String("project")
	nsFlag := in.String("namespace")
	if projectScoped && projectFlag == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--project is required with --project-scoped").
			WithHint("pass --project <id|name>")
	}
	if projectScoped && nsFlag != "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--namespace cannot be combined with --project-scoped").
			WithHint("project-scoped secrets are written into the project's backing namespace")
	}

	var stringData map[string]any
	if literal := in.String("from-literal"); literal != "" {
		var err error
		stringData, err = parseLiteralPairs(literal)
		if err != nil {
			return safety.Effect{}, nil, err
		}
	}
	secretType := in.String("type")
	if secretType == "" {
		secretType = "Opaque"
	}

	secretKind, _ := kinds.Lookup("secret")

	if projectScoped {
		cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		projectID, err := ResolveProject(ctx, c.clients, cluster, projectFlag)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		backingNS, err := BackingNamespace(ctx, c.clients, cluster, projectID)
		if err != nil {
			return safety.Effect{}, nil, err
		}

		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      name,
				"namespace": backingNS,
				"labels":    map[string]any{projectScopedSecretLabel: projectID},
			},
			"type": secretType,
		}}
		if stringData != nil {
			_ = unstructured.SetNestedMap(obj.Object, stringData, "stringData")
		}

		client, err := c.clients.Cluster(ctx, engine.LocalCluster)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		eff, exec, err := engine.PlanCreate(ctx, client, secretKind.GVR, backingNS, obj, cluster, "Secret")
		if err != nil {
			return safety.Effect{}, nil, err
		}
		if backingNS == projectID {
			// status.backingNamespace was absent/empty; the fallback still
			// writes to a namespace, but propagation only works there on
			// Rancher 2.12+ (spec PC-9).
			inner := exec
			exec = func(ctx context.Context) (runtime.Result, error) {
				res, err := inner(ctx)
				if err != nil {
					return res, err
				}
				res.Warnings = append(res.Warnings, "if this Rancher is older than 2.12, project-scoped secrets are not propagated")
				return res, nil
			}
		}
		return eff, exec, nil
	}

	if nsFlag == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--namespace is required").
			WithHint("pass --namespace <namespace>, or --project/--project-scoped for a project-scoped secret")
	}
	cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": name, "namespace": nsFlag},
		"type":       secretType,
	}}
	if stringData != nil {
		_ = unstructured.SetNestedMap(obj.Object, stringData, "stringData")
	}
	client, err := c.clients.Cluster(ctx, cluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return engine.PlanCreate(ctx, client, secretKind.GVR, nsFlag, obj, cluster, "Secret")
}
