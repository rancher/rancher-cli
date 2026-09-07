package access

import (
	"regexp"
	"strings"
)

const (
	bindingKindClusterRoleBinding = "ClusterRoleBinding"
	bindingKindRoleBinding        = "RoleBinding"
)

// reasonBinding is the one binding the RBAC authorizer named in an allowed
// answer's reason, field by field. Namespace is set for a RoleBinding
// only. SubjectName is the quoted subject text verbatim (a service account
// reads "<namespace>/<name>").
type reasonBinding struct {
	Kind, Name, Namespace    string
	RoleKind, RoleName       string
	SubjectKind, SubjectName string
}

// reasonRE is the RBAC authorizer's sentence for an allowed answer:
// `RBAC: allowed by <BindingKind> "<name>" of <RoleKind> "<role>" to
// <SubjectKind> "<subject>"`, where a RoleBinding's quoted name is
// "<name>/<namespace>", name first. Quoted values never contain a double
// quote (Kubernetes names and RBAC subjects cannot), so the quoted
// captures are greedy up to the next quote.
var reasonRE = regexp.MustCompile(`^RBAC: allowed by (ClusterRoleBinding|RoleBinding) "([^"]+)" of (ClusterRole|Role) "([^"]+)" to (User|Group|ServiceAccount) "([^"]+)"$`)

// parseReason parses an allowed answer's reason against the authorizer's
// sentence. ok is false for any other text, including a denied answer's
// empty reason; the zero value is returned then so a caller never reads a
// half-parsed binding.
func parseReason(reason string) (reasonBinding, bool) {
	m := reasonRE.FindStringSubmatch(reason)
	if m == nil {
		return reasonBinding{}, false
	}
	b := reasonBinding{Kind: m[1], Name: m[2], RoleKind: m[3], RoleName: m[4], SubjectKind: m[5], SubjectName: m[6]}
	if b.Kind == bindingKindRoleBinding {
		name, ns, found := strings.Cut(m[2], "/")
		if !found || name == "" || ns == "" {
			return reasonBinding{}, false
		}
		b.Name, b.Namespace = name, ns
	}
	return b, true
}
