package access

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// The Kubernetes RBAC bindings who-can reads its candidates from. They are
// not kind-table rows: who-can reads them as a candidate source, never as
// an operator-facing resource.
var (
	roleBindingGVR        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	clusterRoleBindingGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
)

// mastersGroup is included independently of bindings: the apiserver
// authorizes it before RBAC is consulted, so it is asked about on every
// question whether or not a binding names it.
const mastersGroup = "system:masters"

// candidate is one distinct binding subject, kept byte for byte. Name is
// what Kubernetes sees: the username, the group name, or
// system:serviceaccount:<namespace>:<name> for a ServiceAccount subject.
type candidate struct {
	Kind  SubjectKind
	Name  string
	Fixed bool // the system:masters row, not read from a binding
}

// candidateKey orders and deduplicates candidates by kind, then name. The
// separator cannot occur in a Kubernetes name.
func candidateKey(c candidate) string { return string(c.Kind) + "\x00" + c.Name }

// candidateSourceFor is the candidateSource every result states.
func candidateSourceFor(clusterID string) string {
	return fmt.Sprintf("rolebindings and clusterrolebindings on %s (+ %s)", clusterID, mastersGroup)
}

// listCandidates reads the cluster's ClusterRoleBindings and the
// RoleBindings the scope reaches with the caller's client and returns the
// distinct subjects plus the fixed system:masters row (fixed even when a
// binding names that group as well), sorted by kind then name. A
// namespace scope lists RoleBindings in that namespace; a project
// scope lists them cluster-wide and keeps the project's members; every
// other scope keeps them all. Subjects of a kind outside User, Group, and
// ServiceAccount are skipped and counted in one warning per kind. A
// forbidden LIST is a refusal naming the two rights who-can needs; any
// other failure keeps its mapped error.
func listCandidates(ctx context.Context, client engine.ClusterClient, scope Scope) ([]candidate, []string, error) {
	crbs, err := client.List(ctx, clusterRoleBindingGVR, "", "")
	if err != nil {
		return nil, nil, candidateListError(err, scope.Cluster)
	}
	rbNamespace := ""
	if scope.mode == scopeNamespace {
		rbNamespace = scope.Namespace
	}
	rbs, err := client.List(ctx, roleBindingGVR, rbNamespace, "")
	if err != nil {
		return nil, nil, candidateListError(err, scope.Cluster)
	}
	seen := map[string]bool{}
	skipped := map[string]int{}
	var out []candidate
	add := func(c candidate) {
		k := candidateKey(c)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, c)
	}
	// The fixed row goes in first: Kubernetes' own cluster-admin binding
	// names system:masters too, and the first candidate for a key wins, so
	// the row keeps Fixed and its note whatever the bindings say.
	add(candidate{Kind: KindGroup, Name: mastersGroup, Fixed: true})
	collect := func(list *unstructured.UnstructuredList, keep func(namespace string) bool) {
		for i := range list.Items {
			b := &list.Items[i]
			if !keep(b.GetNamespace()) {
				continue
			}
			subjects, _, _ := unstructured.NestedSlice(b.Object, "subjects")
			for _, s := range subjects {
				m, ok := s.(map[string]any)
				if !ok {
					continue
				}
				kind, _, _ := unstructured.NestedString(m, "kind")
				name, _, _ := unstructured.NestedString(m, "name")
				namespace, _, _ := unstructured.NestedString(m, "namespace")
				c, ok := candidateFromSubject(kind, name, namespace)
				if !ok {
					skipped[kind]++
					continue
				}
				add(c)
			}
		}
	}
	collect(crbs, func(string) bool { return true })
	collect(rbs, func(namespace string) bool {
		return scope.mode != scopeProject || slices.Contains(scope.targets, namespace)
	})
	slices.SortFunc(out, func(a, b candidate) int { return strings.Compare(candidateKey(a), candidateKey(b)) })
	var warnings []string
	for _, kind := range slices.Sorted(maps.Keys(skipped)) {
		warnings = append(warnings, fmt.Sprintf("%d binding subject(s) of kind %q skipped: not a User, Group, or ServiceAccount", skipped[kind], kind))
	}
	return out, warnings, nil
}

// candidateFromSubject converts one RBAC subject tuple to the output kind
// set, the only place the title-case kinds are read. Names are kept byte
// for byte; a ServiceAccount becomes the username Kubernetes authenticates
// it as. A subject without a name, or a service account without a
// namespace, is not a candidate.
func candidateFromSubject(kind, name, namespace string) (candidate, bool) {
	switch kind {
	case "User":
		return candidate{Kind: KindUser, Name: name}, name != ""
	case "Group":
		return candidate{Kind: KindGroup, Name: name}, name != ""
	case "ServiceAccount":
		if name == "" || namespace == "" {
			return candidate{}, false
		}
		return candidate{Kind: KindServiceAccount, Name: serviceAccountPrefix + namespace + ":" + name}, true
	default:
		return candidate{}, false
	}
}

// candidateListError maps a failed binding LIST: forbidden names the two
// rights who-can needs, everything else keeps its mapped error.
func candidateListError(err error, clusterID string) error {
	if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeRBAC {
		return clierr.New(clierr.CodeRBAC, fmt.Sprintf("who-can needs list on rolebindings and clusterrolebindings on cluster %s", clusterID))
	}
	return err
}
