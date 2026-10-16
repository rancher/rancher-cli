package access

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
	// takesProject says whether the command declares --project.
	takesProject bool
}

// selectorNames lists the selector flags the command declares, for the
// refusals that name them.
func (f scopeFlags) selectorNames() []string {
	names := []string{"--namespace"}
	if f.takesProject {
		names = append(names, "--project")
	}
	if f.allowAll {
		names = append(names, "--all-namespaces")
	}
	return names
}

// joinFlagNames writes names as the refusals list them: "a", "a and b",
// "a, b, and c".
func joinFlagNames(names []string) string {
	switch len(names) {
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
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
		return clierr.New(clierr.CodeValidation, "pass at most one of "+strings.Join(f.selectorNames(), ", "))
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
			fmt.Sprintf("resource %s is cluster-scoped; omit %s", r.Resource, joinFlagNames(f.selectorNames())))
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
// resolved cluster. It existence-checks only a user-supplied namespace; a
// missing one is the GET's NOT_FOUND, as in the commands that read the
// namespace or a named object in it. Names returned by a successful LIST
// are targets as listed. p is the project typedProject read, the zero
// Project when none was typed.
func resolveScope(ctx context.Context, client engine.ClusterClient, clusterID string, f scopeFlags, p runtime.Project) (Scope, error) {
	sc := Scope{Cluster: clusterID}
	nsKind, _ := kinds.Lookup("namespace")
	switch {
	case f.namespace != "":
		if _, err := client.Get(ctx, nsKind.GVR, "", f.namespace); err != nil {
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

// namespaceFlag is the --namespace of an access command, with the command's
// own description.
func namespaceFlag(desc string) runtime.ArgSpec {
	f := engine.NamespaceFlag(false)
	f.Desc = desc
	return f
}

// clusterFlag is the required --cluster of an access command, which takes
// --namespace, with the command's own description.
func clusterFlag(desc string) runtime.ArgSpec {
	f := engine.ClusterFlag(true, true)
	f.Desc = desc
	return f
}
