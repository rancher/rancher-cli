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
// secret entry). Values containing a comma can't be expressed this way -
// the VALIDATION message points at create resource -f instead. The message
// never quotes the pair: it may be, or hold the second half of, a secret
// value.
func parseLiteralPairs(s string) (map[string]any, error) {
	return parsePairs(s, "from-literal", true, func(string) error {
		return clierr.New(clierr.CodeValidation,
			"malformed --from-literal value; pass key=value pairs separated by commas; a value containing a comma goes in a manifest file, <file> is that file; add --apply after reading the preview: "+clierr.Rancher+" create resource -f <file> --dry-run")
	})
}

type createSecretCmd struct {
	clients engine.ClusterClientFactory
}

// NewCreateSecret builds `create secret <name> [--from-literal k=v[,k=v...]]
// [--type <t>]` (mutating) with two routing modes, exactly one of which the
// flags select: --namespace (an ordinary namespaced Secret on the target
// cluster) and --project (a project-scoped Secret, written on the management
// cluster into the project's backing namespace - Rancher's own
// GetProjectBackingNamespace fallback semantics).
func NewCreateSecret(clients engine.ClusterClientFactory) runtime.Command {
	return &createSecretCmd{clients: clients}
}

func (createSecretCmd) Describe() runtime.Descriptor {
	namespace := engine.NamespaceFlag(false)
	namespace.Desc = "`namespace` for the secret"
	return runtime.Descriptor{
		Name:        "create secret",
		Usage:       "Create a secret, optionally project-scoped",
		CommandType: runtime.Mutating,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string", Required: true}},
		Flags: []runtime.ArgSpec{
			namespace,
			engine.ClusterFlag(true, true),
			runtime.ProjectFlag(false, "create a project-scoped secret in this project's backing namespace; Rancher copies it into every namespace of the project (not with --namespace)"),
			{Name: "from-literal", Type: "string", Once: true, Desc: "secret data as `key=value` pairs (comma-separated)"},
			{Name: "type", Type: "string", Desc: "secret `type` (default Opaque)"},
		},
		Examples: []string{
			"rancher create secret app-secret --namespace team-a --cluster c-x --from-literal key=v1,other=v2 --apply",
			"rancher create secret shared-cred --project p-x --cluster c-x --from-literal token=abc --apply",
		},
		Destructive: "true",
		Auth:        true,
		Shape:       runtime.ShapeText,
		SecretInput: true,
	}
}

func (c createSecretCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "argument <name> is required")
	}
	name := args[0]

	projectScoped := in.String(runtime.FlagProject) != ""
	nsFlag := in.String("namespace")
	switch {
	case projectScoped && nsFlag != "":
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
			"--project and --namespace cannot be combined: --project creates a project-scoped secret in the project's backing namespace, --namespace creates a secret in that namespace")
	case !projectScoped && nsFlag == "":
		ce := clierr.New(clierr.CodeValidation,
			"pass --namespace <namespace> to create a secret in a namespace, or --project <project> to create a project-scoped secret")
		if cluster := in.String(engine.FlagCluster); cluster != "" {
			ce = ce.WithHint(clierr.Rancher + " access projects --cluster " + clierr.ShellQuote(cluster))
		}
		return safety.Effect{}, nil, ce
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
		cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, true)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		// The project is resolved on the downstream cluster it belongs to;
		// the secret itself is written through the local cluster.
		p, _, err := runtime.TypedProjectOn(ctx, in, cluster)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		projectID := p.ID
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
		eff, exec, err := engine.PlanCreate(ctx, client, secretKind.GVR, backingNS, obj, secretKind.Redaction(), cluster, secretKind.Name, "Secret", secretKind.Sensitive)
		if err != nil {
			return safety.Effect{}, nil, err
		}
		if statusEmpty {
			// status.backingNamespace was absent/empty; the fallback still
			// writes to a namespace, but Rancher might not copy the secret
			// into the project's namespaces.
			inner := exec
			exec = func(ctx context.Context) (runtime.Result, error) {
				res, err := inner(ctx)
				if err != nil {
					return res, err
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf("the project has no status.backingNamespace yet; Rancher might not copy this project-scoped secret into the project's namespaces (copying needs Rancher %s or later, the oldest version this CLI supports)", config.MinRancherVersion))
				return res, nil
			}
		}
		return eff, exec, nil
	}

	cluster, err := engine.ResolveDownstreamCluster(ctx, c.clients, in, true)
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
	eff, exec, err := engine.PlanCreate(ctx, client, secretKind.GVR, nsFlag, obj, secretKind.Redaction(), cluster, secretKind.Name, "Secret", secretKind.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	return eff, exec, nil
}
