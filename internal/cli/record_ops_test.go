package cli

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/experiment"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// hitLine is the exec plugin's flag fast path, served from a cached
// credential.
var hitLine = []string{"auth", "get-token", "--server", "https://r.ex", "--name", "prod", "--cluster", "c-1"}

// slotPath matches the path of an event file.
var slotPath = regexp.MustCompile(`events/\d{10}/\d{4}\.json`)

// runCounted runs argv on a new tree and returns the identity operations the
// run made and the telemetry file operations, with the config directory
// and the event file name masked.
func runCounted(t *testing.T, dir string, argv ...string) (identity map[string]int, files []string) {
	t.Helper()
	return runCountedRoot(t, newRoot(t), dir, argv...)
}

// runCountedRoot is runCounted on the tree root.
func runCountedRoot(t *testing.T, root *ucli.Command, dir string, argv ...string) (identity map[string]int, files []string) {
	t.Helper()
	identity = map[string]int{}
	restore := config.SetIdentityObserverForTest(func(op string) { identity[op]++ })
	defer restore()
	ops := observeFiles(t)
	got := runRecorded(t, root, argv...)
	require.Zero(t, got.res.code, got.stderr)
	for _, op := range *ops {
		op = strings.ReplaceAll(op, dir, "DIR")
		files = append(files, slotPath.ReplaceAllString(op, "events/HOUR/SLOT.json"))
	}
	return identity, files
}

// TestRecordingAddsTheListedOperationsToThePluginHit asserts the exec
// plugin's hit, through the built tree, makes the same identity operations
// with recording on as off (no config load added) and no telemetry file
// operation off; with it on, the directory and the id present and the
// hourly marker fresh, it adds the stat of the config directory, then
// opening the directory, looking at the telemetry directory, reading the id,
// looking at the marker, reading the server's facts, and creating, writing
// and closing the event file. A command with no server skips the facts read.
func TestRecordingAddsTheListedOperationsToThePluginHit(t *testing.T) {
	dir := recordingEnv(t)
	seedCachedHit(t, dir, "https://r.ex")
	runRecorded(t, newRoot(t), hitLine...)
	require.Len(t, storedRuns(t, dir), 1)

	t.Setenv(telemetry.EnvTelemetry, "false")
	baseline, files := runCounted(t, dir, hitLine...)
	assert.Equal(t, map[string]int{"configload": 2, "stampread": 2}, baseline)
	assert.Empty(t, files, "off adds no telemetry operation")

	t.Setenv(telemetry.EnvTelemetry, "true")
	identity, files := runCounted(t, dir, hitLine...)
	assert.Equal(t, baseline, identity, "recording adds no config load and no stamp read")
	facts := "readfile servers/" + telemetry.FactsFileName("https://r.ex")
	assert.Equal(t, []string{
		"stat DIR",
		"openroot .",
		"lstat telemetry",
		"openroot telemetry",
		"readfile config-directory-id",
		"stat pruned",
		facts,
		"openfile events/HOUR/SLOT.json",
		"write events/HOUR/SLOT.json",
		"close events/HOUR/SLOT.json",
	}, files)

	_, files = runCounted(t, dir, "config", "path")
	assert.NotContains(t, files, facts)
	assert.Len(t, files, 9, "with no server the facts read is missing")
	assert.Equal(t, "stat DIR", files[0])

	t.Run("a development root with the experiment set", func(t *testing.T) {
		develop := func() *ucli.Command {
			root, err := BuildRoot("dev")
			require.NoError(t, err)
			return root
		}
		runCountedRoot(t, develop(), dir, hitLine...)
		_, without := runCountedRoot(t, develop(), dir, hitLine...)
		assert.Equal(t, []string{
			"stat DIR", "openroot .", "lstat telemetry", "openroot telemetry", "readfile config-directory-id", "stat pruned",
			facts, "openfile events/HOUR/SLOT.json", "write events/HOUR/SLOT.json", "close events/HOUR/SLOT.json",
		}, without, "the plugin hit of a development root with no experiment")
		t.Setenv(experiment.EnvVar, "telemetryupload=http://127.0.0.1:9")
		identity, with := runCountedRoot(t, develop(), dir, hitLine...)
		assert.Equal(t, without, with, "the exec plugin reads and writes nothing for the upload")
		assert.Equal(t, baseline, identity)
	})
}

// hitBench builds the plugin's hit once for a benchmark: a config directory
// with a cached credential and the telemetry directory and id present.
func hitBench(b *testing.B) string {
	b.Helper()
	dir := recordingEnv(b)
	seedCachedHit(b, dir, "https://r.ex")
	root := newRoot(b)
	runRecorded(b, root, hitLine...)
	return dir
}

// runHit runs the plugin's hit on a new tree, with the tree built outside
// the timed region, and discards the output.
func runHit(b *testing.B) {
	b.Helper()
	b.StopTimer()
	root := newRoot(b)
	root.Writer, root.ErrWriter, root.Reader = io.Discard, io.Discard, strings.NewReader("")
	b.StartTimer()
	res := run(b.Context(), root, append([]string{"rancher"}, hitLine...))
	if res.code != 0 {
		b.Fatalf("the hit exited %d", res.code)
	}
}

// BenchmarkPluginHit measures the exec plugin's flag fast path hit through
// the built tree and run, with recording on and off. Run it with a fixed
// -benchtime such as 200x: each recorded run adds an event to the current
// hour, and the hour holds 1024.
func BenchmarkPluginHit(b *testing.B) {
	hitBench(b)
	for _, mode := range []struct{ name, variable string }{{"off", "false"}, {"on", "true"}} {
		b.Run(mode.name, func(b *testing.B) {
			b.Setenv(telemetry.EnvTelemetry, mode.variable)
			b.ReportAllocs()
			for b.Loop() {
				runHit(b)
			}
		})
	}
}

// BenchmarkPluginHitWithTheHourlyPass measures a plugin hit that also runs
// the hourly pass: the marker is two days old and one finished hour with one
// event waits to be compacted. Run it with a fixed -benchtime such as 200x.
func BenchmarkPluginHitWithTheHourlyPass(b *testing.B) {
	dir := hitBench(b)
	b.Setenv(telemetry.EnvTelemetry, "true")
	events := filepath.Join(dir, "telemetry", "events")
	hours, err := os.ReadDir(events)
	require.NoError(b, err)
	require.Len(b, hours, 1)
	slots, err := os.ReadDir(filepath.Join(events, hours[0].Name()))
	require.NoError(b, err)
	require.NotEmpty(b, slots)
	sample, err := os.ReadFile(filepath.Join(events, hours[0].Name(), slots[0].Name()))
	require.NoError(b, err)
	marker := filepath.Join(dir, "telemetry", "pruned")
	for b.Loop() {
		b.StopTimer()
		old := filepath.Join(events, time.Now().UTC().Add(-5*time.Hour).Format("2006010215"))
		require.NoError(b, os.RemoveAll(old))
		require.NoError(b, os.MkdirAll(old, 0o700))
		require.NoError(b, os.WriteFile(filepath.Join(old, "0001.json"), sample, 0o600))
		past := time.Now().Add(-48 * time.Hour)
		require.NoError(b, os.Chtimes(marker, past, past))
		b.StartTimer()
		runHit(b)
	}
}
