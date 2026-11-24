package rbac

import (
	"errors"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// userSubjectResolver resolves value against users and returns the id of the
// user it resolved to, or the error the command fails with.
type userSubjectResolver func(t *testing.T, users []map[string]any, value string) (string, error)

// userSubjectResolvers are the resolvers of a user value that is not a fully
// qualified principal: the rbac resolver, grant's and revoke's Plan, and the
// inspect user resolver. A new resolver gets a row here, or in
// TestAccessUserLookupMatchesNoDisplayName when it lives in another package;
// TestEveryUserLookupIsListed in test/conformance fails until it does.
var userSubjectResolvers = map[string]userSubjectResolver{
	"ResolveSubject": func(t *testing.T, users []map[string]any, value string) (string, error) {
		t.Helper()
		rs, err := ResolveSubject(t.Context(), fakeFactory{c: &fakeClient{users: users}}, Subject{Kind: SubjectUser, Bare: value})
		return rs.UserID, err
	},
	"grant": func(t *testing.T, users []map[string]any, value string) (string, error) {
		t.Helper()
		client := &fakeClient{
			users: users,
			gets: map[string]*unstructured.Unstructured{
				"c-x/p-x":         projectGetObj("p-x"),
				"/project-member": roleGet("project-member"),
			},
			listsByGVR:  map[string]*unstructured.UnstructuredList{"projectroletemplatebindings": {}},
			allowCreate: true,
		}
		_, _, err := planGrant(t, NewGrant(fakeFactory{c: client}), stubInput{
			strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
			args:    []string{value},
		})
		if err != nil {
			return "", err
		}
		require.NotNil(t, client.created, "a grant that plans creates a binding")
		userName, _, _ := unstructured.NestedString(client.created.Object, "userName")
		return userName, nil
	},
	"revoke": func(t *testing.T, users []map[string]any, value string) (string, error) {
		t.Helper()
		bindings := &unstructured.UnstructuredList{}
		for _, u := range users {
			id := u["metadata"].(map[string]any)["name"].(string)
			bindings.Items = append(bindings.Items,
				revokeBinding("prtb-"+id, "p-x", "rv-"+id, "roleTemplateName", "project-member", id, "", ""))
		}
		client := &fakeClient{
			users:       users,
			gets:        map[string]*unstructured.Unstructured{"c-x/p-x": projectGetObj("p-x")},
			listsByGVR:  map[string]*unstructured.UnstructuredList{"projectroletemplatebindings": bindings},
			allowDelete: true,
		}
		eff, _, err := planRevoke(t, NewRevoke(fakeFactory{c: client}), stubInput{
			strings: map[string]string{"project": "p-x", "cluster": "c-x", "role": "project-member"},
			args:    []string{value},
		})
		if err != nil {
			return "", err
		}
		id, ok := strings.CutPrefix(eff.DisplayName, "prtb-")
		require.True(t, ok, "revoke deletes the binding of one user, got %q", eff.DisplayName)
		return id, nil
	},
	"ResolveInspectUser": func(t *testing.T, users []map[string]any, value string) (string, error) {
		t.Helper()
		rs, err := ResolveInspectUser(t.Context(), fakeFactory{c: &fakeClient{users: users}}, value)
		return rs.UserID, err
	},
}

// displayNameCase is one value typed against one list of users: either the
// id of the user it resolves to, or the NOT_FOUND it fails with and the ids
// of the users that error lists.
type displayNameCase struct {
	name       string
	users      []map[string]any
	value      string
	resolvesTo string
	candidates []string
}

// displayNameCases are the cases every user subject resolver runs: a value
// that is only one user's display name, the same for a user of an external
// provider with no username and a display name the provider set, a value
// that is one user's display name and another user's username, a value two
// users have as display name, and the username and id controls.
func displayNameCases() []displayNameCase {
	oneDisplay := []map[string]any{
		userObj("u-aaa", "Dana Display", "dana", "github_user://1"),
		userObj("u-bbb", "Other", "other", "local://u-bbb"),
	}
	return []displayNameCase{
		{name: "only a display name", users: oneDisplay, value: "Dana Display", candidates: []string{"u-aaa"}},
		{name: "display name of a user with no username", users: []map[string]any{
			userObj("u-ext", "Dana Display", "", "github_user://1"),
			userObj("u-bbb", "Other", "other", "local://u-bbb"),
		}, value: "Dana Display", candidates: []string{"u-ext"}},
		{name: "display name and another user's username", users: []map[string]any{
			userObj("u-aaa", "dev", "aaa", "github_user://1"),
			userObj("u-bbb", "", "dev", "local://u-bbb"),
		}, value: "dev", resolvesTo: "u-bbb"},
		{name: "display name of two users", users: []map[string]any{
			userObj("u-aaa", "Jane", "jane1", "github_user://1"),
			userObj("u-bbb", "Jane", "jane2", "github_user://2"),
		}, value: "Jane", candidates: []string{"u-aaa", "u-bbb"}},
		{name: "username", users: oneDisplay, value: "dana", resolvesTo: "u-aaa"},
		{name: "id", users: oneDisplay, value: "u-bbb", resolvesTo: "u-bbb"},
	}
}

// TestNoUserSubjectResolverMatchesADisplayName asserts every resolver of a
// user value that is not a fully qualified principal matches it by user id,
// then by username, and never by display name: a value that only display
// names match fails with NOT_FOUND and lists exactly those users.
func TestNoUserSubjectResolverMatchesADisplayName(t *testing.T) {
	t.Parallel()
	for row, resolve := range userSubjectResolvers {
		for _, tc := range displayNameCases() {
			t.Run(row+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				got, err := resolve(t, tc.users, tc.value)
				if tc.resolvesTo != "" {
					require.NoError(t, err, "%s must resolve %q", row, tc.value)
					assert.Equal(t, tc.resolvesTo, got, "%s resolved %q to the wrong user", row, tc.value)
					return
				}
				require.Error(t, err, "%s resolved %q, which only display names match, to %s", row, tc.value, got)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%s: want a *clierr.Error, got %v", row, err)
				assert.Equal(t, clierr.CodeNotFound, ce.Code, "%s: %s", row, ce.Message)
				var ids []string
				for _, c := range ce.Candidates {
					ids = append(ids, c.ID)
				}
				assert.Equal(t, tc.candidates, ids, "%s lists the users that have %q as display name", row, tc.value)
			})
		}
	}
}
