package rbac

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
)

type ScopeKind string

const (
	ScopeProject ScopeKind = "project"
	ScopeCluster ScopeKind = "cluster"
	ScopeGlobal  ScopeKind = "global"
)

// Scope is the resolved binding target: which kind, which namespace, and
// which role field grant/revoke read and write.
type Scope struct {
	Kind      ScopeKind
	Cluster   string
	ProjectID string
	NS        string
	KindKey   string
	RoleField string
}

// ResolveScope maps the --project/--cluster/--global flags to a Scope.
// Exactly one scope must be selected; --project may be qualified by
// --cluster (or the context cluster). Cluster scope requires the EXPLICIT
// --cluster flag — the context-cluster fallback applies only inside
// project resolution — so an unscoped invocation errors instead of
// silently granting cluster-wide.
func ResolveScope(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) (Scope, error) {
	global := in.Bool("global")
	project := in.String("project")
	clusterFlag := in.String(engine.FlagCluster)

	if global && (project != "" || clusterFlag != "") {
		return Scope{}, clierr.New(clierr.CodeValidation,
			"--global cannot be combined with --project or --cluster").
			WithHint("pick exactly one scope")
	}
	if global {
		return Scope{Kind: ScopeGlobal, KindKey: "globalrolebinding", RoleField: "globalRoleName"}, nil
	}
	if project != "" {
		cluster, err := engine.ResolveDownstreamCluster(ctx, clients, in)
		if err != nil {
			return Scope{}, err
		}
		projectID, err := projects.ResolveProject(ctx, clients, cluster, project)
		if err != nil {
			return Scope{}, err
		}
		ns, _, err := projects.BackingNamespace(ctx, clients, cluster, projectID)
		if err != nil {
			return Scope{}, err
		}
		return Scope{Kind: ScopeProject, Cluster: cluster, ProjectID: projectID, NS: ns,
			KindKey: "projectroletemplatebinding", RoleField: "roleTemplateName"}, nil
	}
	if clusterFlag != "" {
		cluster, err := engine.ResolveDownstreamCluster(ctx, clients, in)
		if err != nil {
			return Scope{}, err
		}
		return Scope{Kind: ScopeCluster, Cluster: cluster, NS: cluster,
			KindKey: "clusterroletemplatebinding", RoleField: "roleTemplateName"}, nil
	}
	return Scope{}, clierr.New(clierr.CodeValidation,
		"a scope is required: --project, --cluster, or --global").
		WithHint("e.g. --project Default --cluster c-x, or --cluster c-x, or --global")
}
