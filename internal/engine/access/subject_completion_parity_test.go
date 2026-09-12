package access

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/rbac/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestSubjectCompletionOffersWhatTheAccessCommandsResolve asserts the
// shared --as source and the access commands' own user lookup agree over
// the shared user list: every value the source offers resolves to the
// very user it was taken from, and a system account is never offered.
// The projection is stricter than this lookup on purpose - it also
// serves grant and revoke, whose resolver matches display names, so a
// username another user carries as display name is left out here too.
func TestSubjectCompletionOffersWhatTheAccessCommandsResolve(t *testing.T) {
	t.Parallel()
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(completiontest.SubjectsBody), &decoded))
	userKind, ok := kinds.ByKey("user")
	require.True(t, ok)
	// The fixture repeats one row for the surface's dedupe; a server lists
	// every object once.
	list := &unstructured.UnstructuredList{}
	seen := map[string]bool{}
	for _, item := range decoded.Items {
		u := unstructured.Unstructured{Object: item}
		if !seen[u.GetName()] {
			seen[u.GetName()] = true
			list.Items = append(list.Items, u)
		}
	}
	objects := map[string]*unstructured.Unstructured{}
	for i := range list.Items {
		objects[list.Items[i].GetName()] = &list.Items[i]
	}

	s := completiontest.NewFixed(t, completiontest.SubjectsBody)
	got := complete.SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	local := &fakeClient{Objects: objects, Lists: map[string]*unstructured.UnstructuredList{"users||": list}}

	from := map[string]string{}
	for i := range list.Items {
		from[list.Items[i].GetName()] = list.Items[i].GetName()
	}
	for i := range list.Items {
		username, _, _ := unstructured.NestedString(list.Items[i].Object, "username")
		if slices.Contains(got.Names, username) && from[username] == "" {
			from[username] = list.Items[i].GetName()
		}
	}
	for _, name := range got.Names {
		u, err := findRancherUser(t.Context(), local, name)
		require.NoError(t, err, "%s is offered, so the commands must resolve it", name)
		assert.Equal(t, from[name], u.GetName(), "%s must resolve to the user it was offered for", name)
	}
	for i := range list.Items {
		if userKind.SystemFilter.Matches(list.Items[i].Object) {
			username, _, _ := unstructured.NestedString(list.Items[i].Object, "username")
			assert.NotContains(t, got.Names, list.Items[i].GetName())
			assert.NotContains(t, got.Names, username)
		}
	}
	assert.NotContains(t, got.Names, "nopids", "another user's display name; left out for grant's and revoke's resolver")
}
