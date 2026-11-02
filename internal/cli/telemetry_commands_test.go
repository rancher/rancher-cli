package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

var telemetryVerbs = []string{"status", "on", "off", "reset", "show"}

// runTelemetry builds a fresh root and runs `telemetry <verb> -o json`.
func runTelemetry(t *testing.T, verb string) (stdout, stderr string, code int) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	return runBounded(t, root, []string{"telemetry", verb, "-o", "json"})
}

// TestTelemetryCommandsRunUnderEnvAndConfigReadOnly asserts all five
// commands exit 0 under the environment and the config read-only sources,
// that on and off leave the key written, and that in the same setup a
// mutating command is refused by the read-only source.
func TestTelemetryCommandsRunUnderEnvAndConfigReadOnly(t *testing.T) {
	restoreRawArgv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	for _, source := range []string{"env", "config"} {
		t.Run(source, func(t *testing.T) {
			rec := extraArgsEnv(t)
			dir := os.Getenv(config.EnvConfigDir)
			if source == "env" {
				t.Setenv(config.EnvReadOnly, "1")
			} else {
				require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))
			}
			for _, verb := range telemetryVerbs {
				_, stderr, code := runTelemetry(t, verb)
				assert.Equal(t, 0, code, "%s: %s", verb, stderr)
				if verb == "on" || verb == "off" {
					want := map[string]string{"on": "true", "off": "false"}[verb]
					raw, err := os.ReadFile(config.FilePath(dir))
					require.NoError(t, err)
					var m map[string]any
					require.NoError(t, yaml.Unmarshal(raw, &m))
					assert.Equal(t, want, m["telemetry"], verb)
				}
			}
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			_, stderr, code := runBounded(t, root, []string{"create", "namespace", "ns1", "--cluster", "local", "-o", "json"})
			assert.Equal(t, 2, code, "a mutating command is refused: %s", stderr)
			assert.Contains(t, renderedError(t, "json", stderr).Message, "read-only mode: create namespace is a mutating command")
			assert.Empty(t, rec.take())
		})
	}
}

// TestTelemetryCommandsMakeNoRequest asserts all five commands, run through
// the built tree with a server and a token in the environment, send no
// request to that server.
func TestTelemetryCommandsMakeNoRequest(t *testing.T) {
	restoreRawArgv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	rec := extraArgsEnv(t)
	for _, verb := range telemetryVerbs {
		_, stderr, code := runTelemetry(t, verb)
		assert.Equal(t, 0, code, "%s: %s", verb, stderr)
	}
	assert.Empty(t, rec.take())
}

// TestTelemetryEnvelopes asserts the decoded envelopes: configKey is a JSON
// string and absent without a key; an invalid key gives state on, decidedBy
// default and the warning, exit 0; on and off carry replacedValue.
func TestTelemetryEnvelopes(t *testing.T) {
	restoreRawArgv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	extraArgsEnv(t)
	dir := os.Getenv(config.EnvConfigDir)
	type envelope struct {
		Kind string `json:"kind"`
		Data map[string]any
		// Warnings sits at the envelope level.
		Warnings []string `json:"warnings"`
	}
	decode := func(stdout string) envelope {
		var raw struct {
			Kind     string         `json:"kind"`
			Data     map[string]any `json:"data"`
			Warnings []string       `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &raw), stdout)
		return envelope{Kind: raw.Kind, Data: raw.Data, Warnings: raw.Warnings}
	}

	stdout, stderr, code := runTelemetry(t, "status")
	require.Equal(t, 0, code, stderr)
	env := decode(stdout)
	assert.Equal(t, "TelemetryStatus", env.Kind)
	assert.NotContains(t, env.Data, "configKey")
	assert.Equal(t, "on", env.Data["state"])
	assert.Equal(t, "default", env.Data["decidedBy"])

	for _, key := range []string{"true", "false", "maybe", ""} {
		require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: "+strings.TrimSpace(quote(key))+"\n"), 0o600))
		stdout, stderr, code = runTelemetry(t, "status")
		require.Equal(t, 0, code, stderr)
		env = decode(stdout)
		got, ok := env.Data["configKey"].(string)
		require.True(t, ok, "configKey is %T", env.Data["configKey"])
		assert.Equal(t, key, got)
		switch key {
		case "true":
			assert.Equal(t, "config", env.Data["decidedBy"])
			assert.Empty(t, env.Warnings)
		case "false":
			assert.Equal(t, "off", env.Data["state"])
			assert.Empty(t, env.Warnings)
		default:
			assert.Equal(t, "on", env.Data["state"])
			assert.Equal(t, "default", env.Data["decidedBy"])
			require.Len(t, env.Warnings, 1)
			assert.Contains(t, env.Warnings[0], quote(key))
		}
	}

	t.Setenv(telemetry.EnvTelemetry, "false")
	stdout, _, code = runTelemetry(t, "status")
	require.Equal(t, 0, code)
	env = decode(stdout)
	assert.Equal(t, "off", env.Data["state"])
	assert.Equal(t, "RANCHER_CLI_TELEMETRY", env.Data["decidedBy"])
	require.Len(t, env.Warnings, 1, "the empty key still warns while the variable decides")

	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: maybe\n"), 0o600))
	stdout, stderr, code = runTelemetry(t, "on")
	require.Equal(t, 0, code, stderr)
	env = decode(stdout)
	assert.Equal(t, "TelemetrySetting", env.Kind)
	assert.Equal(t, "maybe", env.Data["replacedValue"])
	assert.Equal(t, "true", env.Data["configKey"])
	assert.Empty(t, env.Warnings)

	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: \"a b\\nc\"\n"), 0o600))
	stdout, stderr, code = runTelemetry(t, "off")
	require.Equal(t, 0, code, stderr)
	env = decode(stdout)
	assert.Equal(t, "a b\nc", env.Data["replacedValue"])
	assert.Equal(t, []any{"events", "serverFacts", "configDirectoryId", "uploadState"}, env.Data["cleared"])

	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("telemetry: [a]\n"), 0o600))
	for _, verb := range telemetryVerbs {
		_, stderr, code = runTelemetry(t, verb)
		assert.Equal(t, 1, code, "%s stops on a config that fails to load", verb)
		assert.Equal(t, clierr.CodeGeneric, renderedError(t, "json", stderr).Code)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestTelemetryShowEmptyListEnvelope asserts the JSON data of an empty store
// is an empty array, not null.
func TestTelemetryShowEmptyListEnvelope(t *testing.T) {
	restoreRawArgv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	extraArgsEnv(t)
	stdout, stderr, code := runTelemetry(t, "show")
	require.Equal(t, 0, code, stderr)
	var raw struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &raw))
	assert.JSONEq(t, `[]`, string(raw.Data))
}

// TestEnvVarsListTheTelemetryVariable asserts the environment table has one
// row named by the telemetry package's constant and no row for DO_NOT_TRACK.
func TestEnvVarsListTheTelemetryVariable(t *testing.T) {
	var names []string
	for _, e := range runtime.EnvVars() {
		names = append(names, e.Name)
	}
	assert.Equal(t, 1, strings.Count(strings.Join(names, " "), telemetry.EnvTelemetry+" "), "one row")
	assert.Contains(t, names, telemetry.EnvTelemetry)
	assert.NotContains(t, names, "DO_NOT_TRACK")
}

// TestManifestTelemetryMatchesTheControls asserts the manifest's control names
// equal telemetry.Controls without the test binary, in order, and its
// version equals the contract version; every control carries its text.
func TestManifestTelemetryMatchesTheControls(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	m := wr.reg.Schema("v-test", config.MinRancherVersion).Telemetry
	assert.Equal(t, telemetry.ContractVersion, m.ContractVersion)
	var want []string
	for _, c := range telemetry.Controls() {
		if c != telemetry.ControlTestBinary {
			want = append(want, string(c))
		}
	}
	var got []string
	for _, c := range m.Controls {
		got = append(got, c.Name)
		assert.NotEmpty(t, c.Source, c.Name)
		assert.NotEmpty(t, c.Values, c.Name)
		assert.NotEmpty(t, c.Desc, c.Name)
		assert.NotContains(t, c.Desc, clierr.Rancher, "plain text: no remedy marker in the manifest")
	}
	assert.Equal(t, want, got)
	assert.False(t, slices.Contains(got, string(telemetry.ControlTestBinary)))
}

// TestTelemetryOffFailureEnvelopeNamesEachError asserts the decoded error
// envelope of an off whose deletes fail names each item left with its own
// error, in order, joined by "; ". A telemetry directory that cannot be
// written makes the events and the server facts fail with different paths in
// their errors.
func TestTelemetryOffFailureEnvelopeNamesEachError(t *testing.T) {
	if goruntime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory modes do not stop deletes on Windows or as root")
	}
	restoreRawArgv(t)
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Setenv(telemetry.EnvTelemetry, "")
	extraArgsEnv(t)
	tel := filepath.Join(os.Getenv(config.EnvConfigDir), "telemetry")
	for _, p := range []string{"events/2026102010", "servers"} {
		require.NoError(t, os.MkdirAll(filepath.Join(tel, p), 0o700))
	}
	require.NoError(t, os.Chmod(tel, 0o500))
	t.Cleanup(func() { _ = os.Chmod(tel, 0o700) })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runBounded(t, root, []string{"telemetry", "off", "-o", "json"})
	assert.Equal(t, 1, code)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Regexp(t, `^the telemetry key is "false": could not delete from .*telemetry: events \(.+\); server facts \(.+\)$`, ce.Message)
	assert.NotContains(t, ce.Message, "config directory id")
}
