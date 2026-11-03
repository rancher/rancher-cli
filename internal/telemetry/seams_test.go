package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFixedClockSeamFreezesTheEventHourAndRestoresTheClock asserts that
// activating the seam over another clock makes an event land in the hour
// directory of the current time, and that the restore puts the earlier clock
// back.
func TestFixedClockSeamFreezesTheEventHourAndRestoresTheClock(t *testing.T) {
	enable(t)
	earlier := useClock(t, time.Now().UTC().Add(-72*time.Hour))
	before := earlier.now()

	restore := SetFixedClockForTest()
	t.Cleanup(restore)
	fixed := nowFunc()
	require.NotEqual(t, before, fixed)

	dir := t.TempDir()
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(time.Time{})))
	assert.Equal(t, []string{hourName(fixed)}, readNames(t, filepath.Join(dir, "telemetry", "events")))

	restore()
	assert.Equal(t, before, nowFunc(), "the restore puts the earlier clock back")
}

// TestFixedClockSeamKeepsOneTimeAcrossActivations asserts a second activation
// while the seam is active keeps the first time, and that its restore does
// nothing.
func TestFixedClockSeamKeepsOneTimeAcrossActivations(t *testing.T) {
	first := SetFixedClockForTest()
	t.Cleanup(first)
	at := nowFunc()

	time.Sleep(5 * time.Millisecond)
	second := SetFixedClockForTest()
	assert.Equal(t, at, nowFunc())

	second()
	assert.Equal(t, at, nowFunc(), "the restore of the second activation does nothing")
}

// TestDistinctSlotsSeamKeepsOneCounterAcrossActivations asserts two events
// written around a second activation take slots 0000 and 0001, each with one
// open and no retry.
func TestDistinctSlotsSeamKeepsOneCounterAcrossActivations(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(hourPath(dir, fixedNoon), 0o700), "an existing hour directory: no create-and-retry")
	ops := observeOps(t)

	t.Cleanup(SetDistinctSlotsForTest())
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	first := ops.take()

	t.Cleanup(SetDistinctSlotsForTest())
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	second := ops.take()

	assert.Equal(t, []string{"openfile events/" + hourName(fixedNoon) + "/0000.json"}, opens(first))
	assert.Equal(t, []string{"openfile events/" + hourName(fixedNoon) + "/0001.json"}, opens(second))
}

// TestDistinctSlotsSeamRestoreBringsBackTheRandomChoice asserts the restore
// puts the previous choice function back and the seam can be activated again.
func TestDistinctSlotsSeamRestoreBringsBackTheRandomChoice(t *testing.T) {
	restore := SetDistinctSlotsForTest()
	t.Cleanup(restore)
	assert.Equal(t, 0, randIntn(10))
	assert.Equal(t, 1, randIntn(10))
	restore()

	restore = SetDistinctSlotsForTest()
	t.Cleanup(restore)
	assert.Equal(t, 0, randIntn(10), "a new activation starts a new counter")
}

// opens returns the file creations in ops.
func opens(ops []string) []string {
	var out []string
	for _, op := range ops {
		if strings.HasPrefix(op, "openfile events/") && strings.HasSuffix(op, ".json") {
			out = append(out, op)
		}
	}
	return out
}
