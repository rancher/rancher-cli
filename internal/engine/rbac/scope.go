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
// Exactly one scope must be selected; --project names a project on the
// cluster --cluster names, and without --cluster it is refused naming the
// clusters the project is on. Cluster scope requires the EXPLICIT --cluster
// flag, so an unscoped invocation errors instead of silently granting
// cluster-wide.
func ResolveScope(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) (Scope, error) {
	global := in.Bool("global")
	project := in.String(runtime.FlagProject)
	clusterFlag := in.String(engine.FlagCluster)

	if global && (project != "" || clusterFlag != "") {
		return Scope{}, clierr.New(clierr.CodeValidation,
			"--global cannot be combined with --project or --cluster; pick exactly one scope")
	}
	if global {
		return Scope{Kind: ScopeGlobal, KindKey: "globalrolebinding", RoleField: "globalRoleName"}, nil
	}
	if project != "" {
		cluster, err := engine.ResolveDownstreamCluster(ctx, clients, in, false)
		if err != nil {
			return Scope{}, err
		}
		p, _, err := runtime.TypedProjectOn(ctx, in, cluster)
		if err != nil {
			return Scope{}, err
		}
		projectID := p.ID
		ns, _, err := projects.BackingNamespace(ctx, clients, cluster, projectID)
		if err != nil {
			return Scope{}, err
		}
		return Scope{Kind: ScopeProject, Cluster: cluster, ProjectID: projectID, NS: ns,
			KindKey: "projectroletemplatebinding", RoleField: "roleTemplateName"}, nil
	}
	if clusterFlag != "" {
		cluster, err := engine.ResolveDownstreamCluster(ctx, clients, in, false)
		if err != nil {
			return Scope{}, err
		}
		return Scope{Kind: ScopeCluster, Cluster: cluster, NS: cluster,
			KindKey: "clusterroletemplatebinding", RoleField: "roleTemplateName"}, nil
	}
	return Scope{}, clierr.New(clierr.CodeValidation,
		"a scope is required: --project with --cluster, --cluster, or --global")
}

// Flags is the --project/--cluster/--global flags an equivalent grant or
// revoke invocation needs, for a hint that names a complete paste-valid
// command.
func (s Scope) Flags() clierr.CommandOption {
	return func(c clierr.Command) clierr.Command {
		switch s.Kind {
		case ScopeProject:
			return c.FlagValue("--project", s.ProjectID).FlagValue("--cluster", s.Cluster)
		case ScopeCluster:
			return c.FlagValue("--cluster", s.Cluster)
		default: // ScopeGlobal
			return c.Flag("--global")
		}
	}
}
