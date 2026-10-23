package telemetry

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
)

func exampleFiles(t *testing.T, kind string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(versionDir, "examples", kind, "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no %s examples", kind)
	return files
}

// TestValidExamplesPassTheSchema asserts every committed valid example is
// accepted by the committed schema.
func TestValidExamplesPassTheSchema(t *testing.T) {
	v := loadSchema(t, filepath.Join(versionDir, SchemaFile))
	for _, f := range exampleFiles(t, "valid") {
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.Empty(t, v.errors(decodeJSON(t, data)), filepath.Base(f))
	}
}

// invalidReasons names, for every committed invalid example, the text its
// schema error must contain: the rule it exists to trip.
var invalidReasons = map[string]string{
	"agent-not-on-the-list":              "io.rancher.client.agent: my-private-agent is not in the list",
	"boolean-written-as-text":            "io.rancher.client.ci: not of type boolean",
	"client-type-not-on-the-list":        "io.rancher.client.type: robot is not in the list",
	"command-with-four-words":            "io.rancher.cli.command: does not match",
	"command-with-uppercase":             "io.rancher.cli.command: does not match",
	"dropped-count-below-one":            "io.rancher.cli.events.dropped.count: below 1",
	"dropped-without-count":              "missing io.rancher.cli.events.dropped.count",
	"duration-bucket-not-on-the-list":    "duration.bucket: 12s is not in the list",
	"error-type-not-on-the-list":         "error.type: Some text with a customer name is not in the list",
	"error-type-on-an-ok-run":            "matches a schema it must not match",
	"exit-code-out-of-range":             "process.exit.code: above 255",
	"flag-carries-a-value":               "command.flags[0]: does not match",
	"id-in-uppercase":                    "service.instance.id: does not match",
	"install-id-and-missing-reason":      "matches a schema it must not match",
	"install-id-not-a-uuid":              "install.id: does not match",
	"manager-version-over-64-characters": "io.rancher.manager.version: longer than 64",
	"missing-record-uid":                 "missing log.record.uid",
	"more-than-32-flags":                 "more than 32 items",
	"no-install-id-and-no-reason":        "missing io.rancher.cli.install_id.missing_reason",
	"os-type-not-on-the-list":            "os.type: Linux is not in the list",
	"time-not-a-whole-hour":              "time: does not match",
	"unknown-event-name":                 "matches 0 of the alternatives",
	"version-keeps-hotfix-id":            "service.version: does not match",
	"version-with-build-suffix":          "service.version: does not match",
	"wrong-schema-version":               "schema_version: not the constant",
}

// TestInvalidExamplesFailTheSchema asserts every committed invalid example
// is refused by the committed schema for the rule it exists to trip, one
// rule each.
func TestInvalidExamplesFailTheSchema(t *testing.T) {
	v := loadSchema(t, filepath.Join(versionDir, SchemaFile))
	seen := map[string]bool{}
	for _, f := range exampleFiles(t, "invalid") {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		seen[name] = true
		want, ok := invalidReasons[name]
		require.True(t, ok, "%s has no entry in invalidReasons", name)
		data, err := os.ReadFile(f)
		require.NoError(t, err)
		errs := v.errors(decodeJSON(t, data))
		assert.True(t, slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, want) }), "%s: want %q in %v", name, want, errs)
	}
	for name := range invalidReasons {
		assert.True(t, seen[name], "invalidReasons lists %s, which is not a file", name)
	}
}

// fixedUUIDBytes makes newUUID return a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d.
func fixedUUIDBytes(t *testing.T) {
	t.Helper()
	old := randRead
	randRead = func(b []byte) (int, error) {
		copy(b, []byte{0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x4a, 0x7b, 0x8c, 0x9d, 0x0e, 0x1f, 0x2a, 0x3b, 0x4c, 0x5d})
		return len(b), nil
	}
	t.Cleanup(func() { randRead = old })
}

// TestValidExamplesAreWhatTheEncoderWrites asserts the full command run
// example and the dropped example equal what the constructors and the
// encoder write for the same input; an example cannot drift from the
// code.
func TestValidExamplesAreWhatTheEncoderWrites(t *testing.T) {
	fixedUUIDBytes(t)
	ctx := invocation.Context{Version: "0.1.0-alpha.8", OS: "linux", Arch: "amd64", Agent: "claude-code"}
	run := NewCommandRun(RunInput{
		Time:           time.Date(2026, 10, 23, 14, 37, 12, 0, time.UTC),
		Invocation:     ctx,
		InstanceID:     "3f2a6c1e-8b7d-4e5a-9c10-2d4f6a8b0c1e",
		Command:        "get project",
		Flags:          []string{"output", "cluster"},
		Outcome:        OutcomeError,
		ErrorType:      "RBAC",
		ErrorCodes:     []string{"RBAC", "NOT_FOUND"},
		ExitCode:       6,
		ExitCodeSet:    true,
		Duration:       200 * time.Millisecond,
		ManagerVersion: "v2.15.2",
		Edition:        EditionPrime,
		InstallID:      "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e",
	}).withDirectoryID("7b1d9e04-5a3c-4f68-8e21-c0d4a6b8e2f1")
	prod, _ := producerOf(ctx, "3f2a6c1e-8b7d-4e5a-9c10-2d4f6a8b0c1e")
	dropped := newEventsDropped(time.Date(2026, 10, 23, 13, 0, 0, 0, time.UTC), 12, prod).withDirectoryID("7b1d9e04-5a3c-4f68-8e21-c0d4a6b8e2f1")
	// The dropped example carries the uid its own file lists.
	cases := map[string]Event{
		"command-run-full.json": run,
		"events-dropped.json":   dropped,
	}
	for name, ev := range cases {
		want, err := os.ReadFile(filepath.Join(versionDir, "examples", "valid", name))
		require.NoError(t, err)
		got := decodeJSON(t, ev.Flat()).(map[string]any)
		if name == "events-dropped.json" {
			got[attrRecordUID] = "0f1e2d3c-4b5a-4968-8776-655443322110"
			got[attrInstanceID] = "3f2a6c1e-8b7d-4e5a-9c10-2d4f6a8b0c1e"
		}
		assert.Equal(t, decodeJSON(t, want), any(got), name)
	}
}
