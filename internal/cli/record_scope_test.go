package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// TestLinesWithNoChosenCommandRecordNothing asserts a line with no command
// chosen, or one the exclusion table names, stores no event and builds none:
// each refusal the CLI makes before urfave parses the line, help, the
// version, a group or the root with no word, an unknown command at the root
// and in a group, an unknown flag, a flag with no value, the completion
// script and each telemetry command.
func TestLinesWithNoChosenCommandRecordNothing(t *testing.T) {
	rows := map[string][]string{
		"words after a lone dash":       {"get", "cluster", "-", "x"},
		"empty bool value":              {"get", "cluster", "--dry-run="},
		"bool word after a bool flag":   {"get", "cluster", "--dry-run", "true"},
		"help":                          {"get", "cluster", "--help"},
		"short help":                    {"get", "cluster", "-h"},
		"version":                       {"--version"},
		"group with no word":            {"config"},
		"the root with no word":         {},
		"unknown command":               {"bogus"},
		"unknown command in a group":    {"config", "bogus"},
		"unknown flag":                  {"get", "cluster", "--nope"},
		"flag with no value":            {"get", "cluster", "-o"},
		"completion request":            {protoSentinel, "get", "cluster", ""},
		"completion script":             {"completion", "bash"},
		"telemetry status":              {"telemetry", "status"},
		"telemetry on":                  {"telemetry", "on"},
		"telemetry off":                 {"telemetry", "off"},
		"telemetry reset":               {"telemetry", "reset"},
		"telemetry show":                {"telemetry", "show"},
		"telemetry upload":              {"telemetry", "upload"},
		"completion request with flags": {protoSentinel, "get", "cluster", "--output", ""},
	}
	for name, args := range rows {
		t.Run(name, func(t *testing.T) {
			dir := recordingEnv(t)
			builds := countBuilds(t)
			runRecorded(t, newRoot(t), args...)
			assert.Empty(t, storedRuns(t, dir))
			assert.Zero(t, *builds)
		})
	}
}

// TestCompletionRequestsTouchNoTelemetryFile asserts a completion request
// does no telemetry file operation and no stat, with or without a config
// directory, and creates no directory.
func TestCompletionRequestsTouchNoTelemetryFile(t *testing.T) {
	for _, exists := range []bool{true, false} {
		name := "directory missing"
		if exists {
			name = "directory present"
		}
		t.Run(name, func(t *testing.T) {
			dir := recordingEnv(t)
			if !exists {
				dir = filepath.Join(dir, "missing")
				t.Setenv(config.EnvConfigDir, dir)
			}
			ops := observeFiles(t)
			builds := countBuilds(t)
			runRecorded(t, newRoot(t), protoSentinel, "get", "cluster", "")
			assert.Empty(t, *ops)
			assert.Zero(t, *builds)
			if !exists {
				assert.NoDirExists(t, dir)
			}
		})
	}
}

// TestRecordingOffByEachControlTouchesNothing asserts that each control that
// turns recording off - the variable, the config key, a config that does not
// load - leaves no telemetry file operation and no stat, builds no event
// and stores none, for a command that records when on.
func TestRecordingOffByEachControlTouchesNothing(t *testing.T) {
	rows := map[string]func(t *testing.T, dir string){
		"variable": func(t *testing.T, _ string) {
			t.Helper()
			t.Setenv(telemetry.EnvTelemetry, "false")
		},
		"key": func(t *testing.T, dir string) {
			t.Helper()
			writeConfig(t, dir, "telemetry: false\n")
		},
		"unloadable config": func(t *testing.T, dir string) {
			t.Helper()
			writeConfig(t, dir, "servers: [\n")
		},
	}
	for name, setup := range rows {
		t.Run(name, func(t *testing.T) {
			dir := recordingEnv(t)
			setup(t, dir)
			ops := observeFiles(t)
			builds := countBuilds(t)
			runRecorded(t, newRoot(t), "config", "path")
			assert.Empty(t, *ops)
			assert.Zero(t, *builds)
			assert.Empty(t, storedRuns(t, dir))
		})
	}
}

// TestNoConfigDirectoryRecordsNothingAndCreatesNone asserts that with
// recording on and no config directory the hook makes one stat of it and
// nothing else touches a telemetry file; the commands that read or write
// nothing there leave the directory missing and store no event.
func TestNoConfigDirectoryRecordsNothingAndCreatesNone(t *testing.T) {
	rows := map[string][]string{
		"config path":      {"config", "path"},
		"schema":           {"schema"},
		"skill status":     {"skill", "status", "--agent", "claude"},
		"telemetry status": {"telemetry", "status"},
		"telemetry upload": {"telemetry", "upload"},
	}
	for name, args := range rows {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(recordingEnv(t), "missing")
			t.Setenv(config.EnvConfigDir, dir)
			ops := observeFiles(t)
			builds := countBuilds(t)
			runRecorded(t, newRoot(t), args...)
			assert.Equal(t, []string{"stat " + dir}, *ops)
			assert.Zero(t, *builds)
			assert.NoDirExists(t, dir)
		})
	}
}

// TestACommandThatCreatesTheDirectoryRecordsNothing asserts a run that
// creates the config directory is still judged by whether the directory
// existed when it began: no event is built or stored.
func TestACommandThatCreatesTheDirectoryRecordsNothing(t *testing.T) {
	dir := filepath.Join(recordingEnv(t), "missing")
	t.Setenv(config.EnvConfigDir, dir)
	ops := observeFiles(t)
	builds := countBuilds(t)
	got := runRecorded(t, newRoot(t), "config", "set-token-reuse", "on")
	require.Zero(t, got.res.code, got.stderr)
	require.DirExists(t, dir)
	assert.Equal(t, []string{"stat " + dir}, *ops)
	assert.Zero(t, *builds)
	assert.Empty(t, storedRuns(t, dir))
}

// TestRecordedFlagsAreTheNamesTyped asserts the flag names stored: an alias
// by its long name, a global flag typed after the command word, a repeated
// flag once, nothing after --, nothing set from the environment, and a bool
// typed with =true; and that every recorded command and flag name is in the
// manifest of the command surface.
func TestRecordedFlagsAreTheNamesTyped(t *testing.T) {
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	rows := []struct {
		name string
		args []string
		want []string
	}{
		{"alias by long name", []string{"get", "cluster", "-o", "json"}, []string{"output"}},
		{"global flag after the command word", []string{"get", "cluster", "--no-input"}, []string{"no-input"}},
		{"global flags either side of the command", []string{"-o", "json", "get", "cluster", "--no-input"}, []string{"no-input", "output"}},
		{"repeated flag once", []string{"get", "cluster", "-o", "json", "--output", "yaml"}, []string{"output"}},
		{"words after the double dash", []string{"get", "cluster", "--", "--yes", "-o", "json"}, nil},
		{"bool typed with =true", []string{"get", "cluster", "--dry-run=true"}, []string{"dry-run"}},
		{"own flag and global flag", []string{"config", "path", "-c", "DIR"}, []string{"config"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := recordingEnv(t)
			args := slices.Clone(row.args)
			for i, a := range args {
				if a == "DIR" {
					args[i] = dir
				}
			}
			runRecorded(t, newRoot(t), args...)
			evs := storedRuns(t, dir)
			require.Len(t, evs, 1)
			assert.Equal(t, row.want, storedFlags(evs[0]))
			name, _ := evs[0].Fields["io.rancher.cli.command"].(string)
			assert.True(t, manifestHasFlags(m, name, storedFlags(evs[0])), "%s %v", name, storedFlags(evs[0]))
		})
	}
	t.Run("flag set from the environment", func(t *testing.T) {
		dir := recordingEnv(t)
		runRecorded(t, newRoot(t), "config", "path")
		evs := storedRuns(t, dir)
		require.Len(t, evs, 1)
		assert.Empty(t, storedFlags(evs[0]), "RANCHER_CLI_CONFIG_DIR sets config; it was not typed")
	})
}

// manifestHasFlags reports whether the manifest has the command and each
// flag, as a flag of the command or a global flag.
func manifestHasFlags(m runtime.Manifest, command string, flags []string) bool {
	var cmd *runtime.SchemaCommand
	for i := range m.Commands {
		if m.Commands[i].Name == command {
			cmd = &m.Commands[i]
		}
	}
	if cmd == nil {
		return false
	}
	global := map[string]bool{}
	for _, g := range m.GlobalFlags {
		long := g.Name
		if i := strings.LastIndex(long, "/"); i >= 0 {
			long = long[i+1:]
		}
		global[strings.TrimPrefix(long, "--")] = true
	}
	for _, f := range flags {
		found := global[f]
		for _, own := range cmd.Flags {
			found = found || own.Name == f
		}
		if !found {
			return false
		}
	}
	return true
}

// TestTheHookStateNeverLeaksBetweenRuns asserts that, on one root, the state
// the hook kept survives the run that made it (the test reads it there), a
// run that does not reach the hook records nothing even when an earlier run
// did, and an unknown command between two recorded runs affects neither.
func TestTheHookStateNeverLeaksBetweenRuns(t *testing.T) {
	dir := recordingEnv(t)
	root := newRoot(t)
	runRecorded(t, root, "config", "path")
	st := recordingStateNow()
	assert.True(t, st.on)
	assert.True(t, st.dirExisted)
	assert.Equal(t, dir, st.dir)
	require.Len(t, storedRuns(t, dir), 1)

	root.Before = nil
	runRecorded(t, root, "config", "path")
	assert.Len(t, storedRuns(t, dir), 1, "a run that never reaches the hook records nothing")
	assert.Equal(t, recordingState{}, recordingStateNow())
}

// TestRunsOnOneRootRecordWhatEachShould asserts an unknown command after a
// recorded line records nothing, and a recorded line after an unknown
// command records one event.
func TestRunsOnOneRootRecordWhatEachShould(t *testing.T) {
	dir := recordingEnv(t)
	root := newRoot(t)
	runRecorded(t, root, "config", "path")
	require.Len(t, storedRuns(t, dir), 1)
	runRecorded(t, root, "bogus")
	require.Len(t, storedRuns(t, dir), 1)
	runRecorded(t, root, "config", "status")
	var commands []any
	for _, ev := range storedRuns(t, dir) {
		commands = append(commands, ev.Fields["io.rancher.cli.command"])
	}
	assert.ElementsMatch(t, []any{"config path", "config status"}, commands, "events of one hour have no stored order")
	assert.True(t, recordingStateNow().on)

	fresh := newRoot(t)
	runRecorded(t, fresh, "bogus")
	runRecorded(t, fresh, "config", "path")
	assert.Len(t, storedRuns(t, dir), 3)
}

// TestTheHookStateIsResetWhenARunStarts asserts run clears the hook state
// before urfave runs: a state left on by an earlier run does not record a run
// that never reaches the hook.
func TestTheHookStateIsResetWhenARunStarts(t *testing.T) {
	dir := recordingEnv(t)
	root := newRoot(t)
	runRecorded(t, root, "config", "path")
	require.True(t, recordingStateNow().on)
	runRecorded(t, root, "--version")
	assert.Equal(t, recordingState{}, recordingStateNow())
	assert.Len(t, storedRuns(t, dir), 1)
}
