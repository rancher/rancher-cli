package access

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// matchUser is a management User with an id, a display name, a username and
// principal ids, the fields the user lookup reads.
func matchUser(id, display, username string, principals ...string) *unstructured.Unstructured {
	pids := make([]any, 0, len(principals))
	for _, p := range principals {
		pids = append(pids, p)
	}
	obj := map[string]any{
		"apiVersion":   "management.cattle.io/v3",
		"kind":         "User",
		"metadata":     map[string]any{"name": id},
		"username":     username,
		"principalIds": pids,
	}
	if display != "" {
		obj["displayName"] = display
	}
	return &unstructured.Unstructured{Object: obj}
}

// TestAccessUserLookupMatchesNoDisplayName asserts the access commands' user
// lookup matches a value by user id, then by username, and never by display
// name. The cases are those of TestNoUserSubjectResolverMatchesADisplayName
// in internal/engine/rbac, without the one with two display names: this
// lookup's NOT_FOUND does not list the users a display name matches, so a
// display-name value is asserted only not to resolve.
func TestAccessUserLookupMatchesNoDisplayName(t *testing.T) {
	t.Parallel()
	oneDisplay := []*unstructured.Unstructured{
		matchUser("u-aaa", "Dana Display", "dana", "github_user://1"),
		matchUser("u-bbb", "Other", "other", "local://u-bbb"),
	}
	cases := []struct {
		name       string
		users      []*unstructured.Unstructured
		value      string
		resolvesTo string
	}{
		{name: "only a display name", users: oneDisplay, value: "Dana Display"},
		{name: "display name of a user with no username", users: []*unstructured.Unstructured{
			matchUser("u-ext", "Dana Display", "", "github_user://1"),
			matchUser("u-bbb", "Other", "other", "local://u-bbb"),
		}, value: "Dana Display"},
		{name: "display name and another user's username", users: []*unstructured.Unstructured{
			matchUser("u-aaa", "dev", "aaa", "github_user://1"),
			matchUser("u-bbb", "", "dev", "local://u-bbb"),
		}, value: "dev", resolvesTo: "u-bbb"},
		{name: "username", users: oneDisplay, value: "dana", resolvesTo: "u-aaa"},
		{name: "id", users: oneDisplay, value: "u-bbb", resolvesTo: "u-bbb"},
	}
	for _, tc := range cases {
		t.Run("findRancherUser/"+tc.name, func(t *testing.T) {
			t.Parallel()
			list := &unstructured.UnstructuredList{}
			objects := map[string]*unstructured.Unstructured{}
			for _, u := range tc.users {
				list.Items = append(list.Items, *u)
				objects[u.GetName()] = u
			}
			local := &fakeClient{Objects: objects, Lists: map[string]*unstructured.UnstructuredList{"users||": list}}
			u, err := findRancherUser(t.Context(), local, tc.value)
			if tc.resolvesTo == "" {
				require.Error(t, err, "findRancherUser resolved %q, which only a display name matches", tc.value)
				return
			}
			require.NoError(t, err, "findRancherUser must resolve %q", tc.value)
			assert.Equal(t, tc.resolvesTo, u.GetName(), "findRancherUser resolved %q to the wrong user", tc.value)
		})
	}
}
