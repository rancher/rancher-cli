package cli

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// surfaceRun runs one command line on a fresh root and returns stdout.
func surfaceRun(t *testing.T, scriptArgs []string) string {
	t.Helper()
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer, root.ErrWriter = &out, io.Discard
	_ = root.Run(t.Context(), append([]string{"rancher"}, scriptArgs...))
	return out.String()
}

// helpSections splits a leaf's help into its FLAGS and GLOBAL FLAGS
// text.
func helpSections(help string) (own, globals string) {
	before, globals, _ := strings.Cut(help, "GLOBAL FLAGS:")
	_, own, _ = strings.Cut(before, "FLAGS:")
	return own, globals
}

// listsFlag reports whether a help section has a row for the flag name: a
// line that starts with --name (a description may mention it too).
func listsFlag(section, name string) bool {
	for line := range strings.SplitSeq(section, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--"+name+" ") {
			return true
		}
	}
	return false
}

// folded is s with every run of white space one space, so a wrapped
// description reads as one line.
func folded(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestProjectFlagIsShownWhereTaken asserts, for every registered command,
// that the help, the flag-name completion and the schema agree with the
// declaration: a command that takes --project lists it under its own
// FLAGS and offers it, each with the description the command declares;
// a command that does not take it neither lists nor offers it, not even
// under GLOBAL FLAGS.
func TestProjectFlagIsShownWhereTaken(t *testing.T) {
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv(config.EnvCredentialStore, "file")
	t.Cleanup(func() { CaptureArgv(nil) })
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	schema := map[string]map[string]runtime.ArgSpec{}
	for _, c := range wr.reg.Schema("v-test", "").Commands {
		schema[c.Name] = map[string]runtime.ArgSpec{}
		for _, f := range c.Flags {
			schema[c.Name][f.Name] = f
		}
	}
	taking := 0
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		words := strings.Fields(d.Name)
		var desc string
		takes := runtime.TakesProject(d)
		for _, f := range d.Flags {
			if f.Name == runtime.FlagProject {
				desc = f.Desc
			}
		}
		own, globals := helpSections(surfaceRun(t, slices.Concat(words, []string{"--help"})))
		completion := surfaceRun(t, slices.Concat([]string{"--rancher-completion-proto=2"}, words, []string{"--rancher-completion-cur=--pro", "--generate-shell-completion"}))
		offered := ""
		for line := range strings.SplitSeq(completion, "\n") {
			if candidateToken(line) == "--project" {
				offered = line
			}
		}
		_, inSchema := schema[d.Name][runtime.FlagProject]
		assert.False(t, listsFlag(globals, runtime.FlagProject), "%s: GLOBAL FLAGS never lists --project on a command", d.Name)
		assert.Equal(t, takes, inSchema, "%s: schema", d.Name)
		if !takes {
			assert.False(t, listsFlag(own, runtime.FlagProject), "%s: help", d.Name)
			assert.Empty(t, offered, "%s: completion", d.Name)
			continue
		}
		taking++
		assert.True(t, listsFlag(own, runtime.FlagProject), "%s: help lists --project", d.Name)
		assert.Contains(t, folded(own), folded(strings.ReplaceAll(desc, "`", "")), "%s: help carries the command's description", d.Name)
		assert.Equal(t, "--project:"+desc, offered, "%s: completion offers --project with the command's description", d.Name)
		assert.Equal(t, desc, schema[d.Name][runtime.FlagProject].Desc, "%s: schema description", d.Name)
	}
	assert.Equal(t, 83, taking)
}
