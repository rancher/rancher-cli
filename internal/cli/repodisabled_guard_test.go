package cli

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// repoFlagRows holds, for each command with a repo flag naming a ClusterRepo,
// the arguments it needs besides --repo: its positional arguments and its
// required flags. The commands come from the schema manifest, so a command that
// gains a repo flag fails repoFlagGaps until it has a row. repoFlagGaps checks
// the required flags of a row; a missing positional argument fails the
// refusal test with the command's own usage error.
var repoFlagRows = map[string][]string{
	"charts":      {"--cluster", "prod"},
	"app install": {"my-nginx", "--cluster", "prod", "--namespace", "web", "--chart", "nginx"},
}

// repoFlagGaps returns one problem per command of m that has a flag named
// repo and no row, one per row for a command with no such flag, and one per
// required flag a row leaves out.
func repoFlagGaps(m runtime.Manifest, rows map[string][]string) []string {
	var problems []string
	seen := map[string]bool{}
	for _, c := range m.Commands {
		if !slices.ContainsFunc(c.Flags, func(f runtime.ArgSpec) bool { return f.Name == "repo" }) {
			continue
		}
		seen[c.Name] = true
		row, ok := rows[c.Name]
		if !ok {
			problems = append(problems, c.Name+" has a repo flag and no row")
			continue
		}
		for _, f := range c.Flags {
			if f.Required && f.Name != "repo" && !slices.Contains(row, "--"+f.Name) {
				problems = append(problems, "the row of "+c.Name+" lacks the required flag --"+f.Name)
			}
		}
	}
	for name := range rows {
		if !seen[name] {
			problems = append(problems, "the row of "+name+" names a command with no repo flag")
		}
	}
	slices.Sort(problems)
	return problems
}

// TestEveryRepoFlagRefusesADisabledClusterRepo asserts the rule for every
// command with a repo flag, found in the schema manifest: a disabled
// ClusterRepo is refused with VALIDATION and exit code 2 before any read of
// its index or info.
func TestEveryRepoFlagRefusesADisabledClusterRepo(t *testing.T) {
	m := schemaManifest(t)
	require.Empty(t, repoFlagGaps(m, repoFlagRows))

	s := newChartsServer(t, false)
	chartsFixture(t, s.srv)
	for _, c := range m.Commands {
		if !slices.ContainsFunc(c.Flags, func(f runtime.ArgSpec) bool { return f.Name == "repo" }) {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			args := slices.Concat(strings.Fields(c.Name), repoFlagRows[c.Name], []string{"--repo", "off-charts", "-o", "json"})
			stdout, stderr, err := runChartsCommand(t, args...)
			require.Error(t, err)
			coder, ok := errors.AsType[ucli.ExitCoder](err)
			require.True(t, ok, "the error carries an exit code")
			assert.Equal(t, 2, coder.ExitCode())
			assert.Empty(t, stdout)
			var env chartsErrorEnvelope
			require.NoError(t, json.Unmarshal(stderr, &env))
			assert.Equal(t, "VALIDATION", env.Data.Error.Code)
			assert.Equal(t, "ClusterRepo 'off-charts' is disabled", env.Data.Error.Message)
			assert.False(t, env.Data.Error.Retryable)
			for _, uri := range s.catalogRequests() {
				assert.NotContains(t, uri, "link=", "an index or info read for the disabled ClusterRepo")
			}
		})
	}
}

// TestRepoFlagGapsNamesACommandWithNoRow asserts the gap function names a
// command that has a repo flag and no row, a row for a command with no repo
// flag, and a row without a required flag.
func TestRepoFlagGapsNamesACommandWithNoRow(t *testing.T) {
	t.Parallel()
	m := schemaManifest(t)
	assert.Empty(t, repoFlagGaps(m, repoFlagRows))

	withCommand := m
	withCommand.Commands = slices.Clone(m.Commands)
	withCommand.Commands = append(withCommand.Commands, runtime.SchemaCommand{
		Name:  "charts info",
		Flags: []runtime.ArgSpec{{Name: "repo", Type: "string"}},
	})
	assert.Equal(t, []string{"charts info has a repo flag and no row"}, repoFlagGaps(withCommand, repoFlagRows))

	extra := map[string][]string{"charts": {"--cluster", "prod"}, "app install": repoFlagRows["app install"], "get pods": nil}
	assert.Equal(t, []string{"the row of get pods names a command with no repo flag"}, repoFlagGaps(m, extra))

	short := map[string][]string{"charts": nil, "app install": repoFlagRows["app install"]}
	assert.Equal(t, []string{"the row of charts lacks the required flag --cluster"}, repoFlagGaps(m, short))
}
