package projects

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// unprintableNames are values no shell reads as written: one for each kind of
// refusal.
var unprintableNames = map[string]string{
	"apostrophe":       "team'a",
	"right quote":      "team\u2019a",
	"zero width":       "team\u200ba",
	"double backslash": "team\\\\a",
}

// TestRemoveNamespaceHintPreviewsTheRemoval asserts the preview of removing a
// namespace from its project, for an ordinary name and for a name that starts
// with a dash.
func TestRemoveNamespaceHintPreviewsTheRemoval(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" project remove-namespace 'team-a' --cluster 'c-x' --dry-run", removeNamespaceHint("team-a", "c-x").String())
	assert.Equal(t, clierr.Rancher+" project remove-namespace --cluster 'c-x' --dry-run -- '-x'", removeNamespaceHint("-x", "c-x").String())
}

// TestRemoveNamespaceHintIsLeftOutForAnUnprintableName asserts a namespace name
// no shell reads as written leaves the preview out of the refusal hint.
func TestRemoveNamespaceHintIsLeftOutForAnUnprintableName(t *testing.T) {
	t.Parallel()
	hinter, ok := NewRemoveNamespace(stubFactory{}).(runtime.ProjectRefusalHinter)
	require.True(t, ok)
	for name, ns := range unprintableNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.False(t, removeNamespaceHint(ns, "c-x").OK())
			assert.Empty(t, hinter.ProjectRefusalHint(in{s: map[string]string{"cluster": "c-x"}, a: []string{ns}}).Field())
		})
	}
}

// TestAddNamespaceInAnotherProjectLeavesOutThePreviewOfAnUnprintableName
// asserts the refusal of a namespace that belongs to another project hints the
// removal preview and says so, and with a name no shell reads as written leaves
// the command and the sentence about it out and carries the note.
func TestAddNamespaceInAnotherProjectLeavesOutThePreviewOfAnUnprintableName(t *testing.T) {
	t.Parallel()
	plan := func(t *testing.T, name string) *clierr.Error {
		t.Helper()
		client := &membershipClient{namespaces: map[string]*unstructured.Unstructured{name: namespaceObj(name, "p-2")}}
		_, _, err := planMembership(t, projectCtx(t, "p-1"), NewAddNamespace(stubFactory{c: client}),
			in{s: map[string]string{"project": "web", "cluster": "c-x"}, a: []string{name}})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		return ce
	}
	ce := plan(t, "team-a")
	assert.Equal(t, clierr.Rancher+" project remove-namespace 'team-a' --cluster 'c-x' --dry-run", ce.Hint)
	assert.Contains(t, ce.Message, "the hint previews the removal")
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
	for name, ns := range unprintableNames {
		t.Run(name, func(t *testing.T) {
			ce := plan(t, ns)
			assert.Empty(t, ce.Hint)
			assert.NotContains(t, ce.Message, "the hint previews")
			assert.Contains(t, ce.Message, clierr.LeftOutNote)
		})
	}
}
