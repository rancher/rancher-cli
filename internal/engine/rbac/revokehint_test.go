package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// unprintableValues are values no shell reads as written: one for each kind of
// refusal.
var unprintableValues = map[string]string{
	"apostrophe":       "x'y",
	"right quote":      "x\u2019y",
	"zero width":       "x\u200by",
	"double backslash": "x\\\\y",
}

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
			assert.Equal(t, clierr.Rancher+" revoke 'u-abc12' --role 'project-member'"+tc.flags+" --dry-run",
				aliasRevokeHint("u-abc12", "project-member", tc.scope).String())
			assert.Equal(t, clierr.Rancher+" revoke --role 'project-member'"+tc.flags+" --dry-run -- '-x'",
				aliasRevokeHint("-x", "project-member", tc.scope).String())
		})
	}
}

// TestGrantAliasConflictLeavesOutTheRevokePreviewForAnUnprintablePrincipal
// asserts the conflict with a binding that holds another principal of the same
// user hints the revoke preview and says so, and with a principal no shell
// reads as written leaves the command and the sentence about it out, still
// names the principal and carries the note.
func TestGrantAliasConflictLeavesOutTheRevokePreviewForAnUnprintablePrincipal(t *testing.T) {
	t.Parallel()
	conflict := func(t *testing.T, upn string) *clierr.Error {
		t.Helper()
		client := &fakeClient{
			users: []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc", upn)},
			gets: map[string]*unstructured.Unstructured{
				"c-x/p-x":         projectGetObj("p-x"),
				"/project-member": roleGet("project-member"),
			},
			listsByGVR: map[string]*unstructured.UnstructuredList{
				"projectroletemplatebindings": {Items: []unstructured.Unstructured{{Object: prtbObj("pm-1", "", upn, "", "project-member")}}},
			},
		}
		_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), stubInput{
			strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
			args:    []string{"u-abc"},
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeConflict, ce.Code)
		return ce
	}
	ce := conflict(t, "okta_user://alice")
	assert.Equal(t, clierr.Rancher+" revoke 'okta_user://alice' --role 'project-member' --project 'p-x' --cluster 'c-x' --dry-run", ce.Hint)
	assert.Contains(t, ce.Message, "the hint previews the revoke")
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
	for name, v := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			upn := "okta_user://" + v
			ce := conflict(t, upn)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, "another principal of the same user")
			assert.NotContains(t, ce.Message, "the hint previews")
			assert.NotContains(t, ce.Message, "nothing to grant")
			assert.Contains(t, ce.Message, clierr.LeftOutNote)
		})
	}
}

// TestRevokeZeroMatchesLeavesOutTheGetCommandForAnUnprintableScope asserts a
// revoke that finds no binding hints the get command of the binding kind, and
// with a project id no shell reads as written leaves the command out and
// carries the note.
func TestRevokeZeroMatchesLeavesOutTheGetCommandForAnUnprintableScope(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, project string) *clierr.Error {
		t.Helper()
		client := &fakeClient{
			users:      []map[string]any{userObj("u-abc", "Alice", "alice", "local://u-abc")},
			listsByGVR: map[string]*unstructured.UnstructuredList{"projectroletemplatebindings": {}},
			gets:       map[string]*unstructured.Unstructured{"c-x/" + project: projectGetObj(project)},
		}
		_, _, err := planRevoke(t, NewRevoke(fakeFactory{c: client}), stubInput{
			strings: map[string]string{"project": project, "cluster": "c-x"},
			args:    []string{"u-abc"},
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		return ce
	}
	ce := refused(t, "p-x")
	assert.Equal(t, clierr.Rancher+" get projectroletemplatebinding --namespace 'p-x' -o json", ce.Hint)
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
	for name, v := range unprintableValues {
		t.Run(name, func(t *testing.T) {
			ce := refused(t, "p-"+v)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, clierr.LeftOutNote)
		})
	}
}
