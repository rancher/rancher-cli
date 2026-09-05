package engine

import "strings"

// IsSystemPrincipal reports whether a principal id is in Rancher's system
// space - the "system:" prefix, which also covers the "system://" ids
// Rancher stamps on provisioning-created bindings. Mirrors Rancher's rbac
// IsSystem rule; the single predicate shared by the rbac grant/revoke
// rejection and inspect's member system badge.
func IsSystemPrincipal(id string) bool {
	return strings.HasPrefix(id, "system:")
}
