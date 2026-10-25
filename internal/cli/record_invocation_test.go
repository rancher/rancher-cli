package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// TestTheInvocationContextReachesTheEvent asserts the context SetInvocation
// holds is the one the stored event describes: the version, the exec plugin,
// the agent, CI and the client type that follows from them, and the
// operating system and architecture, for the plugin's cached hit run
// through the built tree.
func TestTheInvocationContextReachesTheEvent(t *testing.T) {
	rows := []struct {
		name string
		env  map[string]string
		want map[string]any
	}{
		{
			name: "exec plugin",
			env:  map[string]string{"KUBERNETES_EXEC_INFO": "{}"},
			want: map[string]any{
				"io.rancher.cli.exec_plugin": true,
				"io.rancher.client.type":     "automation",
				"io.rancher.client.ci":       false,
			},
		},
		{
			name: "agent in CI",
			env:  map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1", "CI": "true"},
			want: map[string]any{
				"io.rancher.client.agent":    "claude-code",
				"io.rancher.client.ci":       true,
				"io.rancher.client.type":     "agent",
				"io.rancher.cli.exec_plugin": false,
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := recordingEnv(t)
			seedCachedHit(t, dir, "https://r.ex")
			getenv := func(k string) string { return row.env[k] }
			t.Cleanup(SetInvocation(invocation.Detect("1.2.3", getenv)))
			got := runRecorded(t, newRoot(t), hitLine...)
			require.Zero(t, got.res.code, got.stderr)
			evs := storedRuns(t, dir)
			require.Len(t, evs, 1)
			fields := evs[0].Fields
			assert.Equal(t, "1.2.3", fields["service.version"])
			assert.NotEqual(t, "_OTHER", fields["os.type"])
			assert.NotEqual(t, "_OTHER", fields["host.arch"])
			for key, want := range row.want {
				assert.Equal(t, want, fields[key], key)
			}
		})
	}
}

// fakeClock makes every run measure elapsed between its first and second
// reading of the clock, and puts the clock back when the test ends.
func fakeClock(t *testing.T, elapsed time.Duration) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0)
	calls := 0
	orig := now
	now = func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(elapsed)
	}
	t.Cleanup(func() { now = orig })
}

// TestTheEventCarriesTheDurationBucketAndTheClientType asserts the duration
// bucket follows the time between the start of a run and the end of its
// output, with a clock the test controls, and that a run with no agent, no
// exec plugin and a buffer for standard output is an automation client.
func TestTheEventCarriesTheDurationBucketAndTheClientType(t *testing.T) {
	rows := []struct {
		elapsed time.Duration
		bucket  string
	}{
		{500 * time.Millisecond, "lt1s"},
		{3 * time.Second, "1to5s"},
		{6 * time.Second, "gt5s"},
	}
	for _, row := range rows {
		t.Run(row.bucket, func(t *testing.T) {
			dir := recordingEnv(t)
			fakeClock(t, row.elapsed)
			runRecorded(t, newRoot(t), "config", "path")
			evs := storedRuns(t, dir)
			require.Len(t, evs, 1)
			assert.Equal(t, row.bucket, evs[0].Fields["io.rancher.cli.command.duration.bucket"])
			assert.Equal(t, "automation", evs[0].Fields["io.rancher.client.type"])
		})
	}
}

// TestAPanicWhileRecordingChangesNothing asserts a panic in the code that
// builds the event leaves the same standard output, standard error and
// result as the same line with recording off, and stores no event.
func TestAPanicWhileRecordingChangesNothing(t *testing.T) {
	dir := recordingEnv(t)
	t.Setenv(telemetry.EnvTelemetry, "false")
	off := runRecorded(t, newRoot(t), "config", "path")

	t.Setenv(telemetry.EnvTelemetry, "true")
	orig := newRunEvent
	newRunEvent = func(telemetry.RunInput) telemetry.CommandRun { panic("event builder failed") }
	t.Cleanup(func() { newRunEvent = orig })
	on := runRecorded(t, newRoot(t), "config", "path")

	assert.Equal(t, off, on)
	assert.Empty(t, storedRuns(t, dir))
}
