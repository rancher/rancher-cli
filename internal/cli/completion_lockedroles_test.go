package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedRoleCommands are the commands whose --role value completes live,
// each with whether it offers role templates marked locked and global roles
// that inherit one. Grant creates a binding, which Rancher refuses for such
// a role; revoke removes a binding that already exists.
var lockedRoleCommands = []struct {
	name         string
	offersLocked bool
}{
	{name: "grant", offersLocked: false},
	{name: "revoke", offersLocked: true},
}

// lockedRoleAllowlist names the commands that have a --role or --global-role
// flag completing live and are not in lockedRoleCommands, each with the
// reason.
var lockedRoleAllowlist = map[string]string{
	"create user": "completion and candidates of --global-role are a separate change",
}

// lockedFixtureTemplates are the role templates the --role completion reads,
// one row per state: unlocked, locked, hidden and unlocked, locked spelled
// false, a locked role template with no context, and an unlocked role
// template that inherits a locked one through roleTemplateNames. Each
// carries its own display name, so a leaked description shows as well as a
// leaked name.
var lockedFixtureTemplates = []string{
	`{"metadata":{"name":"project-open"},"context":"project","displayName":"Project Open"}`,
	`{"metadata":{"name":"project-locked"},"context":"project","locked":true,"displayName":"Project Locked"}`,
	`{"metadata":{"name":"project-hidden"},"context":"project","hidden":true,"displayName":"Project Hidden"}`,
	`{"metadata":{"name":"project-false"},"context":"project","locked":false,"displayName":"Project False"}`,
	`{"metadata":{"name":"cluster-open"},"context":"cluster","displayName":"Cluster Open"}`,
	`{"metadata":{"name":"cluster-locked"},"context":"cluster","locked":true,"displayName":"Cluster Locked"}`,
	`{"metadata":{"name":"cluster-hidden"},"context":"cluster","hidden":true,"displayName":"Cluster Hidden"}`,
	`{"metadata":{"name":"cluster-false"},"context":"cluster","locked":false,"displayName":"Cluster False"}`,
	`{"metadata":{"name":"cluster-parent-of-locked"},"context":"cluster","roleTemplateNames":["cluster-locked"],"displayName":"Cluster Parent Of Locked"}`,
	`{"metadata":{"name":"orphan-locked"},"locked":true,"displayName":"Orphan Locked"}`,
}

// lockedFixtureGlobalRole is one global role of the fixture: the JSON of its
// inheritedClusterRoles field ("" leaves the field out) and whether grant
// offers it. Rancher's webhook reads a global role's own
// inheritedClusterRoles only, by name, whatever a role template's context
// and whatever the role template itself inherits, and refuses a binding to
// a global role that names a locked role template or one that does not
// exist. Revoke offers every global role.
type lockedFixtureGlobalRole struct {
	name         string
	display      string
	inherited    string
	grantOffers  bool
	namesNothing bool
}

var lockedFixtureGlobalRoles = []lockedFixtureGlobalRole{
	{name: "global-open", display: "Global Open", grantOffers: true, namesNothing: true},
	{name: "global-empty-list", display: "Global Empty List", inherited: `[]`, grantOffers: true, namesNothing: true},
	{name: "global-inherits-open", display: "Global Inherits Open", inherited: `["cluster-open"]`, grantOffers: true},
	{name: "global-inherits-hidden", display: "Global Inherits Hidden", inherited: `["cluster-hidden"]`, grantOffers: true},
	{name: "global-inherits-false", display: "Global Inherits False", inherited: `["cluster-false"]`, grantOffers: true},
	{name: "global-inherits-parent-of-locked", display: "Global Inherits Parent Of Locked", inherited: `["cluster-parent-of-locked"]`, grantOffers: true},
	{name: "global-inherits-locked", display: "Global Inherits Locked", inherited: `["cluster-locked"]`},
	{name: "global-inherits-project-locked", display: "Global Inherits Project Locked", inherited: `["project-locked"]`},
	{name: "global-inherits-orphan-locked", display: "Global Inherits Orphan Locked", inherited: `["orphan-locked"]`},
	{name: "global-inherits-mixed", display: "Global Inherits Mixed", inherited: `["cluster-open","cluster-locked"]`},
	{name: "global-inherits-missing", display: "Global Inherits Missing", inherited: `["no-such-template"]`},
}

func lockedGlobalRoleJSON(r lockedFixtureGlobalRole) string {
	inherited := ""
	if r.inherited != "" {
		inherited = `,"inheritedClusterRoles":` + r.inherited
	}
	return fmt.Sprintf(`{"metadata":{"name":%q},"displayName":%q%s}`, r.name, r.display, inherited)
}

func lockedGlobalRolesBody(roles []lockedFixtureGlobalRole) string {
	rows := make([]string, 0, len(roles))
	for _, r := range roles {
		rows = append(rows, lockedGlobalRoleJSON(r))
	}
	return items(rows...)
}

// lockedGlobalCandidates lists name:display of the fixture's global roles
// that keep, in name order.
func lockedGlobalCandidates(roles []lockedFixtureGlobalRole, keep func(lockedFixtureGlobalRole) bool) []string {
	var kept []lockedFixtureGlobalRole
	for _, r := range roles {
		if keep(r) {
			kept = append(kept, r)
		}
	}
	slices.SortFunc(kept, func(a, b lockedFixtureGlobalRole) int { return strings.Compare(a.name, b.name) })
	out := make([]string, 0, len(kept))
	for _, r := range kept {
		out = append(out, r.name+":"+r.display)
	}
	return out
}

// newLockedRoleServer serves the role templates and global roles of the
// fixture.
func newLockedRoleServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{
		roleTemplatesPath: items(lockedFixtureTemplates...),
		globalRolesPath:   lockedGlobalRolesBody(lockedFixtureGlobalRoles),
	}, nil)
}

// TestLockedRoleTemplatesInRoleCompletion asserts, through the real TAB
// callback, that grant never offers a role template marked locked at
// project and cluster scope, and at global scope never offers a global role
// that inherits a locked role template or one that does not exist, and that
// revoke always offers every role, in the space form and the --role= form of
// the value. Project and cluster scope make one request. Global scope makes
// one request for revoke and, for grant, a second listing of the role
// templates. Only the full callback shows which command got which source.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestLockedRoleTemplatesInRoleCompletion(t *testing.T) {
	s := newLockedRoleServer(t)
	projectGrant := []string{"project-false:Project False", "project-hidden:Project Hidden", "project-open:Project Open"}
	projectRevoke := []string{"project-false:Project False", "project-hidden:Project Hidden", "project-locked:Project Locked", "project-open:Project Open"}
	clusterGrant := []string{"cluster-false:Cluster False", "cluster-hidden:Cluster Hidden", "cluster-open:Cluster Open", "cluster-parent-of-locked:Cluster Parent Of Locked"}
	clusterRevoke := []string{"cluster-false:Cluster False", "cluster-hidden:Cluster Hidden", "cluster-locked:Cluster Locked", "cluster-open:Cluster Open", "cluster-parent-of-locked:Cluster Parent Of Locked"}
	globalGrant := lockedGlobalCandidates(lockedFixtureGlobalRoles, func(r lockedFixtureGlobalRole) bool { return r.grantOffers })
	globalRevoke := lockedGlobalCandidates(lockedFixtureGlobalRoles, func(lockedFixtureGlobalRole) bool { return true })
	require.Len(t, globalGrant, 6)
	require.Len(t, globalRevoke, 11)
	withPrefix := func(candidates []string) []string {
		out := make([]string, 0, len(candidates))
		for _, c := range candidates {
			out = append(out, "--role="+c)
		}
		return out
	}
	oneTemplates := map[string]int{roleTemplatesPath: 1}
	globalTwo := map[string]int{globalRolesPath: 1, roleTemplatesPath: 1}
	globalOne := map[string]int{globalRolesPath: 1}
	tests := []struct {
		name      string
		tail      []string
		cur       string
		grant     []string
		rev       []string
		grantHits map[string]int
		revHits   map[string]int
	}{
		{name: "project with cluster", tail: []string{"jane", "--project", "Default", "--cluster", "c-aaa11", "--role"}, grant: projectGrant, rev: projectRevoke, grantHits: oneTemplates, revHits: oneTemplates},
		{name: "project alone", tail: []string{"jane", "--project", "Default", "--role"}, grant: projectGrant, rev: projectRevoke, grantHits: oneTemplates, revHits: oneTemplates},
		{name: "project equals form", tail: []string{"jane", "--project=Default", "--cluster=c-aaa11"}, cur: "--role=", grant: withPrefix(projectGrant), rev: withPrefix(projectRevoke), grantHits: oneTemplates, revHits: oneTemplates},
		{name: "cluster", tail: []string{"jane", "--cluster", "c-aaa11", "--role"}, grant: clusterGrant, rev: clusterRevoke, grantHits: oneTemplates, revHits: oneTemplates},
		{name: "cluster equals form", tail: []string{"jane", "--cluster=c-aaa11"}, cur: "--role=", grant: withPrefix(clusterGrant), rev: withPrefix(clusterRevoke), grantHits: oneTemplates, revHits: oneTemplates},
		{name: "global", tail: []string{"jane", "--global", "--role"}, grant: globalGrant, rev: globalRevoke, grantHits: globalTwo, revHits: globalOne},
		{name: "global equals form", tail: []string{"jane", "--global"}, cur: "--role=", grant: withPrefix(globalGrant), rev: withPrefix(globalRevoke), grantHits: globalTwo, revHits: globalOne},
	}
	for _, cmd := range lockedRoleCommands {
		for _, tt := range tests {
			t.Run(cmd.name+" "+tt.name, func(t *testing.T) {
				completionFixture(t, s.srv.URL, true)
				s.reset()
				want, hits := tt.grant, tt.grantHits
				if cmd.offersLocked {
					want, hits = tt.rev, tt.revHits
				}
				got := runCompletion(t, append([]string{cmd.name}, tt.tail...), tt.cur)
				assert.Equal(t, want, got)
				assert.Equal(t, hits, s.reset())
			})
		}
	}
}

// TestGlobalRoleCompletionReadsRoleTemplatesOnlyWhenNeeded asserts grant's
// global --role completion lists the role templates only when a listed
// global role names an inherited one, and answers from the global roles
// alone otherwise; that a role template collection which lists none leaves
// out every global role that names one; and that a role template listing
// that fails (here a 404) leaves only the global roles that name none, while
// revoke offers every global role in all three cases with one request.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestGlobalRoleCompletionReadsRoleTemplatesOnlyWhenNeeded(t *testing.T) {
	naming := []lockedFixtureGlobalRole{lockedFixtureGlobalRoles[0], lockedFixtureGlobalRoles[1]}
	keepAll := func(lockedFixtureGlobalRole) bool { return true }
	noTemplates := lockedGlobalCandidates(lockedFixtureGlobalRoles, func(r lockedFixtureGlobalRole) bool { return r.namesNothing })
	require.Equal(t, []string{"global-empty-list:Global Empty List", "global-open:Global Open"}, noTemplates)
	tests := []struct {
		name      string
		bodies    map[string]string
		grant     []string
		revoke    []string
		grantHits map[string]int
	}{
		{
			name:      "no global role names a role template",
			bodies:    map[string]string{globalRolesPath: lockedGlobalRolesBody(naming), roleTemplatesPath: items(lockedFixtureTemplates...)},
			grant:     noTemplates,
			revoke:    noTemplates,
			grantHits: map[string]int{globalRolesPath: 1},
		},
		{
			name:      "the role template collection lists none",
			bodies:    map[string]string{globalRolesPath: lockedGlobalRolesBody(lockedFixtureGlobalRoles), roleTemplatesPath: items()},
			grant:     noTemplates,
			revoke:    lockedGlobalCandidates(lockedFixtureGlobalRoles, keepAll),
			grantHits: map[string]int{globalRolesPath: 1, roleTemplatesPath: 1},
		},
		{
			name:      "the role template listing fails",
			bodies:    map[string]string{globalRolesPath: lockedGlobalRolesBody(lockedFixtureGlobalRoles)},
			grant:     noTemplates,
			revoke:    lockedGlobalCandidates(lockedFixtureGlobalRoles, keepAll),
			grantHits: map[string]int{globalRolesPath: 1, roleTemplatesPath: 1},
		},
	}
	for _, tt := range tests {
		for _, cmd := range lockedRoleCommands {
			t.Run(tt.name+" "+cmd.name, func(t *testing.T) {
				s := newPathCountingServer(t, tt.bodies, nil)
				completionFixture(t, s.srv.URL, true)
				want, hits := tt.grant, tt.grantHits
				if cmd.offersLocked {
					want, hits = tt.revoke, map[string]int{globalRolesPath: 1}
				}
				got := runCompletion(t, []string{cmd.name, "jane", "--global", "--role"}, "")
				assert.Equal(t, want, got)
				assert.Equal(t, hits, s.reset())
			})
		}
	}
}

// TestLockedRoleCompletionCoversEveryRoleCommand asserts the commands of
// the locked-role table and its allowlist are exactly the commands that
// complete a --role or --global-role value live, so a command added with
// such a flag fails here until its rule for locked role templates is in the
// table or the allowlist. It sees command names and flags named role or
// global-role only; a suggestion path whose flag has another name is not
// found.
func TestLockedRoleCompletionCoversEveryRoleCommand(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, c := range lockedRoleCommands {
		covered[c.name] = true
	}
	for name, reason := range lockedRoleAllowlist {
		assert.NotEmpty(t, reason, name)
		assert.False(t, covered[name], "%s is in both the table and the allowlist", name)
		covered[name] = true
	}
	live := map[string]bool{}
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		if _, ok := probe.(completion.Completer); !ok {
			continue
		}
		d := probe.Describe()
		for _, f := range d.Flags {
			if f.Name == "role" || f.Name == "global-role" {
				live[d.Name] = true
			}
		}
	}
	assert.ElementsMatch(t, slices.Sorted(maps.Keys(live)), slices.Sorted(maps.Keys(covered)))
}

// lockedCandidateCommands are the commands whose NOT_FOUND error lists roles
// to bind, each covered by the candidate tables of internal/engine/rbac.
var lockedCandidateCommands = []string{"grant"}

// lockedCandidateAllowlist names the commands that declare a role or
// global-role flag and are not in lockedCandidateCommands, each with the
// reason.
var lockedCandidateAllowlist = map[string]string{
	"revoke":      "lists the roles the subject holds, not roles to bind; its completion offers every role by design",
	"create user": "candidates of --global-role are a separate change",
}

// TestLockedCandidatesCoverEveryRoleCommand asserts the commands of the
// candidate tables and their allowlist are exactly the commands that declare
// a role or global-role flag, so a command added with such a flag fails here
// until its candidates are in the tables or the allowlist. It sees command
// names and flags named role or global-role only; the rows themselves are in
// internal/engine/rbac and above, and a suggestion path whose flag has
// another name is not found.
func TestLockedCandidatesCoverEveryRoleCommand(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, name := range lockedCandidateCommands {
		covered[name] = true
	}
	for name, reason := range lockedCandidateAllowlist {
		assert.NotEmpty(t, reason, name)
		assert.False(t, covered[name], "%s is in both the tables and the allowlist", name)
		covered[name] = true
	}
	declared := map[string]bool{}
	for _, e := range buildEntries() {
		d := e.make(runDeps{}).Describe()
		for _, f := range d.Flags {
			if f.Name == "role" || f.Name == "global-role" {
				declared[d.Name] = true
			}
		}
	}
	assert.ElementsMatch(t, slices.Sorted(maps.Keys(declared)), slices.Sorted(maps.Keys(covered)))
}
