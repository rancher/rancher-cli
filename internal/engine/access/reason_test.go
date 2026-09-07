package access

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseReasonGrammar asserts both authorizer sentences parse with
// every field, the RoleBinding name and namespace split at the last "/",
// and every other text is rejected without a partial result.
func TestParseReasonGrammar(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		reason string
		want   reasonBinding
		ok     bool
	}{
		"clusterrolebinding to user": {
			reason: `RBAC: allowed by ClusterRoleBinding "crb-digweakrr4" of ClusterRole "cluster-member" to User "u-g7rbm"`,
			want:   reasonBinding{Kind: "ClusterRoleBinding", Name: "crb-digweakrr4", RoleKind: "ClusterRole", RoleName: "cluster-member", SubjectKind: "User", SubjectName: "u-g7rbm"},
			ok:     true,
		},
		"clusterrolebinding to group": {
			reason: `RBAC: allowed by ClusterRoleBinding "e2e-fid-plain-crb" of ClusterRole "view" to Group "system:cattle:authenticated"`,
			want:   reasonBinding{Kind: "ClusterRoleBinding", Name: "e2e-fid-plain-crb", RoleKind: "ClusterRole", RoleName: "view", SubjectKind: "Group", SubjectName: "system:cattle:authenticated"},
			ok:     true,
		},
		"rolebinding of clusterrole": {
			reason: `RBAC: allowed by RoleBinding "rb-5ttmtkqlvs/e2e-fid-explain" of ClusterRole "edit" to User "u-g7rbm"`,
			want:   reasonBinding{Kind: "RoleBinding", Name: "rb-5ttmtkqlvs", Namespace: "e2e-fid-explain", RoleKind: "ClusterRole", RoleName: "edit", SubjectKind: "User", SubjectName: "u-g7rbm"},
			ok:     true,
		},
		"rolebinding of role": {
			reason: `RBAC: allowed by RoleBinding "e2e-fid-plain-rb/e2e-fid-explain" of Role "e2e-fid-plain-role" to User "u-dxb9w"`,
			want:   reasonBinding{Kind: "RoleBinding", Name: "e2e-fid-plain-rb", Namespace: "e2e-fid-explain", RoleKind: "Role", RoleName: "e2e-fid-plain-role", SubjectKind: "User", SubjectName: "u-dxb9w"},
			ok:     true,
		},
		"rolebinding to service account with namespace": {
			reason: `RBAC: allowed by RoleBinding "rb-1/team-a" of Role "reader" to ServiceAccount "team-a/ci"`,
			want:   reasonBinding{Kind: "RoleBinding", Name: "rb-1", Namespace: "team-a", RoleKind: "Role", RoleName: "reader", SubjectKind: "ServiceAccount", SubjectName: "team-a/ci"},
			ok:     true,
		},
		"empty":                  {reason: ""},
		"denied has no reason":   {reason: "no RBAC policy matched"},
		"unknown binding kind":   {reason: `RBAC: allowed by Binding "x" of ClusterRole "y" to User "z"`},
		"rolebinding without ns": {reason: `RBAC: allowed by RoleBinding "rb-1" of Role "reader" to User "u"`},
		"missing quote":          {reason: `RBAC: allowed by ClusterRoleBinding "crb of ClusterRole "y" to User "z"`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseReason(tc.reason)
			require.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}
