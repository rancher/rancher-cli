package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestAliasRevokeHintPreviewsTheRevoke asserts the preview of the revoke of a
// binding that holds another principal of the same user: the flags follow the
// name, and a name that starts with a dash follows them and "--".
func TestAliasRevokeHintPreviewsTheRevoke(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		scope Scope
		flags string
	}{
		{"project", Scope{Kind: ScopeProject, ProjectID: "p-1", Cluster: "c-abc"}, " --project 'p-1' --cluster 'c-abc'"},
		{"cluster", Scope{Kind: ScopeCluster, Cluster: "c-abc"}, " --cluster 'c-abc'"},
		{"global", Scope{Kind: ScopeGlobal}, " --global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.flags, tc.scope.Flags())
			assert.Equal(t, clierr.Rancher+" revoke 'u-abc12' --role 'project-member'"+tc.flags+" --dry-run",
				aliasRevokeHint("u-abc12", "project-member", tc.scope.Flags()))
			assert.Equal(t, clierr.Rancher+" revoke --role 'project-member'"+tc.flags+" --dry-run -- '-x'",
				aliasRevokeHint("-x", "project-member", tc.scope.Flags()))
		})
	}
}
