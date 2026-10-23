package telemetry

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fixedNoon = time.Date(2026, 10, 23, 14, 30, 0, 0, time.UTC)

// TestRecordWritesOneValidEvent asserts one recorded event is one slot file
// in the event's hour directory, valid against the schema, readable back.
func TestRecordWritesOneValidEvent(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	ev := runEvent(fixedNoon)
	require.Equal(t, StatusRecorded, record(t, dir, ev))

	names := readNames(t, hourPath(dir, fixedNoon))
	require.Len(t, names, 1)
	assert.Regexp(t, `^[0-9]{4}\.json$`, names[0])
	data, err := os.ReadFile(filepath.Join(hourPath(dir, fixedNoon), names[0]))
	require.NoError(t, err)
	stored := decodeJSON(t, data).(map[string]any)
	assert.EqualValues(t, LocalFormat, stored[keyLocalFormat])
	assert.Equal(t, ev.UID(), stored[attrRecordUID])
	assert.Regexp(t, uuidPattern, stored[attrConfigDirID], "the first event creates the directory id")
	delete(stored, keyLocalFormat)
	assert.Empty(t, schemaValidator(t).errors(stored))

	s := openStore(t, dir)
	got, err := s.List()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ev.UID(), got[0].UID)
	assert.Equal(t, EventCommandRun, got[0].Name)
	assert.Equal(t, fixedNoon.Truncate(time.Hour), got[0].Time)
}

// TestRecordTouchesNothingWhileOff asserts every control that turns
// recording off, and a caller that saw no configuration directory, return
// before any file operation and leave the directory as it was.
func TestRecordTouchesNothingWhileOff(t *testing.T) {
	useClock(t, fixedNoon)
	ops := observeOps(t)
	cases := []struct {
		name        string
		opts        Options
		testBinary  bool
		noDirectory bool
	}{
		{name: "test binary", testBinary: true},
		{name: "do not track", opts: Options{Getenv: envOf(map[string]string{EnvDoNotTrack: "1"})}},
		{name: "variable off", opts: Options{Getenv: envOf(map[string]string{EnvTelemetry: "off"})}},
		{name: "config off", opts: Options{Config: ConfigOff}},
		{name: "config unreadable", opts: Options{Config: ConfigUnreadable}},
		{name: "no configuration directory", noDirectory: true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		opts := c.opts
		opts.ConfigDir, opts.DirExisted = dir, !c.noDirectory
		restore := func() {}
		if !c.testBinary {
			restore = SetEnabledForTest()
		}
		got := Record(runEvent(fixedNoon), opts)
		restore()
		want := StatusOff
		if c.noDirectory {
			want = StatusNoDirectory
		}
		assert.Equal(t, want, got, c.name)
		assert.Empty(t, ops.take(), "%s: no file operation", c.name)
		assert.Empty(t, readNames(t, dir), "%s: nothing created", c.name)
	}
}

// TestRecordNeverCreatesTheConfigDirectory asserts a missing configuration
// directory stays missing, whether the caller said it existed or not.
func TestRecordNeverCreatesTheConfigDirectory(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	missing := filepath.Join(t.TempDir(), "rancher")
	assert.Equal(t, StatusNoDirectory, Record(runEvent(fixedNoon), Options{ConfigDir: missing}))
	assert.Equal(t, StatusFailed, Record(runEvent(fixedNoon), Options{ConfigDir: missing, DirExisted: true}))
	_, err := os.Stat(missing)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestSteadyStateFileOperations asserts the file operations of one recorded
// event in the steady state: open the configuration directory, lstat the
// telemetry directory, open it, read the directory id, stat the prune marker,
// read the server facts when a server is set, create the slot, write it,
// close it. The names are the wrapper's operations; the real system-call
// count is higher because os.Root opens each path component.
func TestSteadyStateFileOperations(t *testing.T) {
	enable(t)
	useDistinctSlots(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	ops := observeOps(t)

	base := []string{
		"openroot .", "lstat telemetry", "openroot telemetry",
		"readfile config-directory-id", "stat pruned",
	}
	tail := []string{
		"openfile events/<hour>/<slot>", "write events/<hour>/<slot>", "close events/<hour>/<slot>",
	}
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	assert.Equal(t, slices.Concat(base, tail), shape(ops.take()), "no server: no facts read")

	SetServer("https://rancher.example.com", "")
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	assert.Equal(t, slices.Concat(base, []string{"readfile servers/<facts>"}, tail), shape(ops.take()), "with a server")
}

// TestFirstEventOperations asserts the extra operations the first event of
// a directory makes, and that the hourly pass (which lists the events
// directory) runs once and not again within the hour.
func TestFirstEventOperations(t *testing.T) {
	enable(t)
	useDistinctSlots(t)
	c := useClock(t, fixedNoon)
	dir := t.TempDir()
	ops := observeOps(t)
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	first := shape(ops.take())
	for _, want := range []string{"mkdir telemetry", "openfile config-directory-id", "mkdirall events/<hour>", "readdir events"} {
		assert.Contains(t, first, want)
	}
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	assert.NotContains(t, ops.take(), "readdir events", "the marker is fresh")

	c.set(fixedNoon.Add(61 * time.Minute))
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(c.now())))
	assert.Contains(t, ops.take(), "readdir events", "the marker is older than an hour")
}

// TestPassRefreshesTheMarkerBeforeItRuns asserts the marker's modification
// time is the clock's now by the time the pass starts.
func TestPassRefreshesTheMarkerBeforeItRuns(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	var seen time.Time
	t.Cleanup(SetObserverForTest(func(op string) {
		if op == "readdir events" {
			if info, err := os.Stat(filepath.Join(dir, "telemetry", "pruned")); err == nil {
				seen = info.ModTime()
			}
		}
	}))
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	assert.True(t, seen.Equal(fixedNoon), "marker time %v", seen)
}

// TestFilesAreOwnerOnly asserts the modes: 0700 for the directories and
// 0600 for the files.
func TestFilesAreOwnerOnly(t *testing.T) {
	skipOnWindows(t, "mode bits are not meaningful on NTFS; the access list test covers it")
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	oldCount := slotCount
	slotCount = 1
	t.Cleanup(func() { slotCount = oldCount })
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	require.Equal(t, StatusDropped, record(t, dir, runEvent(fixedNoon)), "the second event leaves a drop marker")
	var f Facts
	f.SetVersion("2.15.2", EditionPrime, fixedNoon)
	require.NoError(t, openStore(t, dir).WriteFacts("https://rancher.example.com", f))
	var kinds []string
	require.NoError(t, filepath.WalkDir(filepath.Join(dir, "telemetry"), func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		info, err := d.Info()
		require.NoError(t, err)
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		assert.Equal(t, want, info.Mode().Perm(), p)
		kinds = append(kinds, filepath.Base(p))
		return nil
	}))
	for _, want := range []string{"servers", fileID, fileMarker, FactsFileName("https://rancher.example.com")} {
		assert.Contains(t, kinds, want, "the walk covered %s", want)
	}
	assert.True(t, slices.ContainsFunc(kinds, func(n string) bool { return strings.HasPrefix(n, droppedPrefix) }), "and a drop marker")
}

// TestSymlinkedTelemetryDirectoryIsRefused asserts a telemetry path that
// is a symlink is never followed: the event is dropped and the target stays
// empty.
func TestSymlinkedTelemetryDirectoryIsRefused(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "telemetry")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	assert.Equal(t, StatusFailed, record(t, dir, runEvent(fixedNoon)))
	assert.Empty(t, readNames(t, outside))

	_, err := OpenStore(dir)
	require.ErrorIs(t, err, errUnsafeDir)
	assert.Empty(t, readNames(t, outside))
}

// TestAccessListCallRunsOnceWhenTheDirectoryIsCreated asserts the hook that
// sets the inheritable owner-only access list runs exactly once, on the
// telemetry directory, when that directory is created, and never again; and
// that a failing call leaves no directory and records nothing.
func TestAccessListCallRunsOnceWhenTheDirectoryIsCreated(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	var calls []string
	old := hardenDir
	hardenDir = func(p string) error {
		calls = append(calls, p)
		return nil
	}
	t.Cleanup(func() { hardenDir = old })
	dir := t.TempDir()
	for range 3 {
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	}
	assert.Equal(t, []string{filepath.Join(dir, "telemetry")}, calls)

	hardenDir = func(string) error { return errors.New("access list refused") }
	failing := t.TempDir()
	assert.Equal(t, StatusFailed, record(t, failing, runEvent(fixedNoon)))
	assert.Empty(t, readNames(t, failing), "a directory without the access list is removed")
}

// TestAnHourNeverHoldsMoreThanTheSlotCount asserts N slots bound an hour:
// with a small N, many writers leave at most N slot files and every other
// event is dropped with a marker; an empty hour never drops.
func TestAnHourNeverHoldsMoreThanTheSlotCount(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()

	for range 100 {
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)), "the default N never drops 100 events")
	}
	assert.Len(t, readNames(t, hourPath(dir, fixedNoon)), 100)

	oldCount := slotCount
	slotCount = 4
	t.Cleanup(func() { slotCount = oldCount })
	small := t.TempDir()
	recorded, dropped := 0, 0
	for range 60 {
		switch record(t, small, runEvent(fixedNoon)) {
		case StatusRecorded:
			recorded++
		case StatusDropped:
			dropped++
		default:
			t.Fatal("unexpected status")
		}
	}
	var slots, markers int
	for _, n := range readNames(t, hourPath(small, fixedNoon)) {
		if isSlotName(n) {
			slots++
		} else {
			markers++
		}
	}
	assert.Equal(t, 4, slots, "N slots and no more")
	assert.Equal(t, recorded, slots)
	assert.Equal(t, dropped, markers, "each drop leaves one marker")
	assert.Positive(t, dropped)
}

// TestADropIsCountedAndAnEmptyHourNeverDrops asserts k taken slots drop the
// event with a zero-byte marker, and the hourly pass turns the markers into
// one dropped event with the count.
func TestADropIsCountedAndAnEmptyHourNeverDrops(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	dir := t.TempDir()
	oldCount := slotCount
	slotCount = 2
	t.Cleanup(func() { slotCount = oldCount })
	hour := fixedNoon.Truncate(time.Hour)
	putSlot(t, dir, 0, runEvent(fixedNoon))
	putSlot(t, dir, 1, runEvent(fixedNoon))
	for range 3 {
		require.Equal(t, StatusDropped, record(t, dir, runEvent(fixedNoon)))
	}
	var markers []string
	for _, n := range readNames(t, hourPath(dir, hour)) {
		if len(n) > len(droppedPrefix) && n[:len(droppedPrefix)] == droppedPrefix {
			markers = append(markers, n)
			info, err := os.Stat(filepath.Join(hourPath(dir, hour), n))
			require.NoError(t, err)
			assert.Zero(t, info.Size())
		}
	}
	require.Len(t, markers, 3)

	c.set(hour.Add(3 * time.Hour))
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	list, err := s.List()
	require.NoError(t, err)
	var counts []float64
	for _, ev := range list {
		if ev.Name == EventEventsDropped {
			counts = append(counts, ev.Fields[attrDroppedCount].(float64))
			assert.Equal(t, hour, ev.Time, "the dropped event is the start of the hour it summarizes")
		}
	}
	assert.Equal(t, []float64{3}, counts)
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

// captureStandardStreams swaps standard output and error for files and
// returns a function that reads what was written.
func captureStandardStreams(t *testing.T) func() string {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "out")
	require.NoError(t, err)
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, out
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		_ = out.Close()
	})
	return func() string {
		data, err := os.ReadFile(out.Name())
		require.NoError(t, err)
		return string(data)
	}
}

// TestInjectedWriteFailuresReturnNoErrorAndPrintNothing asserts a failure
// of the slot write, of the access list call or of the directory create
// ends in a status, never an error, with nothing on standard output or
// error and no half-written slot left.
func TestInjectedWriteFailuresReturnNoErrorAndPrintNothing(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	read := captureStandardStreams(t)

	dir := t.TempDir()
	oldHook := writeHook
	writeHook = func(string, int) error { return errors.New("disk full") }
	t.Cleanup(func() { writeHook = oldHook })
	assert.Equal(t, StatusFailed, record(t, dir, runEvent(fixedNoon)))
	for _, n := range readNames(t, hourPath(dir, fixedNoon)) {
		assert.False(t, isSlotName(n), "the half-written slot %s was removed", n)
	}
	writeHook = oldHook
	assert.Empty(t, read(), "nothing is written to standard output or error")

	skipOnWindows(t, "a read-only directory is not enforced the same way")
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	locked := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(locked, "telemetry"), 0o700))
	require.NoError(t, os.Chmod(filepath.Join(locked, "telemetry"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "telemetry"), 0o700) })
	assert.Equal(t, StatusFailed, record(t, locked, runEvent(fixedNoon)))
	assert.Empty(t, read(), "nothing is written to standard output or error")
}

// TestDoNotTrackDeletesNothing asserts a process that is off through a
// variable leaves every recorded event where it was.
func TestDoNotTrackDeletesNothing(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	before := listTree(t, dir)
	for _, env := range []map[string]string{{EnvDoNotTrack: "1"}, {EnvTelemetry: "off"}} {
		assert.Equal(t, StatusOff, Record(runEvent(fixedNoon), Options{ConfigDir: dir, DirExisted: true, Getenv: envOf(env)}))
	}
	assert.Equal(t, before, listTree(t, dir))
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		rel, _ := filepath.Rel(dir, p)
		out = append(out, rel)
		return nil
	}))
	slices.Sort(out)
	return out
}

// TestDirectoryIDCases asserts the id file's cases: created on the first
// event and kept; an empty file under a minute old leaves the id out of
// that event and stays; an empty file older than that is replaced.
func TestDirectoryIDCases(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	dir := t.TempDir()
	idPath := filepath.Join(dir, "telemetry", "config-directory-id")
	idOf := func(ev Event) any { return decodeEvent(t, ev.stored())[attrConfigDirID] }

	s := openStore(t, dir)
	require.NoError(t, s.ensureDir())
	first := s.directoryID(c.now())
	assert.Regexp(t, randomIDPattern, first, "new and empty: created")
	assert.Equal(t, first, s.directoryID(c.now()), "kept")
	data, err := os.ReadFile(idPath)
	require.NoError(t, err)
	assert.Equal(t, first, string(data))

	require.NoError(t, os.WriteFile(idPath, nil, 0o600))
	require.NoError(t, os.Chtimes(idPath, c.now().Add(-10*time.Second), c.now().Add(-10*time.Second)))
	assert.Empty(t, s.directoryID(c.now()), "an empty id under a minute old: absent")
	info, err := os.Stat(idPath)
	require.NoError(t, err)
	assert.Zero(t, info.Size(), "and left alone")

	require.NoError(t, os.Chtimes(idPath, c.now().Add(-2*time.Minute), c.now().Add(-2*time.Minute)))
	replaced := s.directoryID(c.now())
	assert.Regexp(t, randomIDPattern, replaced, "stale and empty: created again")
	assert.NotEqual(t, first, replaced)
	assert.Equal(t, replaced, s.directoryID(c.now()))

	require.NoError(t, os.WriteFile(idPath, []byte("half-writ"), 0o600))
	require.NoError(t, os.Chtimes(idPath, c.now().Add(-2*time.Minute), c.now().Add(-2*time.Minute)))
	assert.Regexp(t, randomIDPattern, s.directoryID(c.now()), "a stale partial id is replaced too")

	var ev Event = runEvent(fixedNoon)
	ev = ev.withDirectoryID("")
	assert.Nil(t, idOf(ev), "an absent id is left out of the event")
}

// TestTwoProcessesReplacingAStaleIDKeepOne asserts the stale-id race: a
// process that read the stale empty file and paused sees the winner's id when
// it resumes and uses it; both events carry the one id the directory
// keeps.
func TestTwoProcessesReplacingAStaleIDKeepOne(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	dir := t.TempDir()
	idPath := filepath.Join(dir, "telemetry", "config-directory-id")
	require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o700))
	require.NoError(t, os.WriteFile(idPath, nil, 0o600))
	stale := c.now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(idPath, stale, stale))

	reached := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	oldPause := pauseHook
	pauseHook = func(stage string) {
		if stage == "stale-id" && first.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { pauseHook = oldPause })

	var slow Event = runEvent(fixedNoon)
	var wg sync.WaitGroup
	wg.Go(func() { record(t, dir, slow) })
	<-reached
	fast := runEvent(fixedNoon)
	require.Equal(t, StatusRecorded, record(t, dir, fast))
	close(release)
	wg.Wait()

	data, err := os.ReadFile(idPath)
	require.NoError(t, err)
	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, ev := range list {
		assert.Equal(t, string(data), ev.Fields[attrConfigDirID], "every event carries the surviving id")
	}
}

// TestManyProcessesRacingOnAStaleIDEndWithOne asserts that after any
// number of processes race to replace a stale empty id, exactly one valid id
// is on disk and every later event carries it.
func TestManyProcessesRacingOnAStaleIDEndWithOne(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	dir := t.TempDir()
	idPath := filepath.Join(dir, "telemetry", "config-directory-id")
	require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o700))
	require.NoError(t, os.WriteFile(idPath, nil, 0o600))
	stale := c.now().Add(-5 * time.Minute)
	require.NoError(t, os.Chtimes(idPath, stale, stale))

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { record(t, dir, runEvent(fixedNoon)) })
	}
	wg.Wait()

	data, err := os.ReadFile(idPath)
	require.NoError(t, err)
	assert.Regexp(t, randomIDPattern, string(data))
	later := runEvent(fixedNoon)
	require.Equal(t, StatusRecorded, record(t, dir, later))
	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	for _, ev := range list {
		if ev.UID == later.UID() {
			assert.Equal(t, string(data), ev.Fields[attrConfigDirID])
		}
	}
}

// TestReadersSkipWhatTheyCannotParseAndRemoveDuplicates asserts List and
// Count skip an empty slot, a truncated slot and JSON that is not an event,
// and return a record id once.
func TestReadersSkipWhatTheyCannotParseAndRemoveDuplicates(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	a, b := runEvent(fixedNoon), runEvent(fixedNoon)
	putSlot(t, dir, 0, a)
	putSlot(t, dir, 1, b)
	putSlot(t, dir, 2, a)
	p := hourPath(dir, fixedNoon)
	require.NoError(t, os.WriteFile(filepath.Join(p, "0003.json"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0004.json"), a.stored()[:20], 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0005.json"), []byte(`{"hello":"world"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0006.json"), []byte(`[1,2]`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p, "notes.txt"), []byte("x"), 0o600))

	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	var uids []string
	for _, ev := range list {
		uids = append(uids, ev.UID)
	}
	assert.ElementsMatch(t, []string{a.UID(), b.UID()}, uids)
	n, err := s.Count(CountLimit)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = s.Count(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "Count stops at its limit")
}

// TestResetDeletesEventsFactsAndIDOnly asserts Reset removes the events,
// the facts and the directory id, keeps every other file, and does nothing
// when there is no telemetry directory.
func TestResetDeletesEventsFactsAndIDOnly(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("x: 1\n"), 0o600))
	require.NoError(t, openStoreNoCleanup(t, dir).Reset(), "no telemetry directory: nothing to do")

	SetServer("https://rancher.example.com", "")
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	s := openStore(t, dir)
	var f Facts
	f.SetVersion("2.15.2", EditionPrime, fixedNoon)
	require.NoError(t, s.WriteFacts("https://rancher.example.com", f))
	require.NoError(t, s.Reset())

	tree := listTree(t, dir)
	assert.Contains(t, tree, "config.yaml")
	assert.Contains(t, tree, filepath.Join("telemetry", "pruned"))
	assert.NotContains(t, tree, filepath.Join("telemetry", "events"))
	assert.NotContains(t, tree, filepath.Join("telemetry", "servers"))
	assert.NotContains(t, tree, filepath.Join("telemetry", "config-directory-id"))
}

func openStoreNoCleanup(t *testing.T, dir string) *Store {
	t.Helper()
	return openStore(t, dir)
}

// TestASharingViolationReadIsRetriedOnce asserts a read that fails
// with a sharing violation is retried once, and a second failure skips the
// file.
func TestASharingViolationReadIsRetriedOnce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", "f"), []byte("ok"), 0o600))
	s := openStore(t, dir)
	oldBusy, oldDelay := isBusy, retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { isBusy, retryDelay = oldBusy, oldDelay })
	calls := 0
	isBusy = func(error) bool { calls++; return true }

	_, err := s.readFile("missing")
	require.Error(t, err)
	assert.Equal(t, 1, calls, "retried once, then gave up")

	isBusy = func(error) bool { return false }
	data, err := s.readFile("f")
	require.NoError(t, err)
	assert.Equal(t, "ok", string(data))
}
