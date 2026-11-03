package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubjectSourceClaim asserts the shared source claims the --as value
// alone, with a management need, no cluster need and one listing, and
// declines it under --type group, where a bare value is a group; a
// --type typed twice takes its last spelling.
func TestSubjectSourceClaim(t *testing.T) {
	t.Parallel()
	src := SubjectSource()
	reqs, ok := src.Claim(completion.Request{Flag: "as", Cluster: "prod cluster"})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	_, ok = src.Claim(completion.Request{Flag: "as", Tail: []string{"--type", "user", "--as"}})
	assert.True(t, ok)
	_, ok = src.Claim(completion.Request{Flag: "as", Tail: []string{"--type", "group", "--type", "user", "--as"}})
	assert.True(t, ok, "a --type typed twice takes its last spelling")
	for _, req := range []completion.Request{{Index: 0}, {Flag: "cluster"}, {Flag: "type"}, {Flag: "role"},
		{Flag: "as", Tail: []string{"--type", "group", "--as"}}, {Flag: "as", Tail: []string{"--type=group", "--as"}},
		{Flag: "as", Tail: []string{"--type", "user", "--type", "group", "--as"}}} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestGrantAndRevokeSourceClaim asserts grant's and revoke's sources claim
// the same sites: --role under the role rule and the subject positional
// unless --type group is typed (a bare --type, or --type user, still
// claims; a --type typed twice takes its last spelling), and decline every
// other site. The request budget of the --role site differs: grant claims
// two requests at global scope only, where it lists the global roles and
// then the role templates, and one request at project scope, cluster scope,
// no scope and a conflicting scope; revoke and RoleSource claim one request
// everywhere.
func TestGrantAndRevokeSourceClaim(t *testing.T) {
	t.Parallel()
	scopes := []struct {
		name  string
		tail  []string
		grant completion.Requirements
	}{
		{name: "global", tail: []string{"--global"}, grant: completion.ManagementTwoListings()},
		{name: "global equals form", tail: []string{"--global=true"}, grant: completion.ManagementTwoListings()},
		{name: "project", tail: []string{"--project", "Default"}, grant: completion.ManagementListing()},
		{name: "cluster", tail: []string{"--cluster", "c-x"}, grant: completion.ManagementListing()},
		{name: "no scope", grant: completion.ManagementListing()},
		{name: "conflicting scope", tail: []string{"--global", "--cluster", "c-x"}, grant: completion.ManagementListing()},
		{name: "global switched off", tail: []string{"--global=false", "--cluster", "c-x"}, grant: completion.ManagementListing()},
	}
	for _, sc := range scopes {
		t.Run("role "+sc.name, func(t *testing.T) {
			t.Parallel()
			tail := append([]string{"jane"}, sc.tail...)
			tail = append(tail, "--role")
			reqs, ok := GrantSource().Claim(roleRequest(append([]string{"grant"}, tail...)...))
			require.True(t, ok, "a --role site is owned at every scope; Complete answers nothing without one")
			assert.Equal(t, sc.grant, reqs)
			reqs, ok = RevokeSource().Claim(roleRequest(append([]string{"revoke"}, tail...)...))
			require.True(t, ok)
			assert.Equal(t, completion.ManagementListing(), reqs)
			reqs, ok = RoleSource().Claim(roleRequest(append([]string{"grant"}, tail...)...))
			require.True(t, ok)
			assert.Equal(t, completion.ManagementListing(), reqs)
		})
	}
	for name, src := range map[string]completion.Source{"grant": GrantSource(), "revoke": RevokeSource()} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, tail := range [][]string{nil, {"--type"}, {"--type", "user"}, {"--type=user"}, {"--cluster", "prod cluster"}, {"--type", "group", "--type", "user"}, {"--global"}, {"--global=true"}, {"--global", "--type", "user"}} {
				reqs, ok := src.Claim(completion.Request{Index: 0, Tail: tail})
				assert.True(t, ok, "%v", tail)
				assert.Equal(t, completion.ManagementListing(), reqs)
			}
			for _, req := range []completion.Request{
				{Index: 0, Tail: []string{"--type", "group"}},
				{Index: 0, Tail: []string{"--type=group"}},
				{Index: 0, Tail: []string{"--type", "user", "--type", "group"}},
				{Index: 1, Tail: []string{"jane"}},
				{Flag: "cluster"},
				{Flag: "project"},
				{Flag: "type"},
				{Flag: "as"},
			} {
				_, ok := src.Claim(req)
				assert.False(t, ok, "%+v", req)
			}
		})
	}
}

// TestCompleteSubjects asserts one listing answers every non-system
// user's id in list order, each followed by its username when no other
// user carries that string as username or display name and no account,
// a system one included, carries it as id; a user without principal ids
// left out; a repeated row once; usernames answered raw for the surface
// to sanitize.
func TestCompleteSubjects(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, completiontest.SubjectsBody)
	got := SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	assert.Equal(t, []string{
		"u-alice", "alice",
		"u-bob", "bob",
		"u-badname", "bad name!",
		"sameid", "sameid",
		"u-dup1", "u-dup2",
		"u-carol", "carol",
		"u-dave",
		"u-erin",
	}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/users"}, s.Requested())

	again := completiontest.NewFixed(t, completiontest.SubjectsBody)
	assert.Equal(t, got.Names, GrantSource().Complete(t.Context(), again.Conn(1), completion.Request{Index: 0}).Names, "the subject positional and --as share the projection")
}

// TestCompleteSubjectsFailsEmpty asserts a refused listing answers
// nothing.
func TestCompleteSubjectsFailsEmpty(t *testing.T) {
	t.Parallel()
	s := completiontest.NewStatus(t, 403)
	got := SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	assert.Empty(t, got.Names)
}

// TestCompleteSubjectsDescribesEveryIdAndNoUsername asserts each offered
// id carries its username and display name, raw: both, the username alone
// when the display name is empty or the same, and a user whose username is
// another account's id described by that text. A username offered as a
// value carries no description, except a username equal to its own id,
// which is the id's row.
func TestCompleteSubjectsDescribesEveryIdAndNoUsername(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, completiontest.SubjectsBody)
	got := SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	assert.Equal(t, map[string]string{
		"u-alice":   "alice (Alice)",
		"u-bob":     "bob (Bob)",
		"u-badname": "bad name!",
		"sameid":    "sameid",
		"u-dup1":    "dup (Dup One)",
		"u-dup2":    "dup (Dup Two)",
		"u-carol":   "carol (nopids)",
		"u-dave":    "u-alice (Dave)",
		"u-erin":    "u-sys (Erin)",
	}, got.Descriptions)
	for _, username := range []string{"alice", "bob", "carol"} {
		assert.Contains(t, got.Names, username)
		assert.NotContains(t, got.Descriptions, username, "a username offered as a value carries no description")
	}

	again := completiontest.NewFixed(t, completiontest.SubjectsBody)
	assert.Equal(t, got.Descriptions, GrantSource().Complete(t.Context(), again.Conn(1), completion.Request{Index: 0}).Descriptions)
}

// TestCompleteSubjectsKeepsAHostileDisplayNameRaw asserts text with a
// newline and an escape sequence reaches the surface unchanged, where the
// sanitizer turns it into one line.
func TestCompleteSubjectsKeepsAHostileDisplayNameRaw(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"u-evil"},"username":"evil","displayName":"a\nb\u001b[31m","principalIds":["local://u-evil"]}
	]}`)
	got := SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	assert.Equal(t, map[string]string{"u-evil": "evil (a\nb\u001b[31m)"}, got.Descriptions)
}

// TestCompleteSubjectsDescribesWithTheLookupFieldsOnly asserts the
// description is built from the fields of the user row's Handle.Lookup and
// from no other field the object carries.
func TestCompleteSubjectsDescribesWithTheLookupFieldsOnly(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"u-1"},"username":"alice","displayName":"Alice Smith","description":"decoy description","spec":{"name":"decoy spec","displayName":"decoy display"},"principalIds":["local://u-1"]},
		{"metadata":{"name":"u-2"},"description":"decoy description","spec":{"displayName":"decoy display"},"principalIds":["local://u-2"]}
	]}`)
	got := SubjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "as"})
	assert.Equal(t, map[string]string{"u-1": "alice (Alice Smith)"}, got.Descriptions)
}
