package projects

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
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
// secret entry). Values containing a comma can't be expressed this way —
// the VALIDATION message points at create resource -f instead.
func parseLiteralPairs(s string) (map[string]any, error) {
	return parsePairs(s, true, func(pair string) error {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("malformed --from-literal pair %q; pass key=value pairs, or for values containing a comma use `create resource -f` instead", pair))
	})
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
			{Name: "namespace", Type: "string", Desc: "`namespace` for the secret"},
			{Name: "cluster", Type: "string", Required: true, Desc: "target cluster `id|name`"},
			{Name: "project", Type: "string", Desc: "project `id|name` to scope the secret to"},
			{Name: "project-scoped", Type: "bool", Desc: "create a project-scoped secret"},
			{Name: "from-literal", Type: "string", Desc: "secret data as `key=value` pairs (comma-separated)"},
			{Name: "type", Type: "string", Desc: "secret `type` (default Opaque)"},
		},
		Examples: []string{
			"rancher create secret app-secret --namespace team-a --cluster c-x --from-literal key=v1,other=v2 --apply",
			"rancher create secret shared-cred --project p-x --project-scoped --cluster c-x --from-literal token=abc --apply",
		},
		Destructive: "true",
		Network:     true,
		Auth:        true,
		Shape:       runtime.ShapeText,
	}
}

func (c createSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required")
	}
	name := args[0]

	projectScoped := in.Bool("project-scoped")
	projectFlag := in.String("project")
	nsFlag := in.String("namespace")
	if projectScoped && projectFlag == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "--project is required with --project-scoped")
	}
	if projectScoped && nsFlag != "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"--namespace cannot be combined with --project-scoped; project-scoped secrets are written into the project's backing namespace")
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
		backingNS, statusEmpty, err := BackingNamespace(ctx, c.clients, cluster, projectID)
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
		eff, exec, err := engine.PlanCreate(ctx, client, secretKind.GVR, backingNS, obj, cluster, secretKind.Name, "Secret", secretKind.Sensitive)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		if statusEmpty {
			// status.backingNamespace was absent/empty; the fallback still
			// writes to a namespace, but propagation only works there on
			// Rancher 2.12+ (spec PC-9).
			inner := exec
			exec = func(ctx context.Context) (runtime.Result, error) {
				res, err := inner(ctx)
				if err != nil {
					return res, err
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf("project-scoped secret propagation is not guaranteed (requires Rancher >= %s, the supported minimum)", config.MinRancherVersion))
				return res, nil
			}
		}
		return eff, engine.RedactMutationResult(exec, secretKind.RedactPaths, secretKind.Sensitive), nil
	}

	if nsFlag == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"--namespace is required, or pass --project with --project-scoped for a project-scoped secret")
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
	eff, exec, err := engine.PlanCreate(ctx, client, secretKind.GVR, nsFlag, obj, cluster, secretKind.Name, "Secret", secretKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, engine.RedactMutationResult(exec, secretKind.RedactPaths, secretKind.Sensitive), nil
}
