package access

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// ScopeMode says how the question's namespaces were chosen.
type ScopeMode int

const (
	scopeCluster ScopeMode = iota
	scopeNamespace
	scopeProject
	scopeAllNamespaces
)

// Scope is the resolved target of a question: the wire scope plus the
// unexported fan-out targets (one for -n, the project's members for --project,
// every listed namespace for --all-namespaces; nil for cluster scope).
type Scope struct {
	Cluster       string   `json:"cluster"`
	Namespace     string   `json:"namespace,omitempty"`
	Project       string   `json:"project,omitempty"`
	Namespaces    []string `json:"namespaces,omitempty"`
	AllNamespaces bool     `json:"allNamespaces,omitempty"`
	targets       []string
	mode          ScopeMode
}

type scopeFlags struct {
	namespace     string
	project       string
	allNamespaces bool
	allowAll      bool
}

func (f scopeFlags) selectors() int {
	n := 0
	for _, set := range []bool{f.namespace != "", f.project != "", f.allNamespaces} {
		if set {
			n++
		}
	}
	return n
}

// validateSelectors checks what needs no server: at most one selector,
// and --all-namespaces only where the command accepts it.
func validateSelectors(f scopeFlags) error {
	if f.selectors() > 1 {
		return clierr.New(clierr.CodeValidation, "pass at most one of --namespace, --project, --all-namespaces")
	}
	if f.allNamespaces && !f.allowAll {
		return clierr.New(clierr.CodeValidation, "--all-namespaces is not accepted here; ask one namespace with -n or a project with --project")
	}
	return nil
}

// rejectSelectors refuses a namespace selector on a cluster-scoped
// resource. Pure: it runs right after discovery, before any lookup.
func (r ResourceRef) rejectSelectors(f scopeFlags) error {
	if !r.Namespaced && f.selectors() > 0 {
		return clierr.New(clierr.CodeValidation,
			fmt.Sprintf("resource %s is cluster-scoped; omit --namespace, --project, and --all-namespaces", r.Resource))
	}
	return nil
}

// typedProject reads the project a typed --project names on clusterID; the
// zero Project when none was typed.
func typedProject(ctx context.Context, in runtime.Input, clusterID string) (runtime.Project, error) {
	p, _, err := runtime.TypedProjectOn(ctx, in, clusterID)
	return p, err
}

// resolveScope turns the validated selector into targets on the already
// resolved cluster. It existence-checks only a user-supplied namespace;
// names returned by a successful LIST are targets as listed. p is the
// project typedProject read, the zero Project when none was typed.
func resolveScope(ctx context.Context, client engine.ClusterClient, clusterID string, f scopeFlags, p runtime.Project) (Scope, error) {
	sc := Scope{Cluster: clusterID}
	nsKind, _ := kinds.Lookup("namespace")
	switch {
	case f.namespace != "":
		if _, err := client.Get(ctx, nsKind.GVR, "", f.namespace); err != nil {
			if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeNotFound {
				return Scope{}, clierr.New(clierr.CodeValidation,
					fmt.Sprintf("namespace %s not found on cluster %s", f.namespace, clusterID))
			}
			return Scope{}, err
		}
		sc.mode, sc.Namespace, sc.targets = scopeNamespace, f.namespace, []string{f.namespace}
	case p.ID != "":
		members, err := p.Namespaces(ctx)
		if err != nil {
			return Scope{}, err
		}
		sc.mode, sc.Project = scopeProject, p.ID
		sc.targets = make([]string, 0, len(members))
		for n := range members {
			sc.targets = append(sc.targets, n)
		}
		slices.Sort(sc.targets)
		if len(sc.targets) > 0 {
			sc.Namespaces = slices.Clone(sc.targets)
		}
	case f.allNamespaces:
		list, err := client.List(ctx, nsKind.GVR, "", "")
		if err != nil {
			return Scope{}, err
		}
		sc.mode, sc.AllNamespaces = scopeAllNamespaces, true
		for i := range list.Items {
			sc.targets = append(sc.targets, list.Items[i].GetName())
		}
		slices.Sort(sc.targets)
	default:
		sc.mode = scopeCluster
	}
	return sc, nil
}
