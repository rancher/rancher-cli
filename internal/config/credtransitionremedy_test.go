package config

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestDeleteServerCommandsNamesOneCommandPerEntry asserts the commands that
// remove the entries a refused store change names: one command for each, in
// the order given, separated by a semicolon, with a name that starts with a
// dash written after "--".
func TestDeleteServerCommandsNamesOneCommandPerEntry(t *testing.T) {
	t.Parallel()
	del := clierr.Rancher + " config delete-server"
	assert.Empty(t, deleteServerCommands(nil))
	assert.Equal(t, del+" 'qa'", deleteServerCommands([]string{"qa"}))
	assert.Equal(t, del+" -- '-x'", deleteServerCommands([]string{"-x"}))
	assert.Equal(t, del+" -- '-x'; "+del+" 'qa'; "+del+" 'it'\\''s'", deleteServerCommands([]string{"-x", "qa", "it's"}))
}

// TestAStoreChangeRefusalNamesTheCommandThatRemovesEachEntry asserts the
// refusal of a store change that a credential helper blocks, whether the
// helper is the source or the destination, ends its remedy with one remove
// command per other entry and then asks for the command to run again.
func TestAStoreChangeRefusalNamesTheCommandThatRemovesEachEntry(t *testing.T) {
	del := clierr.Rancher + " config delete-server"
	for _, tc := range []struct {
		name     string
		from, to string
	}{
		{"the helper is the source", "exec:fake", backendKeychain},
		{"the helper is the destination", backendFile, "exec:fake"},
	} {
		for _, entries := range []struct {
			name   string
			others map[string]Server
			want   string
		}{
			{"one other entry", map[string]Server{"qa": {URL: "https://q.ex"}},
				"or remove it with: " + del + " 'qa'; then run this command again"},
			{"two other entries", map[string]Server{"qa": {URL: "https://q.ex"}, "staging": {URL: "https://s.ex"}},
				"or remove them with: " + del + " 'qa'; " + del + " 'staging'; then run this command again"},
		} {
			t.Run(tc.name+", "+entries.name, func(t *testing.T) {
				dir, cfg := planFixture(t)
				t.Setenv("PATH", t.TempDir())
				cfg.Servers = map[string]Server{"prod": {URL: "https://r.ex"}}
				maps.Copy(cfg.Servers, entries.others)
				err := CheckTransitionSources(dir, cfg, "prod", tc.from, tc.to)
				ce, ok := errorsAsClierr(err)
				require.True(t, ok, "%v", err)
				assert.True(t, strings.HasSuffix(ce.Message, entries.want), "the message ends with the remove commands: %q", ce.Message)
			})
		}
	}
}

// TestRevalidateNamesTheTurnedOffSettingWithNoCommand asserts the abort
// reason of a login whose token reuse setting was turned off meanwhile is the
// fact alone, so that it survives the cut of an unprintable config directory
// and does not read as a command to run.
func TestRevalidateNamesTheTurnedOffSettingWithNoCommand(t *testing.T) {
	t.Parallel()
	cfg := &Config{TokenReuse: false}
	got := revalidate(cfg, PersistLoginInput{Decision: &StoreDecision{RequireSetting: true}})
	assert.Equal(t, "token reuse was turned off during the login", got)
	assert.False(t, clierr.HasRemedy(got))
}
