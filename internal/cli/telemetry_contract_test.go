package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// contractDir is the committed directory of the current contract version.
var contractDir = filepath.Join("..", "..", "telemetry", "contract", telemetry.ContractVersion)

func contractInputs() telemetry.ContractInputs {
	return telemetry.ContractInputs{ErrorCodes: clierr.AllCodes()}
}

// TestTelemetryContract asserts the committed schema, field table and OTLP
// mapping are what the attribute metadata, the error codes and the agent ids
// generate. With -update it rewrites them; an attribute added to the
// metadata without regenerating fails here.
func TestTelemetryContract(t *testing.T) {
	files, err := telemetry.ContractFiles(contractInputs())
	require.NoError(t, err)
	require.Len(t, files, 3)
	if *update {
		require.NoError(t, os.MkdirAll(contractDir, 0o755))
		for name, data := range files {
			require.NoError(t, os.WriteFile(filepath.Join(contractDir, name), data, 0o644))
		}
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(contractDir, name))
		require.NoError(t, err, "run: go test ./internal/cli/ -run TestTelemetryContract -update")
		assert.Equal(t, string(want), string(got), "%s is stale; run: go test ./internal/cli/ -run TestTelemetryContract -update", name)
	}
}

// schemaEnum returns the enum list of one attribute of one event in the
// committed schema.
func schemaEnum(t *testing.T, event, attr string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(contractDir, telemetry.SchemaFile))
	require.NoError(t, err)
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	return doc.Defs[event].Properties[attr].Enum
}

// TestTelemetryValueListsEqualTheCode asserts the closed lists in the
// committed schema equal the code they mirror: the error codes and the
// agent ids.
func TestTelemetryValueListsEqualTheCode(t *testing.T) {
	assert.Equal(t, append(clierr.AllCodes(), telemetry.Other), schemaEnum(t, telemetry.EventCommandRun, "error.type"))
	assert.Equal(t, append(invocation.AgentIDs(), invocation.AgentGeneric), schemaEnum(t, telemetry.EventCommandRun, "io.rancher.client.agent"))
}

// releasePlatforms reads the platform list of the release workflow's build
// loop.
func releasePlatforms(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yaml"))
	require.NoError(t, err)
	m := regexp.MustCompile(`for platform in ([a-z0-9/ ]+); do`).FindSubmatch(data)
	require.NotNil(t, m, "release.yaml has no platform loop")
	return strings.Fields(string(m[1]))
}

// TestTelemetryPlatformListsCoverTheRelease asserts every operating system
// and architecture the release workflow builds maps to a listed value of
// os.type and host.arch, never _OTHER, and that the committed schema lists
// each of them.
func TestTelemetryPlatformListsCoverTheRelease(t *testing.T) {
	platforms := releasePlatforms(t)
	require.NotEmpty(t, platforms)
	osList := schemaEnum(t, telemetry.EventCommandRun, "os.type")
	archList := schemaEnum(t, telemetry.EventCommandRun, "host.arch")
	for _, p := range platforms {
		goos, goarch, ok := strings.Cut(p, "/")
		require.True(t, ok, p)
		gotOS := telemetry.NormalizeOS(goos)
		gotArch := telemetry.NormalizeArch(goarch)
		assert.NotEqual(t, telemetry.Other, gotOS, "%s: os.type", p)
		assert.NotEqual(t, telemetry.Other, gotArch, "%s: host.arch", p)
		assert.Contains(t, osList, gotOS, "%s: os.type is not in the schema", p)
		assert.Contains(t, archList, gotArch, "%s: host.arch is not in the schema", p)
	}
}

var (
	commandNameRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}( [a-z][a-z0-9-]{0,31}){0,2}$`)
	flagNameRule    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// nameRuleViolations lists every command name, alias and flag name in the
// manifest that breaks the contract's name rule. A global flag's long form
// is the part after the slash, without dashes.
func nameRuleViolations(m runtime.Manifest) []string {
	var out []string
	for _, c := range m.Commands {
		if !commandNameRule.MatchString(c.Name) {
			out = append(out, "command "+c.Name)
		}
		for _, a := range c.Aliases {
			if !commandNameRule.MatchString(a) {
				out = append(out, "alias "+a+" of "+c.Name)
			}
		}
		for _, f := range c.Flags {
			if !flagNameRule.MatchString(f.Name) {
				out = append(out, "flag "+f.Name+" of "+c.Name)
			}
		}
	}
	for _, g := range m.GlobalFlags {
		long := g.Name
		if i := strings.LastIndex(long, "/"); i >= 0 {
			long = long[i+1:]
		}
		long = strings.TrimPrefix(long, "--")
		if !flagNameRule.MatchString(long) {
			out = append(out, "global flag "+g.Name)
		}
	}
	return out
}

// TestRegisteredNamesFollowTheContractNameRule asserts every registered
// command, alias and flag, the global flags included, follows the name rule
// the contract states: a recorded name is never refused by the encoder.
func TestRegisteredNamesFollowTheContractNameRule(t *testing.T) {
	m, err := buildManifest("test")
	require.NoError(t, err)
	require.NotEmpty(t, m.Commands)
	require.NotEmpty(t, m.GlobalFlags)
	assert.Empty(t, nameRuleViolations(m))
}

// TestNameRuleCatchesViolations asserts the check above reports an
// uppercase command word, an overlong flag and a global flag with an
// underscore.
func TestNameRuleCatchesViolations(t *testing.T) {
	m := runtime.Manifest{
		Commands: []runtime.SchemaCommand{
			{Name: "get Project"},
			{Name: "get project", Flags: []runtime.ArgSpec{{Name: strings.Repeat("a", 33)}}},
			{Name: "ok", Aliases: []string{"Bad"}},
		},
		GlobalFlags: []runtime.GlobalFlag{{Name: "--bad_flag"}, {Name: "-o/--output"}},
	}
	got := nameRuleViolations(m)
	assert.True(t, slices.ContainsFunc(got, func(s string) bool { return s == "command get Project" }), got)
	assert.True(t, slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "flag aaaa") }), got)
	assert.Contains(t, got, "alias Bad of ok")
	assert.Contains(t, got, "global flag --bad_flag")
	assert.Len(t, got, 4)
}
