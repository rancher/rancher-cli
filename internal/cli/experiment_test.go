package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/experiment"
	"github.com/rancher/rancher-cli/internal/invocation"
)

const (
	warningPrefix = experiment.EnvVar + ": "
	schemeReason  = "it must use https; http is accepted only for localhost, ::1 and 127.0.0.0/8"
	queryReason   = "it holds a query or fragment"
	pathReason    = "it holds a path; give only scheme://host[:port]"
	noEndpoint    = "telemetryupload has no endpoint; set telemetryupload=<url>"
	userReason    = "it holds a user name or password"
	portReason    = "it holds a port that is not a number from 1 to 65535"
	hostReason    = "it holds a host that is not an IP address or a DNS name"
	ipReason      = "it holds a host that is not a valid IP address"
	asciiReason   = "it holds a host with non-ASCII characters; write the host in its ASCII (xn--) form"
)

// unrecognized is the warning for an unrecognized item at position n.
func unrecognized(n int) string {
	return warningPrefix + "ignored an unrecognized item (item " + strconv.Itoa(n) + ")"
}

var releaseVersions = []string{"1.2.3", "0.1.0-alpha.8", "0.1.0-rc.1", "1.2.3-hotfix-x", "v0.1.0", "unknown", "_OTHER"}

var developmentVersions = []string{"dev", "dev-abc123", "dev-unknown"}

// experimentEnv makes the process environment hermetic for a run of the
// built tree: temporary home and config directories, the file credential
// store, no server, no token, no exec plugin and no experiment variable, and
// a counter of the reads of the experiment variable. It restores everything
// when the test ends. Not for a parallel test.
func experimentEnv(t *testing.T) (reads *int) {
	t.Helper()
	restoreRawArgv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	for _, name := range []string{config.EnvURL, config.EnvToken, config.EnvReadOnly, "KUBERNETES_EXEC_INFO"} {
		t.Setenv(name, "")
	}
	t.Setenv(experiment.EnvVar, "")
	t.Chdir(t.TempDir())
	t.Cleanup(SetInvocation(invocation.Context{}))
	reads = new(int)
	orig := experimentGetenv
	experimentGetenv = func(key string) string {
		if key == experiment.EnvVar {
			*reads++
		}
		return os.Getenv(key)
	}
	t.Cleanup(func() { experimentGetenv = orig })
	return reads
}

// setExperiment sets the variable for the test, or removes it when ok is
// false.
func setExperiment(t *testing.T, value string, ok bool) {
	t.Helper()
	t.Setenv(experiment.EnvVar, value)
	if !ok {
		require.NoError(t, os.Unsetenv(experiment.EnvVar))
	}
}

type experimentRun struct {
	stdout, stderr string
	code           int
}

func (r experimentRun) combined() string { return r.stdout + r.stderr }

// runBuilt builds a root with the version and runs argv on it through Run, as
// the process does.
func runBuilt(t *testing.T, version string, argv ...string) experimentRun {
	t.Helper()
	root, err := BuildRoot(version)
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader("")
	code := Run(t.Context(), root, append([]string{"rancher"}, argv...))
	return experimentRun{stdout: out.String(), stderr: errOut.String(), code: code}
}

type statusEnvelope struct {
	Data     map[string]any `json:"data"`
	Warnings []string       `json:"warnings"`
}

func decodeStatusEnvelope(t *testing.T, stdout string) statusEnvelope {
	t.Helper()
	var e statusEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &e), stdout)
	return e
}

// experimentWarnings returns the lines of warnings that come from the
// experiment variable.
func experimentWarnings(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, warningPrefix) {
			out = append(out, l)
		}
	}
	return out
}

// statusOf runs `telemetry status -o json` and returns its data and the
// experiment warnings of its envelope.
func statusOf(t *testing.T, version string) (data map[string]any, warnings []string, run experimentRun) {
	t.Helper()
	run = runBuilt(t, version, "telemetry", "status", "-o", "json")
	require.Equal(t, 0, run.code, run.stderr)
	e := decodeStatusEnvelope(t, run.stdout)
	return e.Data, experimentWarnings(e.Warnings), run
}

func experimentsOf(data map[string]any) []string {
	raw, _ := data["experiments"].([]any)
	var out []string
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// TestExperimentParse asserts, through Run on a development build, that the
// last item of a name decides, that an invalid last item leaves the
// experiment off, that upper case and spaces in a name warn with the item's
// position, that empty items are skipped but counted, and that an unrecognized
// item is never echoed.
func TestExperimentParse(t *testing.T) {
	experimentEnv(t)
	for _, tc := range []struct {
		name     string
		value    string
		endpoint string
		warnings []string
	}{
		{"last repeat wins", "telemetryupload=https://a.example,telemetryupload=https://b.example", "https://b.example", nil},
		{"an invalid earlier repeat is not checked", "telemetryupload=http://a.example,telemetryupload=https://b.example", "https://b.example", nil},
		{"an invalid last repeat leaves it off", "telemetryupload=https://a.example,telemetryupload=http://b.example", "",
			[]string{warningPrefix + "telemetryupload ignored: " + schemeReason}},
		{"upper case and a space warn", "Telemetryupload=https://a.example, telemetryupload=https://b.example", "", []string{
			unrecognized(1),
			unrecognized(2),
		}},
		{"empty items are skipped and counted", ",,bogus,,telemetryupload=https://a.example", "https://a.example",
			[]string{warningPrefix + "unknown experiment bogus ignored"}},
		{"a colon after the name", "telemetryupload:https://qzxuser:s3cr3t@h.example", "",
			[]string{unrecognized(1)}},
		{"a space after the name", "telemetryupload https://qzxuser:s3cr3t@h.example", "",
			[]string{unrecognized(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setExperiment(t, tc.value, true)
			data, warnings, run := statusOf(t, "dev")
			assert.Equal(t, tc.warnings, warnings)
			if tc.endpoint == "" {
				assert.NotContains(t, data, "uploadEndpoint")
				assert.Empty(t, experimentsOf(data))
			} else {
				assert.Equal(t, tc.endpoint, data["uploadEndpoint"])
			}
			text := runBuilt(t, "dev", "telemetry", "status")
			assert.Equal(t, 0, text.code, text.stderr)
			for _, out := range []string{run.combined(), text.combined()} {
				assert.NotContains(t, out, "qzxuser")
				assert.NotContains(t, out, "s3cr3t")
			}
		})
	}
}

// TestUploadURLRules asserts, through Run on a development build, that each
// refused value warns once with its fixed reason and leaves the experiment
// off, that a bare name and an empty value warn once and leave the experiment
// on with no endpoint, that a query with a comma gives two warnings, that no
// output holds a user name or password, and that the accepted values list
// their canonical origin; and that, with events waiting, a recorded run posts
// to the canonical origin and the encoder's path for a loopback address with
// and without a trailing slash, localhost, ::1 and 127.0.0.2.
func TestUploadURLRules(t *testing.T) {
	experimentEnv(t)
	refused := func(reason string) []string { return []string{warningPrefix + "telemetryupload ignored: " + reason} }
	for _, tc := range []struct {
		value    string
		warnings []string
	}{
		{"telemetryupload=http://h.example", refused(schemeReason)},
		{"telemetryupload=https://alice@h.example", refused(userReason)},
		{"telemetryupload=https://alice:s3cr3t@h.example", refused(userReason)},
		{"telemetryupload=https://h.example/?a=1", refused(queryReason)},
		{"telemetryupload=https://h.example/#f", refused(queryReason)},
		{"telemetryupload=https://h.example/?", refused(queryReason)},
		{"telemetryupload=https://h.example/other", refused(pathReason)},
		{"telemetryupload=https://h.example/v1/logs", refused(pathReason)},
		{"telemetryupload=https://h.example:", refused(portReason)},
		{"telemetryupload=https://h.example:0", refused(portReason)},
		{"telemetryupload=https://h.example:65536", refused(portReason)},
		{"telemetryupload=https://h.example:99999", refused(portReason)},
		{"telemetryupload=https://h.example;x", refused(hostReason)},
		{"telemetryupload=https://256.1.1.1", refused(ipReason)},
		{"telemetryupload=https://1.2.3", refused(ipReason)},
		{"telemetryupload=https://1.2.3.4.", refused(hostReason)},
		{"telemetryupload=https://-h.example", refused(hostReason)},
		{"telemetryupload=https://b\u00fccher.example", refused(asciiReason)},
		{"telemetryupload=http://[::ffff:127.0.0.1]", refused(schemeReason)},
		{"telemetryupload=http://localhost.", refused(schemeReason)},
		{"telemetryupload=https://h.example/?a=1,b=2", []string{
			warningPrefix + "telemetryupload ignored: " + queryReason,
			warningPrefix + "unknown experiment b ignored",
		}},
	} {
		t.Run(tc.value, func(t *testing.T) {
			setExperiment(t, tc.value, true)
			data, warnings, run := statusOf(t, "dev")
			assert.Equal(t, tc.warnings, warnings)
			assert.Empty(t, experimentsOf(data))
			assert.NotContains(t, data, "uploadEndpoint")
			assert.NotContains(t, run.combined(), "alice")
			assert.NotContains(t, run.combined(), "s3cr3t")
		})
	}

	for _, value := range []string{"telemetryupload", "telemetryupload="} {
		t.Run(value, func(t *testing.T) {
			setExperiment(t, value, true)
			data, warnings, _ := statusOf(t, "dev")
			assert.Equal(t, []string{warningPrefix + noEndpoint}, warnings)
			assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
			assert.NotContains(t, data, "uploadEndpoint")
		})
	}

	urlRequestRows(t)

	for value, want := range map[string]string{
		"https://h.example":         "https://h.example",
		"https://h.example/":        "https://h.example",
		"https://H.EXAMPLE:443/":    "https://h.example",
		"https://h.example:0443":    "https://h.example",
		"https://h.example:00080":   "https://h.example:80",
		"http://localhost:80":       "http://localhost",
		"http://localhost:8080":     "http://localhost:8080",
		"http://[::1]:8080":         "http://[::1]:8080",
		"http://127.0.0.2":          "http://127.0.0.2",
		"https://telemetry.example": "https://telemetry.example",
	} {
		t.Run(value, func(t *testing.T) {
			setExperiment(t, "telemetryupload="+value, true)
			data, warnings, _ := statusOf(t, "dev")
			assert.Empty(t, warnings)
			assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
			assert.Equal(t, want, data["uploadEndpoint"])
		})
	}
}

// TestReleaseBuildsIgnoreExperiments asserts that for each release version,
// with the variable set to a valid endpoint, a bare name and garbage, there is
// no warning on stderr or in warnings, status shows no experiment line and no
// experiments key, and the variable is not read; and that with events waiting
// and a local server as the endpoint a recorded run sends no request, and that
// the sender's entry sends nothing and creates nothing when called directly
// with a release version, or with a development version and an origin the
// check refuses or changes.
func TestReleaseBuildsIgnoreExperiments(t *testing.T) {
	reads := experimentEnv(t)
	for _, version := range releaseVersions {
		for _, value := range []string{"telemetryupload=https://h.example", "telemetryupload", "Bad item,=x,a-b,,bogus=1"} {
			t.Run(version+" "+value, func(t *testing.T) {
				setExperiment(t, value, true)
				*reads = 0
				data, warnings, run := statusOf(t, version)
				assert.Empty(t, warnings)
				assert.NotContains(t, data, "experiments")
				assert.NotContains(t, data, "uploadEndpoint")
				assert.NotContains(t, data, "uploadLastAttempt")
				assert.NotContains(t, run.stderr, warningPrefix)

				text := runBuilt(t, version, "telemetry", "status")
				assert.NotContains(t, text.stdout, "experiments:")
				assert.NotContains(t, text.stdout, "upload")
				assert.NotContains(t, text.stderr, warningPrefix)

				path := runBuilt(t, version, "config", "path", "-o", "json")
				assert.Empty(t, experimentWarnings(decodeStatusEnvelope(t, path.stdout).Warnings))
				assert.NotContains(t, path.stderr, warningPrefix)
				assert.Zero(t, *reads, "a release build reads the variable")
			})
		}
	}
	releaseRequestRows(t)
}

// TestDevBuildsHonorExperiments asserts that for dev, dev-<hex> and
// dev-unknown the endpoint is listed by status, in text and in -o json, as
// the canonical origin, and that the variable is read once per run; and that
// with events of a finished hour waiting and the attempt due, a recorded run
// sends exactly one request.
func TestDevBuildsHonorExperiments(t *testing.T) {
	reads := experimentEnv(t)
	for _, version := range developmentVersions {
		t.Run(version, func(t *testing.T) {
			setExperiment(t, "telemetryupload=https://H.EXAMPLE:443/", true)
			*reads = 0
			data, warnings, _ := statusOf(t, version)
			assert.Equal(t, 1, *reads)
			assert.Empty(t, warnings)
			assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
			assert.Equal(t, "https://h.example", data["uploadEndpoint"])

			text := runBuilt(t, version, "telemetry", "status")
			assert.Equal(t, 0, text.code, text.stderr)
			assert.Contains(t, text.stdout, "  experiments: telemetryupload\n")
			assert.Contains(t, text.stdout, "  upload endpoint: https://h.example\n")
		})
	}
	devRequestRows(t)
}

// TestNoUploadWithoutAnEndpoint asserts that on a development build status
// lists no experiment and no endpoint when the variable is absent, and
// lists the experiment with `upload endpoint: not set` for the bare name; and
// that in both cases a recorded run with events waiting never calls the sender.
func TestNoUploadWithoutAnEndpoint(t *testing.T) {
	experimentEnv(t)
	setExperiment(t, "", false)
	data, warnings, run := statusOf(t, "dev")
	assert.Empty(t, warnings)
	assert.NotContains(t, data, "experiments")
	assert.NotContains(t, data, "uploadEndpoint")
	text := runBuilt(t, "dev", "telemetry", "status")
	assert.NotContains(t, text.stdout, "upload")
	assert.NotContains(t, run.combined(), "://")

	setExperiment(t, "telemetryupload", true)
	data, _, _ = statusOf(t, "dev")
	assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
	assert.NotContains(t, data, "uploadEndpoint")
	text = runBuilt(t, "dev", "telemetry", "status")
	assert.Contains(t, text.stdout, "  upload endpoint: not set\n")
	noEndpointRequestRows(t)
}

// TestExecPluginRunsNeverUpload asserts that an exec plugin run of an ordinary
// command, auth get-token and token (the last two refused for want of their
// flags), and a shell completion request print no experiment warning, in
// stderr or in the envelope, while the same ordinary command warns outside
// the exec plugin; and that help, the version, a group alone and the
// completion command print none on a development build; and that, with events
// waiting and the attempt due, a recorded run sends no request and creates no
// telemetry/upload in an exec plugin run, for auth get-token and for token,
// while an ordinary run sends one.
func TestExecPluginRunsNeverUpload(t *testing.T) {
	reads := experimentEnv(t)
	setExperiment(t, "Bad item,telemetryupload=http://h.example", true)

	control := runBuilt(t, "dev", "config", "path", "-o", "json")
	require.Equal(t, 0, control.code, control.stderr)
	require.Len(t, experimentWarnings(decodeStatusEnvelope(t, control.stdout).Warnings), 2, "the control run must warn")

	for _, tc := range []struct {
		name       string
		execPlugin bool
		argv       []string
	}{
		{"an ordinary command in an exec plugin run", true, []string{"config", "path", "-o", "json"}},
		{"an ordinary command in an exec plugin run, text", true, []string{"config", "path"}},
		{"auth get-token", false, []string{"auth", "get-token", "-o", "json"}},
		{"auth get-token in an exec plugin run", true, []string{"auth", "get-token"}},
		{"token", false, []string{"token", "-o", "json"}},
		{"token in an exec plugin run", true, []string{"token"}},
		{"help", false, []string{"--help"}},
		{"version", false, []string{"--version"}},
		{"a group alone", false, []string{"telemetry"}},
		{"completion bash", false, []string{"completion", "bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := invocation.Context{ExecPlugin: tc.execPlugin}
			t.Cleanup(SetInvocation(ctx))
			if tc.execPlugin {
				t.Setenv("KUBERNETES_EXEC_INFO", "{}")
			}
			run := runBuilt(t, "dev", tc.argv...)
			assert.NotContains(t, run.combined(), warningPrefix, "exit %d", run.code)
		})
	}

	t.Run("a completion request", func(t *testing.T) {
		*reads = 0
		root, err := BuildRoot("dev")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &errOut
		runCompletionRequest(t, root, []string{"telemetry"}, "")
		assert.NotContains(t, out.String()+errOut.String(), warningPrefix)
		assert.Zero(t, *reads, "a completion request reads the variable")
	})
	execPluginRequestRows(t)
}

// TestExperimentNoticeExclusionsNameRegisteredCommands asserts every key of
// the exclusion map is the registered name of a command, so a misspelled key
// cannot exclude nothing.
func TestExperimentNoticeExclusionsNameRegisteredCommands(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	registered := map[string]bool{}
	for _, c := range wr.reg.Commands() {
		registered[c.Describe().Name] = true
	}
	for name := range experimentNoticeExcluded {
		assert.True(t, registered[name], "%q is not the name of a registered command", name)
	}
}

// TestExperimentWarningsAreWrittenOnce asserts that on a development build a
// json run holds each warning line once in warnings, and that a text run
// writes each line once to stderr and none to stdout.
func TestExperimentWarningsAreWrittenOnce(t *testing.T) {
	experimentEnv(t)
	setExperiment(t, "telemetryupload=http://h.example,bogus,Bad", true)
	want := []string{
		warningPrefix + "telemetryupload ignored: " + schemeReason,
		warningPrefix + "unknown experiment bogus ignored",
		unrecognized(3),
	}

	run := runBuilt(t, "dev", "config", "path", "-o", "json")
	require.Equal(t, 0, run.code, run.stderr)
	assert.Equal(t, want, experimentWarnings(decodeStatusEnvelope(t, run.stdout).Warnings))
	assert.NotContains(t, run.stderr, warningPrefix)

	text := runBuilt(t, "dev", "config", "path")
	require.Equal(t, 0, text.code, text.stderr)
	assert.NotContains(t, text.stdout, warningPrefix)
	for _, line := range want {
		assert.Equal(t, 1, strings.Count(text.stderr, line), "%s\nstderr: %s", line, text.stderr)
	}
}

// TestStatusListsExperimentsAndNoAttemptYet asserts, through Run, the text
// lines and the keys of status for an active experiment, that release builds
// and an absent variable add nothing, that the status warnings are listed once
// and not printed again by the notice source, and that an exec plugin run
// shows the experiment lines and no warning.
func TestStatusListsExperimentsAndNoAttemptYet(t *testing.T) {
	experimentEnv(t)
	setExperiment(t, "telemetryupload=https://h.example,bogus", true)

	data, warnings, _ := statusOf(t, "dev")
	assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
	assert.Equal(t, "https://h.example", data["uploadEndpoint"])
	assert.Contains(t, data, "uploadLastAttempt")
	assert.Nil(t, data["uploadLastAttempt"])
	assert.Equal(t, []string{warningPrefix + "unknown experiment bogus ignored"}, warnings)

	text := runBuilt(t, "dev", "telemetry", "status")
	require.Equal(t, 0, text.code, text.stderr)
	assert.Contains(t, text.stdout, "  experiments: telemetryupload\n"+
		"  upload endpoint: https://h.example\n"+
		"  last upload attempt: no attempt yet\n"+
		"  next upload attempt: at the next recorded run\n"+
		"  events waiting: 0\n"+
		"  events of the current hour: 0 (sent after the hour ends)\n")
	assert.Equal(t, 1, strings.Count(text.stderr, warningPrefix+"unknown experiment bogus ignored"), text.stderr)
	assert.NotContains(t, text.stdout, warningPrefix)

	release, _, _ := statusOf(t, "v-test")
	assert.NotContains(t, release, "experiments")

	t.Run("an exec plugin run", func(t *testing.T) {
		t.Setenv("KUBERNETES_EXEC_INFO", "{}")
		t.Cleanup(SetInvocation(invocation.Context{ExecPlugin: true}))
		data, warnings, run := statusOf(t, "dev")
		assert.Equal(t, []string{"telemetryupload"}, experimentsOf(data))
		assert.Equal(t, "https://h.example", data["uploadEndpoint"])
		assert.Empty(t, warnings)
		assert.NotContains(t, run.combined(), warningPrefix)
		text := runBuilt(t, "dev", "telemetry", "status")
		assert.Contains(t, text.stdout, "  upload endpoint: https://h.example\n")
		assert.NotContains(t, text.combined(), warningPrefix)
	})

	t.Run("warnings with no active experiment", func(t *testing.T) {
		setExperiment(t, "bogus", true)
		data, warnings, _ := statusOf(t, "dev")
		assert.NotContains(t, data, "experiments")
		assert.Equal(t, []string{warningPrefix + "unknown experiment bogus ignored"}, warnings)
	})
}

// TestExperimentWarningReachesEveryCommand asserts that, on a development
// build with an experiment set and an unknown name beside it, every
// registered command outside the exclusion map prints the warning exactly
// once: on stderr in a text run, and once in the output of an -o json run (in
// the envelope's warnings on success, in the error output on a refusal). The
// table is the registered commands, so the commands wired through Adapt and
// the bare entries are covered, and so is a command added later.
func TestExperimentWarningReachesEveryCommand(t *testing.T) {
	experimentEnv(t)
	t.Setenv("EDITOR", "rancher-test-no-such-editor")
	t.Setenv("VISUAL", "rancher-test-no-such-editor")
	setExperiment(t, "telemetryupload=https://h.example,bogus", true)
	line := warningPrefix + "unknown experiment bogus ignored"
	wr, err := buildWiredRegistry("dev", config.ReadOnlyOverride)
	require.NoError(t, err)
	var names []string
	for _, c := range wr.reg.Commands() {
		name := c.Describe().Name
		if _, excluded := experimentNoticeExcluded[name]; excluded {
			continue
		}
		names = append(names, name)
		words := strings.Fields(name)
		t.Run(name, func(t *testing.T) {
			text := runBuilt(t, "dev", words...)
			assert.Equal(t, 1, strings.Count(text.stderr, line), "text run, exit %d\nstderr: %s", text.code, text.stderr)
			assert.NotContains(t, text.stdout, warningPrefix)

			js := runBuilt(t, "dev", append(slices.Clone(words), "-o", "json")...)
			assert.Equal(t, 1, strings.Count(js.combined(), line), "json run, exit %d\nstdout: %s\nstderr: %s", js.code, js.stdout, js.stderr)
		})
	}
	assert.Contains(t, names, "skill status", "a bare entry")
	assert.Contains(t, names, "schema", "a command wired through Adapt")
	assert.Contains(t, names, "config path", "a command with a store")
}
