package rbac

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
)

// TestSubjectCompletionOffersWhatResolveSubjectAccepts asserts the
// subject completion source and ResolveSubject agree over the shared
// user list: every value the source offers resolves, as a bare user
// subject, to the very user it was taken from; every username it leaves
// out is one ResolveSubject refuses or resolves to another user; a system
// account is never offered.
func TestSubjectCompletionOffersWhatResolveSubjectAccepts(t *testing.T) {
	t.Parallel()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(completiontest.SubjectsBody), &list))
	// The fixture repeats one row for the surface's dedupe; a server lists
	// every object once.
	var users []map[string]any
	seen := map[string]bool{}
	for _, u := range list.Items {
		if id := u["metadata"].(map[string]any)["name"].(string); !seen[id] {
			seen[id] = true
			users = append(users, u)
		}
	}
	userKind, ok := kinds.ByKey("user")
	require.True(t, ok)

	s := completiontest.NewFixed(t, completiontest.SubjectsBody)
	got := complete.SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	factory := fakeFactory{c: &fakeClient{users: users}}

	idOf := func(u map[string]any) string { return u["metadata"].(map[string]any)["name"].(string) }
	from := map[string]string{}
	for _, u := range users {
		from[idOf(u)] = idOf(u)
	}
	for _, u := range users {
		if username := u["username"].(string); slices.Contains(got.Names, username) && from[username] == "" {
			from[username] = idOf(u)
		}
	}
	for _, name := range got.Names {
		res, err := ResolveSubject(t.Context(), factory, Subject{Kind: SubjectUser, Bare: name})
		require.NoError(t, err, "%s is offered, so the command must resolve it", name)
		assert.Equal(t, from[name], res.UserID, "%s must resolve to the user it was offered for", name)
	}
	for _, u := range users {
		username := u["username"].(string)
		if userKind.SystemFilter.Matches(u) {
			assert.NotContains(t, got.Names, idOf(u))
			assert.NotContains(t, got.Names, username)
			continue
		}
		if slices.Contains(got.Names, username) {
			continue
		}
		res, err := ResolveSubject(t.Context(), factory, Subject{Kind: SubjectUser, Bare: username})
		assert.True(t, err != nil || res.UserID != idOf(u), "%s is not offered, so the command must refuse it or resolve it elsewhere", username)
	}
}
