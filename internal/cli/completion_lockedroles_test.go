package cli

import (
	"maps"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
)

// lockedRoleCommands are the commands whose --role value completes live,
// each with whether it offers role templates marked locked. Grant creates a
// binding, which Rancher refuses for a locked role template; revoke removes
// a binding that already exists.
var lockedRoleCommands = []struct {
	name         string
	offersLocked bool
}{
	{name: "grant", offersLocked: false},
	{name: "revoke", offersLocked: true},
}

// newLockedRoleServer serves role templates of both contexts in every
// state the --role completion reads (unlocked, locked, hidden and
// unlocked, locked spelled false), a locked role template with no context,
// and global roles including one marked locked. Each carries its own
// display name, so a leaked description shows as well as a leaked name.
func newLockedRoleServer(t *testing.T) *pathCountingServer {
	t.Helper()
	return newPathCountingServer(t, map[string]string{
		roleTemplatesPath: items(
			`{"metadata":{"name":"project-open"},"context":"project","displayName":"Project Open"}`,
			`{"metadata":{"name":"project-locked"},"context":"project","locked":true,"displayName":"Project Locked"}`,
			`{"metadata":{"name":"project-hidden"},"context":"project","hidden":true,"displayName":"Project Hidden"}`,
			`{"metadata":{"name":"project-false"},"context":"project","locked":false,"displayName":"Project False"}`,
			`{"metadata":{"name":"cluster-open"},"context":"cluster","displayName":"Cluster Open"}`,
			`{"metadata":{"name":"cluster-locked"},"context":"cluster","locked":true,"displayName":"Cluster Locked"}`,
			`{"metadata":{"name":"cluster-hidden"},"context":"cluster","hidden":true,"displayName":"Cluster Hidden"}`,
			`{"metadata":{"name":"cluster-false"},"context":"cluster","locked":false,"displayName":"Cluster False"}`,
			`{"metadata":{"name":"orphan-locked"},"locked":true,"displayName":"Orphan Locked"}`),
		globalRolesPath: items(
			`{"metadata":{"name":"global-open"},"displayName":"Global Open"}`,
			`{"metadata":{"name":"global-locked"},"locked":true,"displayName":"Global Locked"}`),
	}, nil)
}

// TestLockedRoleTemplatesInRoleCompletion asserts, through the real TAB
// callback, that grant never offers a role template marked locked at
// project and cluster scope and revoke always does, in the space form and
// the --role= form of the value, with one listing of the scope's
// collection per TAB. A global role is offered by both whatever its body
// carries. Only the full callback shows which command got which source.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates
// storedConfig.
func TestLockedRoleTemplatesInRoleCompletion(t *testing.T) {
	s := newLockedRoleServer(t)
	projectGrant := []string{"project-false:Project False", "project-hidden:Project Hidden", "project-open:Project Open"}
	projectRevoke := []string{"project-false:Project False", "project-hidden:Project Hidden", "project-locked:Project Locked", "project-open:Project Open"}
	clusterGrant := []string{"cluster-false:Cluster False", "cluster-hidden:Cluster Hidden", "cluster-open:Cluster Open"}
	clusterRevoke := []string{"cluster-false:Cluster False", "cluster-hidden:Cluster Hidden", "cluster-locked:Cluster Locked", "cluster-open:Cluster Open"}
	global := []string{"global-locked:Global Locked", "global-open:Global Open"}
	withPrefix := func(candidates []string) []string {
		out := make([]string, 0, len(candidates))
		for _, c := range candidates {
			out = append(out, "--role="+c)
		}
		return out
	}
	tests := []struct {
		name  string
		tail  []string
		cur   string
		grant []string
		rev   []string
		path  string
	}{
		{name: "project with cluster", tail: []string{"jane", "--project", "Default", "--cluster", "c-aaa11", "--role"}, grant: projectGrant, rev: projectRevoke, path: roleTemplatesPath},
		{name: "project alone", tail: []string{"jane", "--project", "Default", "--role"}, grant: projectGrant, rev: projectRevoke, path: roleTemplatesPath},
		{name: "project equals form", tail: []string{"jane", "--project=Default", "--cluster=c-aaa11"}, cur: "--role=", grant: withPrefix(projectGrant), rev: withPrefix(projectRevoke), path: roleTemplatesPath},
		{name: "cluster", tail: []string{"jane", "--cluster", "c-aaa11", "--role"}, grant: clusterGrant, rev: clusterRevoke, path: roleTemplatesPath},
		{name: "cluster equals form", tail: []string{"jane", "--cluster=c-aaa11"}, cur: "--role=", grant: withPrefix(clusterGrant), rev: withPrefix(clusterRevoke), path: roleTemplatesPath},
		{name: "global", tail: []string{"jane", "--global", "--role"}, grant: global, rev: global, path: globalRolesPath},
		{name: "global equals form", tail: []string{"jane", "--global"}, cur: "--role=", grant: withPrefix(global), rev: withPrefix(global), path: globalRolesPath},
	}
	for _, cmd := range lockedRoleCommands {
		for _, tt := range tests {
			t.Run(cmd.name+" "+tt.name, func(t *testing.T) {
				completionFixture(t, s.srv.URL, true)
				s.reset()
				want := tt.grant
				if cmd.offersLocked {
					want = tt.rev
				}
				got := runCompletion(t, append([]string{cmd.name}, tt.tail...), tt.cur)
				assert.Equal(t, want, got)
				assert.Equal(t, map[string]int{tt.path: 1}, s.reset())
			})
		}
	}
}

// TestLockedRoleCompletionCoversEveryRoleCommand asserts the commands of
// the locked-role table are exactly the commands that complete a --role
// value live, so a command added with a --role flag fails here until its
// rule for locked role templates is in the table.
func TestLockedRoleCompletionCoversEveryRoleCommand(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, c := range lockedRoleCommands {
		covered[c.name] = true
	}
	live := map[string]bool{}
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		if _, ok := probe.(completion.Completer); !ok {
			continue
		}
		d := probe.Describe()
		for _, f := range d.Flags {
			if f.Name == "role" {
				live[d.Name] = true
			}
		}
	}
	assert.ElementsMatch(t, slices.Sorted(maps.Keys(live)), slices.Sorted(maps.Keys(covered)))
}
