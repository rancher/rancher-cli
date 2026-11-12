package telemetrycmd

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

const (
	releaseVersion     = "v1.0.0"
	developmentVersion = "dev-abc123"

	releaseSendingLine     = "sending: none - this build sends no event; recorded events stay on this machine"
	developmentSendingLine = "sending: none - no upload endpoint is set; recorded events stay on this machine"
)

// state names for the tables below.
const (
	stateMissing  = "missing directory"
	stateEmpty    = "empty directory"
	stateSeeded   = "events, facts and an id"
	stateNoIDFile = "events and no id file"
)

// prepareState builds the config directory state under dir, with the config
// file text when it is not empty. The missing state cannot hold a config
// file.
func prepareState(t *testing.T, dir, state, configText string) {
	t.Helper()
	switch state {
	case stateMissing:
		require.Empty(t, configText, "a missing directory holds no config file")
		return
	case stateEmpty:
		require.NoError(t, os.MkdirAll(dir, 0o700))
	case stateSeeded:
		seedStore(t, dir)
	case stateNoIDFile:
		seedStore(t, dir)
		require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", "config-directory-id")))
	}
	if configText != "" {
		writeConfig(t, dir, configText)
	}
}

var allStates = []string{stateMissing, stateEmpty, stateSeeded, stateNoIDFile}

// controlCase arranges the environment and config text so one control
// decides.
type controlCase struct {
	name       string
	want       telemetry.Control
	wantOn     bool
	configText string
	arrange    func(t *testing.T)
}

func controlCases() []controlCase {
	return []controlCase{
		{name: "test binary", want: telemetry.ControlTestBinary, wantOn: false, arrange: func(t *testing.T) {
			t.Helper()
			t.Setenv(telemetry.EnvTelemetry, "")
		}},
		{name: "variable", want: telemetry.ControlVariable, wantOn: false, arrange: func(t *testing.T) {
			t.Helper()
			controlsEnv(t)
			t.Setenv(telemetry.EnvTelemetry, "false")
		}},
		{name: "config", want: telemetry.ControlConfig, wantOn: false, configText: "telemetry: \"false\"\n", arrange: controlsEnv},
		{name: "default", want: telemetry.ControlDefault, wantOn: true, arrange: controlsEnv},
	}
}

// TestStatusCreatesNothingInAnyState asserts status and show leave the tree
// (names, sizes, contents, modification times) as it was, in every state of
// the config directory under every deciding control, and that a missing
// directory stays missing. A config file needs its directory, so the config
// control has no missing-directory case.
func TestStatusCreatesNothingInAnyState(t *testing.T) {
	for _, c := range controlCases() {
		for _, state := range allStates {
			if state == stateMissing && c.configText != "" {
				continue
			}
			t.Run(c.name+"/"+state, func(t *testing.T) {
				c.arrange(t)
				dir := newDir(t)
				prepareState(t, dir, state, c.configText)
				before := snapshot(t, dir)
				for _, cmd := range []string{"status", "show"} {
					if cmd == "status" {
						_, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
						require.NoError(t, err)
					} else {
						_, err := execute(t, NewShow(dir))
						require.NoError(t, err)
					}
					assert.Equal(t, before, snapshot(t, dir), cmd)
				}
			})
		}
	}
}

// TestStatusNamesEachControl asserts status names the deciding control for
// each of the four Control values and the state that follows from it.
func TestStatusNamesEachControl(t *testing.T) {
	require.ElementsMatch(t, telemetry.Controls(), func() []telemetry.Control {
		var all []telemetry.Control
		for _, c := range controlCases() {
			all = append(all, c.want)
		}
		return all
	}(), "the table covers every control")
	for _, c := range controlCases() {
		t.Run(c.name, func(t *testing.T) {
			c.arrange(t)
			dir := newDir(t)
			prepareState(t, dir, stateEmpty, c.configText)
			res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
			require.NoError(t, err)
			d := dataMap(t, res)
			assert.Equal(t, string(c.want), d["decidedBy"])
			wantState := "off"
			if c.wantOn {
				wantState = "on"
			}
			assert.Equal(t, wantState, d["state"])
			assert.Equal(t, "TelemetryStatus", res.Kind)
		})
	}
}

// TestStatusReportsWhatIsRecorded asserts status counts, the contract
// version, the directory and the id over a seeded store.
func TestStatusReportsWhatIsRecorded(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	seedStore(t, dir)
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	d := dataMap(t, res)
	assert.Equal(t, "on", d["state"])
	assert.Equal(t, "default", d["decidedBy"])
	assert.Equal(t, dir, d["configDirectory"])
	assert.Equal(t, true, d["configDirectoryExists"])
	assert.Equal(t, 2, d["events"])
	assert.Equal(t, false, d["eventsLimitReached"])
	assert.Equal(t, 0, d["droppedEvents"])
	assert.Equal(t, telemetry.ContractVersion, d["contractVersion"])
	assert.Equal(t, "11111111-1111-4111-8111-111111111111", d["configDirectoryId"])
	assert.NotContains(t, d, "configKey")
	assert.Empty(t, res.Warnings)
}

// TestStatusWithoutAnIDOmitsIt asserts the id is left out when the file is
// missing, and read without creating it while recording is off.
func TestStatusWithoutAnIDOmitsIt(t *testing.T) {
	controlsEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "false")
	dir := newDir(t)
	seedStore(t, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, "telemetry", "config-directory-id")))
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	assert.NotContains(t, dataMap(t, res), "configDirectoryId")

	putFile(t, dir, "telemetry/config-directory-id", "22222222-2222-4222-8222-222222222222")
	res, err = execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	d := dataMap(t, res)
	assert.Equal(t, "off", d["state"])
	assert.Equal(t, "22222222-2222-4222-8222-222222222222", d["configDirectoryId"], "the id shows whatever the controls say")
}

// TestStatusSaysNothingIsRecordedWithoutADirectory asserts the text when
// recording is on and the config directory does not exist.
func TestStatusSaysNothingIsRecordedWithoutADirectory(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	assert.Contains(t, res.Text, "recording is on; nothing is recorded because the config directory does not exist")
	assert.Contains(t, res.Text, "does not exist)")
	assert.Equal(t, false, dataMap(t, res)["configDirectoryExists"])
	assert.NoDirExists(t, dir)
}

// TestStatusConfigKeyCases asserts configKey is the text as loaded, a Go
// string, omitted when the key is absent; an invalid key gives state on, the
// default, exit 0 and the warning.
func TestStatusConfigKeyCases(t *testing.T) {
	for _, tc := range []struct {
		name       string
		yaml       string
		key        any
		state      string
		decidedBy  string
		wantInText string
		warns      bool
	}{
		{name: "true", yaml: `telemetry: "true"`, key: "true", state: "on", decidedBy: "config", wantInText: `telemetry: "true"`},
		{name: "false", yaml: `telemetry: "false"`, key: "false", state: "off", decidedBy: "config", wantInText: `telemetry: "false"`},
		{name: "unquoted off", yaml: `telemetry: off`, key: "false", state: "off", decidedBy: "config", wantInText: `telemetry: "false"`},
		{name: "invalid", yaml: `telemetry: maybe`, key: "maybe", state: "on", decidedBy: "default", wantInText: `telemetry: "maybe"`, warns: true},
		{name: "space and newline", yaml: "telemetry: \"a b\\nc\"", key: "a b\nc", state: "on", decidedBy: "default", wantInText: `telemetry: "a b\nc"`, warns: true},
		{name: "empty", yaml: `telemetry: ""`, key: "", state: "on", decidedBy: "default", wantInText: `telemetry: ""`, warns: true},
		{name: "absent", yaml: `currentServer: ""`, state: "on", decidedBy: "default"},
		{name: "null", yaml: `telemetry:`, state: "on", decidedBy: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			writeConfig(t, dir, tc.yaml+"\n")
			res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
			require.NoError(t, err)
			d := dataMap(t, res)
			assert.Equal(t, tc.state, d["state"])
			assert.Equal(t, tc.decidedBy, d["decidedBy"])
			if tc.key == nil {
				assert.NotContains(t, d, "configKey")
			} else {
				text, ok := d["configKey"].(string)
				require.True(t, ok, "configKey is %T", d["configKey"])
				assert.Equal(t, tc.key, text)
			}
			if tc.wantInText != "" {
				assert.Contains(t, res.Text, tc.wantInText)
			}
			if tc.warns {
				require.Len(t, res.Warnings, 1)
				assert.Contains(t, res.Warnings[0], "is "+quoted(tc.key.(string))+" and is ignored; only true or false are valid")
				assert.NotContains(t, res.Warnings[0], "\n", "the value is quoted, so a newline stays on one line")
			} else {
				assert.Empty(t, res.Warnings)
			}
		})
	}
}

// TestStatusWarnsForAnInvalidKeyWhateverControlDecides asserts the warning
// names the invalid key when the variable decides too.
func TestStatusWarnsForAnInvalidKeyWhateverControlDecides(t *testing.T) {
	controlsEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "false")
	dir := newDir(t)
	writeConfig(t, dir, "telemetry: maybe\n")
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	assert.Equal(t, telemetry.EnvTelemetry, dataMap(t, res)["decidedBy"])
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], `"maybe"`)
}

// TestStatusVariableValues asserts the variable's accepted and ignored
// values: true and false decide; on, off, 1, FALSE, yes and values with a
// space and a newline are ignored with a warning naming the value; empty is
// unset with no warning.
func TestStatusVariableValues(t *testing.T) {
	for _, tc := range []struct {
		value, state, decidedBy string
		warns                   bool
	}{
		{value: "true", state: "on", decidedBy: "RANCHER_CLI_TELEMETRY"},
		{value: "false", state: "off", decidedBy: "RANCHER_CLI_TELEMETRY"},
		{value: "", state: "on", decidedBy: "default"},
		{value: "on", state: "on", decidedBy: "default", warns: true},
		{value: "off", state: "on", decidedBy: "default", warns: true},
		{value: "1", state: "on", decidedBy: "default", warns: true},
		{value: "FALSE", state: "on", decidedBy: "default", warns: true},
		{value: "yes", state: "on", decidedBy: "default", warns: true},
		{value: "a b\nc", state: "on", decidedBy: "default", warns: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			controlsEnv(t)
			t.Setenv(telemetry.EnvTelemetry, tc.value)
			dir := newDir(t)
			writeConfig(t, dir, "currentServer: \"\"\n")
			res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
			require.NoError(t, err)
			d := dataMap(t, res)
			assert.Equal(t, tc.state, d["state"])
			assert.Equal(t, tc.decidedBy, d["decidedBy"])
			if !tc.warns {
				assert.Empty(t, res.Warnings)
				return
			}
			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], "RANCHER_CLI_TELEMETRY is set to "+quoted(tc.value)+" and is ignored; only true or false are valid")
		})
	}
}

func quoted(s string) string { return strconv.Quote(s) }

// TestStatusSettingFollowsTheKeyUnderAnIgnoredVariable asserts an ignored
// variable lets the key decide.
func TestStatusSettingFollowsTheKeyUnderAnIgnoredVariable(t *testing.T) {
	controlsEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "on")
	dir := newDir(t)
	writeConfig(t, dir, "telemetry: \"false\"\n")
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	d := dataMap(t, res)
	assert.Equal(t, "off", d["state"])
	assert.Equal(t, "config", d["decidedBy"])
	require.Len(t, res.Warnings, 1)
}

// TestStatusCountsTheLimit asserts the limit flag when the count reaches the
// display limit, from one compacted hour of that many events, and that the
// count stops there.
func TestStatusCountsTheLimit(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	hour := "2026102010"
	var lines strings.Builder
	for n := 1; n <= telemetry.CountLimit+5; n++ {
		lines.WriteString(eventJSON(n, "2026-10-20T10:00:00Z", "get project", "ok", 0) + "\n")
	}
	putFile(t, dir, "telemetry/events/"+hour+"/"+hour+".jsonl", lines.String())
	res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
	require.NoError(t, err)
	d := dataMap(t, res)
	assert.Equal(t, telemetry.CountLimit, d["events"])
	assert.Equal(t, true, d["eventsLimitReached"])
	assert.Contains(t, res.Text, "recorded events: 10000 (counting stops at 10000)")
}

// TestStatusWarnsForAnUnsafeOrUnreadableTelemetryPath asserts status warns,
// naming the path, and exits 0 over a telemetry symlink and over an
// unreadable events directory; the symlink target is untouched.
func TestStatusWarnsForAnUnsafeOrUnreadableTelemetryPath(t *testing.T) {
	skipOnWindows(t, "symlinks and directory modes differ")
	for _, which := range []string{"symlink", "unreadable"} {
		t.Run(which, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			var outside string
			if which == "symlink" {
				outside = t.TempDir()
				putFile(t, outside, "events/2026102010/0001.json", eventJSON(1, "2026-10-20T10:00:00Z", "get project", "ok", 0))
				if err := os.Symlink(outside, filepath.Join(dir, "telemetry")); err != nil {
					t.Skipf("symlinks are not available: %v", err)
				}
			} else {
				if os.Geteuid() == 0 {
					t.Skip("root reads any directory")
				}
				seedStore(t, dir)
				events := filepath.Join(dir, "telemetry", "events")
				require.NoError(t, os.Chmod(events, 0))
				t.Cleanup(func() { _ = os.Chmod(events, 0o700) })
			}
			var before []string
			if outside != "" {
				before = snapshot(t, outside)
			}
			for _, run := range []func() ([]string, error){
				func() ([]string, error) {
					res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
					return res.Warnings, err
				},
				func() ([]string, error) { res, err := execute(t, NewShow(dir)); return res.Warnings, err },
			} {
				warnings, err := run()
				require.NoError(t, err)
				require.Len(t, warnings, 1)
				assert.Contains(t, warnings[0], filepath.Join(dir, "telemetry"))
			}
			if outside != "" {
				assert.Equal(t, before, snapshot(t, outside))
			}
		})
	}
}

// TestStatusTextLines asserts the whole text of `telemetry status` for a
// release build, with a store with events and an id, with the key off, with no
// config directory and with the variable off.
func TestStatusTextLines(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	counts := func(dir string) string {
		return "  config directory: " + dir + " (exists)\n" +
			"  recorded events: 2\n  dropped events: 0\n  contract version: " + telemetry.ContractVersion +
			"\n  config directory id: " + id + "\n  " + releaseSendingLine + "\n"
	}
	for _, tc := range []struct {
		name     string
		seed     bool
		config   string
		variable string
		want     func(dir string) string
	}{
		{
			name: "default with events",
			seed: true,
			want: func(dir string) string { return "recording is on\n  decided by: default\n" + counts(dir) },
		},
		{
			name:   "key off",
			seed:   true,
			config: "telemetry: \"false\"\n",
			want: func(dir string) string {
				return "recording is off\n  decided by: config\n  key in config.yaml: telemetry: \"false\"\n" + counts(dir)
			},
		},
		{
			name: "missing directory",
			want: func(dir string) string {
				return "recording is on; nothing is recorded because the config directory does not exist\n  decided by: default\n  config directory: " +
					dir + " (does not exist)\n  recorded events: 0\n  dropped events: 0\n  contract version: " + telemetry.ContractVersion +
					"\n  " + releaseSendingLine + "\n"
			},
		},
		{
			name:     "variable off",
			seed:     true,
			variable: "false",
			want: func(dir string) string {
				return "recording is off\n  decided by: RANCHER_CLI_TELEMETRY\n" + counts(dir)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlsEnv(t)
			dir := newDir(t)
			if tc.seed {
				seedStore(t, dir)
			}
			if tc.config != "" {
				writeConfig(t, dir, tc.config)
			}
			if tc.variable != "" {
				t.Setenv(telemetry.EnvTelemetry, tc.variable)
			}
			res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
			require.NoError(t, err)
			assert.Equal(t, tc.want(dir), res.Text)
		})
	}
}

// TestStatusListsExperimentsAndNoAttemptYet asserts the status text lines and
// data keys of an active experiment with and without an endpoint, that the
// zero view adds nothing to the text or the data, and that the view's warnings
// are in the result once.
func TestStatusListsExperimentsAndNoAttemptYet(t *testing.T) {
	experimentKeys := []string{"experiments", "uploadEndpoint", "uploadLastAttempt"}

	t.Run("zero view", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{}, releaseVersion))
		require.NoError(t, err)
		assert.NotContains(t, res.Text, "experiment")
		assert.NotContains(t, res.Text, "upload")
		data := dataMap(t, res)
		for _, k := range experimentKeys {
			assert.NotContains(t, data, k)
		}
		assert.Empty(t, res.Warnings)
	})

	t.Run("endpoint set", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		view := telemetry.ExperimentView{
			Names:    []string{"telemetryupload"},
			Endpoint: "https://h.example",
			Warnings: []string{"RANCHER_CLI_EXPERIMENT: unknown experiment b ignored"},
		}
		res, err := execute(t, NewStatus(dir, view, developmentVersion))
		require.NoError(t, err)
		assert.True(t, strings.HasSuffix(res.Text, "  experiments: telemetryupload\n"+
			"  upload endpoint: https://h.example\n"+
			"  last upload attempt: no attempt yet\n"+
			"  next upload attempt: at the next recorded run\n"+
			"  events waiting: 0\n"+
			"  events of the current hour: 0 (sent after the hour ends)\n"), res.Text)
		assert.NotContains(t, res.Text, "nothing is sent")
		data := dataMap(t, res)
		assert.Equal(t, []string{"telemetryupload"}, data["experiments"])
		assert.Equal(t, "https://h.example", data["uploadEndpoint"])
		assert.Contains(t, data, "uploadLastAttempt")
		assert.Nil(t, data["uploadLastAttempt"])
		assert.Equal(t, []string{"RANCHER_CLI_EXPERIMENT: unknown experiment b ignored"}, res.Warnings)
	})

	t.Run("no endpoint", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{Names: []string{"telemetryupload"}}, developmentVersion))
		require.NoError(t, err)
		assert.Contains(t, res.Text, "  upload endpoint: not set\n")
		data := dataMap(t, res)
		assert.Equal(t, []string{"telemetryupload"}, data["experiments"])
		assert.NotContains(t, data, "uploadEndpoint")
		assert.Nil(t, data["uploadLastAttempt"])
	})

	t.Run("warnings without an active experiment", func(t *testing.T) {
		controlsEnv(t)
		dir := newDir(t)
		line := "RANCHER_CLI_EXPERIMENT: unknown experiment b ignored"
		res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{Warnings: []string{line}}, developmentVersion))
		require.NoError(t, err)
		assert.NotContains(t, res.Text, "experiments:")
		assert.Equal(t, []string{line}, res.Warnings)
	})

	t.Run("recording off", func(t *testing.T) {
		controlsEnv(t)
		t.Setenv(telemetry.EnvTelemetry, "false")
		dir := newDir(t)
		res, err := execute(t, NewStatus(dir, telemetry.ExperimentView{Names: []string{"telemetryupload"}, Endpoint: "https://h.example"}, developmentVersion))
		require.NoError(t, err)
		assert.Contains(t, res.Text, "recording is off")
		assert.Contains(t, res.Text, "  upload endpoint: https://h.example\n")
	})
}

// TestStatusSendsEventsRows asserts, for each build kind, view and recording
// state, the sending line (or its absence) and the decoded sendsEvents value.
// A release build prints the release line and reports false whatever the view
// holds, including an endpoint with recording on.
func TestStatusSendsEventsRows(t *testing.T) {
	endpoint := telemetry.ExperimentView{Names: []string{"telemetryupload"}, Endpoint: "https://h.example"}
	for _, tc := range []struct {
		name     string
		version  string
		view     telemetry.ExperimentView
		off      bool
		wantLine string
		want     bool
	}{
		{"release with an empty view", releaseVersion, telemetry.ExperimentView{}, false, releaseSendingLine, false},
		{"release with an endpoint and recording on", releaseVersion, endpoint, false, releaseSendingLine, false},
		{"development with an empty view", developmentVersion, telemetry.ExperimentView{}, false, developmentSendingLine, false},
		{"development with telemetryupload and no value", developmentVersion,
			telemetry.ExperimentView{Names: []string{"telemetryupload"}}, false, developmentSendingLine, false},
		{"development with a refused value", developmentVersion,
			telemetry.ExperimentView{Names: []string{"telemetryupload"}, EndpointProblem: "the port is 0"}, false, developmentSendingLine, false},
		{"development with an endpoint and recording on", developmentVersion, endpoint, false, "", true},
		{"development with an endpoint and recording off", developmentVersion, endpoint, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlsEnv(t)
			if tc.off {
				t.Setenv(telemetry.EnvTelemetry, "false")
			}
			dir := newDir(t)
			res, err := execute(t, NewStatus(dir, tc.view, tc.version))
			require.NoError(t, err)
			if tc.wantLine == "" {
				assert.NotContains(t, res.Text, "sending:")
			} else {
				assert.Equal(t, 1, strings.Count(res.Text, "sending:"))
				assert.Contains(t, res.Text, "  "+tc.wantLine+"\n")
			}
			data := dataMap(t, res)
			require.Contains(t, data, "sendsEvents")
			assert.Equal(t, tc.want, data["sendsEvents"])
		})
	}
}
