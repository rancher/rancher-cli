package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// excludedNames are the registered names that record nothing.
var excludedNames = []string{
	"completion", "telemetry status", "telemetry on", "telemetry off", "telemetry reset", "telemetry show",
}

// TestTelemetryExcludedHasExactlyTheSixCommands asserts the exclusion table
// holds the six names, each with a reason.
func TestTelemetryExcludedHasExactlyTheSixCommands(t *testing.T) {
	keys := make([]string, 0, len(telemetryExcluded))
	for name, reason := range telemetryExcluded {
		keys = append(keys, name)
		assert.NotEmpty(t, reason, name)
	}
	assert.ElementsMatch(t, excludedNames, keys)
}

// TestEveryRegisteredCommandRecordsOrIsExcluded asserts that, with recording
// on and the directory present, the decision for the minimal line of each
// registered command is to record exactly when its name is not excluded,
// and that the descriptor it returns is the command's.
func TestEveryRegisteredCommandRecordsOrIsExcluded(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	root := newRoot(t)
	on := recordingState{on: true, dirExisted: true}
	for _, c := range wr.reg.Commands() {
		d := c.Describe()
		line := strings.Fields(d.Name)
		got, records := recordDecision(keptLine{line: parseLine(root, line), parsed: true}, on)
		assert.Equal(t, !slices.Contains(excludedNames, d.Name), records, d.Name)
		if records {
			assert.Equal(t, d.Name, got.Name)
		}
	}
	require.NotEmpty(t, wr.reg.Commands())
}

// TestRecordDecisionNeedsTheHookState asserts the decision is no for a line
// that was not parsed, and for each way the hook state says no.
func TestRecordDecisionNeedsTheHookState(t *testing.T) {
	root := newRoot(t)
	k := keptLine{line: parseLine(root, []string{"get", "cluster"}), parsed: true}
	_, ok := recordDecision(k, recordingState{on: true, dirExisted: true})
	assert.True(t, ok)
	for name, st := range map[string]recordingState{
		"off":          {dirExisted: true},
		"no directory": {on: true},
		"zero":         {},
	} {
		_, ok := recordDecision(k, st)
		assert.False(t, ok, name)
	}
	_, ok = recordDecision(keptLine{line: k.line}, recordingState{on: true, dirExisted: true})
	assert.False(t, ok, "no parse")
}

// TestRecordDecisionRefusesAGroupThatCarriesADescriptor asserts a line that
// ends at a command with subcommands records nothing even when that command
// has a descriptor: an unknown word after it, or no word.
func TestRecordDecisionRefusesAGroupThatCarriesADescriptor(t *testing.T) {
	leaf := &ucli.Command{Name: "leaf"}
	group := &ucli.Command{
		Name:     "grp",
		Metadata: leafMetadata(runtime.Descriptor{Name: "grp"}),
		Commands: []*ucli.Command{leaf},
	}
	root := &ucli.Command{Name: "rancher", Commands: []*ucli.Command{group}}
	on := recordingState{on: true, dirExisted: true}
	for _, argv := range [][]string{{"grp", "bogus"}, {"grp"}} {
		_, ok := recordDecision(keptLine{line: parseLine(root, argv), parsed: true}, on)
		assert.False(t, ok, argv)
	}
}

// TestRecordedRunsEndToEnd asserts a sample of lines, run through the built
// tree with recording on, store exactly one event when the command is not
// excluded and none when it is. Every row has a fresh config directory,
// home directory and editor; the commands that write outside the config
// directory are left to the per-command decision test.
func TestRecordedRunsEndToEnd(t *testing.T) {
	const unreachable = "https://127.0.0.1:1"
	rows := []struct {
		name   string
		args   []string
		record bool
	}{
		{"read-only engine command", []string{"get", "cluster"}, true},
		{"mutating engine command", []string{"create", "namespace", "ns1", "--cluster", "c-1"}, true},
		{"local command", []string{"config", "status"}, true},
		{"config path", []string{"config", "path"}, true},
		{"schema", []string{"schema"}, true},
		{"bare entry", []string{"token", "--server", unreachable, "--user", "kube", "--cluster", "c-1"}, true},
		{"auth get-token", []string{"auth", "get-token", "--server", unreachable, "--cluster", "c-1"}, true},
		{"auth login", []string{"auth", "login", "--url", unreachable}, true},
		{"auth logout", []string{"auth", "logout", "nope"}, true},
		{"auth delete-token", []string{"auth", "delete-token", "nope"}, true},
		{"auth status", []string{"auth", "status"}, true},
		{"config add-server", []string{"config", "add-server", "x", "--url", unreachable}, true},
		{"config update-server", []string{"config", "update-server", "nope"}, true},
		{"config delete-server", []string{"config", "delete-server", "nope"}, true},
		{"kubeconfig", []string{"kubeconfig"}, true},
		{"completion", []string{"completion", "bash"}, false},
		{"telemetry status", []string{"telemetry", "status"}, false},
		{"telemetry on", []string{"telemetry", "on"}, false},
		{"telemetry off", []string{"telemetry", "off"}, false},
		{"telemetry reset", []string{"telemetry", "reset"}, false},
		{"telemetry show", []string{"telemetry", "show"}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := recordingEnv(t)
			runRecorded(t, newRoot(t), row.args...)
			evs := storedRuns(t, dir)
			if !row.record {
				assert.Empty(t, evs)
				return
			}
			require.Len(t, evs, 1)
			assert.Equal(t, strings.Join(row.args[:commandWords(t, row.args)], " "), evs[0].Fields["io.rancher.cli.command"])
		})
	}
}

// commandWords returns how many leading words of args name the registered
// command.
func commandWords(t *testing.T, args []string) int {
	t.Helper()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	best := 0
	for _, c := range wr.reg.Commands() {
		words := strings.Fields(c.Describe().Name)
		if len(words) > best && len(words) <= len(args) && slices.Equal(words, args[:len(words)]) {
			best = len(words)
		}
	}
	require.NotZero(t, best, args)
	return best
}

// outputSweepRow is one line of the sweep that covers every exit path.
type outputSweepRow struct {
	name string
	args []string
	// config, when set, is written as config.yaml before the run.
	config string
	// records is true when a run with recording on stores an event.
	records bool
}

// TestRecordingChangesNoOutputAndNoExitCode asserts that every exit path of
// the exit table gives the same stdout, stderr and exit code with recording
// on, with recording on and a regular file where the telemetry directory
// belongs (the store refuses it), and with recording off.
func TestRecordingChangesNoOutputAndNoExitCode(t *testing.T) {
	rows := []outputSweepRow{
		{name: "success", args: []string{"config", "status"}, records: true},
		{name: "declared status", args: []string{"skill", "status", "--agent", "claude", "--require-installed"}, records: true},
		{name: "typed error", args: []string{"get", "cluster", "-o", "json"}, records: true},
		{name: "validation after the command is chosen", args: []string{"get", "cluster", "a", "b", "-o", "bad"}, records: true},
		{name: "unknown command", args: []string{"bogus"}},
		{name: "unknown command in a group", args: []string{"config", "bogus"}},
		{name: "flag error", args: []string{"get", "cluster", "--nope"}},
		{name: "pre-parse refusal", args: []string{"get", "cluster", "-", "x"}},
		{name: "help", args: []string{"get", "cluster", "--help"}},
		{name: "version", args: []string{"--version"}},
		{name: "group with no word", args: []string{"config"}},
		{name: "completion request", args: []string{protoSentinel, "get", "cluster", ""}},
		{name: "completion script", args: []string{"completion", "bash"}},
		{name: "unloadable config", args: []string{"config", "status"}, config: "servers: [\n", records: true},
	}
	type mode struct {
		name     string
		variable string
		refuse   bool
	}
	modes := []mode{
		{name: "off", variable: "false"},
		{name: "on", variable: "true"},
		{name: "on with a file where the directory belongs", variable: "true", refuse: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := recordingEnv(t)
			var outcomes []recordedRun
			for _, m := range modes {
				require.NoError(t, os.RemoveAll(dir))
				require.NoError(t, os.MkdirAll(dir, 0o700))
				if row.config != "" {
					writeConfig(t, dir, row.config)
				}
				if m.refuse {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry"), nil, 0o600))
				}
				t.Setenv(telemetry.EnvTelemetry, m.variable)
				outcomes = append(outcomes, runRecorded(t, newRoot(t), row.args...))
				if m.refuse {
					continue
				}
				want := 0
				if m.variable == "true" && row.records {
					want = 1
				}
				assert.Len(t, storedRuns(t, dir), want, m.name)
			}
			for i, got := range outcomes[1:] {
				assert.Equal(t, outcomes[0].stdout, got.stdout, modes[i+1].name)
				assert.Equal(t, outcomes[0].stderr, got.stderr, modes[i+1].name)
				assert.Equal(t, outcomes[0].res, got.res, modes[i+1].name)
			}
		})
	}
}

// TestRecordedExitCodeIsTheCodeRunReturns asserts the stored exit code is the
// code run returns, with the outcome and the error type of the error that
// ended the run: a success, a status the command declares, a typed error, a
// refusal after the command is chosen, the bare entry succeeding from a
// cached credential and failing without one, and an interrupt.
func TestRecordedExitCodeIsTheCodeRunReturns(t *testing.T) {
	check := func(t *testing.T, dir string, got recordedRun, wantCode int, wantType string) {
		t.Helper()
		evs := storedRuns(t, dir)
		require.Len(t, evs, 1)
		assert.Equal(t, wantCode, got.res.code)
		assert.EqualValues(t, got.res.code, evs[0].Fields["process.exit.code"])
		if got.res.err == nil {
			assert.Equal(t, "ok", evs[0].Fields["io.rancher.cli.command.outcome"])
			assert.NotContains(t, evs[0].Fields, "error.type")
			return
		}
		assert.Equal(t, "error", evs[0].Fields["io.rancher.cli.command.outcome"])
		assert.Equal(t, got.res.err.Code, evs[0].Fields["error.type"])
		assert.Equal(t, wantType, got.res.err.Code)
	}
	t.Run("success", func(t *testing.T) {
		dir := recordingEnv(t)
		got := runRecorded(t, newRoot(t), "config", "status")
		check(t, dir, got, 0, "")
	})
	t.Run("declared status", func(t *testing.T) {
		dir := recordingEnv(t)
		got := runRecorded(t, newRoot(t), "skill", "status", "--agent", "claude", "--require-installed")
		require.Equal(t, 7, got.res.code, got.stderr)
		require.Nil(t, got.res.err)
		check(t, dir, got, 7, "")
		assert.Equal(t, "ok", storedRuns(t, dir)[0].Fields["io.rancher.cli.command.outcome"])
	})
	t.Run("typed error", func(t *testing.T) {
		dir := recordingEnv(t)
		got := runRecorded(t, newRoot(t), "get", "cluster", "-o", "json")
		check(t, dir, got, 2, clierr.CodeNotLoggedIn)
	})
	t.Run("validation after the command is chosen", func(t *testing.T) {
		dir := recordingEnv(t)
		got := runRecorded(t, newRoot(t), "get", "cluster", "a", "b", "-o", "bad")
		check(t, dir, got, 2, clierr.CodeValidation)
	})
	t.Run("bare entry failing", func(t *testing.T) {
		dir := recordingEnv(t)
		got := runRecorded(t, newRoot(t), "token", "--server", "https://127.0.0.1:1", "--user", "kube", "--cluster", "c-1")
		require.NotZero(t, got.res.code)
		check(t, dir, got, got.res.code, got.res.err.Code)
	})
	t.Run("bare entry succeeding from a cached credential", func(t *testing.T) {
		dir := recordingEnv(t)
		seedCachedHit(t, dir, "https://r.ex")
		got := runRecorded(t, newRoot(t), "token", "--server", "https://r.ex", "--user", "kube", "--cluster", "c-1")
		require.Zero(t, got.res.code, got.stderr)
		assert.Contains(t, got.stdout, "ExecCredential")
		check(t, dir, got, 0, "")
	})
	t.Run("interrupted", func(t *testing.T) {
		dir := recordingEnv(t)
		arrived := make(chan struct{})
		var once sync.Once
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(arrived) })
			testtransport.HangAndAbort(r)
		}))
		t.Cleanup(srv.Close)
		writeConfig(t, dir, serverConfig("prod", map[string]string{"prod": srv.URL}))
		t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
		t.Setenv(config.EnvToken, "token-x:y")
		ctx, cancel := contextWithCancel(t)
		go func() {
			select {
			case <-arrived:
				cancel()
			case <-time.After(30 * time.Second):
				cancel()
			}
		}()
		got := runWithContext(t, ctx, newRoot(t), "get", "cluster", "-o", "json")
		require.NotNil(t, got.res.err, got.stderr)
		assert.Equal(t, clierr.CodeInterrupted, got.res.err.Code, got.stderr)
		assert.Equal(t, 130, got.res.code)
		check(t, dir, got, 130, clierr.CodeInterrupted)
	})
}
