package telemetry

import (
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

	"github.com/rancher/rancher-cli/internal/testfs"
)

func jsonlNames(t *testing.T, dir string, hour time.Time) []string {
	t.Helper()
	var out []string
	for _, n := range readNames(t, hourPath(dir, hour)) {
		if strings.HasSuffix(n, jsonlSuffix) {
			out = append(out, n)
		}
	}
	return out
}

func slotNames(t *testing.T, dir string, hour time.Time) []string {
	t.Helper()
	var out []string
	for _, n := range readNames(t, hourPath(dir, hour)) {
		if isSlotName(n) {
			out = append(out, n)
		}
	}
	return out
}

func uidsOf(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	var out []string
	for _, ev := range list {
		out = append(out, ev.UID)
	}
	return out
}

// oldMarkerFile makes a drop marker file in an hour directory.
func putMarker(t *testing.T, dir string, hour time.Time, name string) {
	t.Helper()
	p := hourPath(dir, hour)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, droppedPrefix+name), nil, 0o600))
}

// TestPassCompactsOneFinishedHourPerPass asserts a pass compacts the oldest
// finished hour only: its slots become one file and are removed, and the
// next hour waits for the next pass; the current hour is never touched.
func TestPassCompactsOneFinishedHourPerPass(t *testing.T) {
	enable(t)
	now := realHour(0).Add(30 * time.Minute)
	c := useClock(t, now)
	dir := t.TempDir()
	h3, h2, h1 := realHour(3*time.Hour), realHour(2*time.Hour), realHour(time.Hour)
	var all []string
	for _, h := range []time.Time{h3, h2, h1, now} {
		for i := range 3 {
			ev := runEvent(h)
			putSlot(t, dir, i, ev)
			all = append(all, ev.UID())
		}
	}
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{hourName(h3) + ".jsonl"}, jsonlNames(t, dir, h3))
	assert.Empty(t, slotNames(t, dir, h3))
	assert.Len(t, slotNames(t, dir, h2), 3, "the next hour waits")
	assert.Len(t, slotNames(t, dir, h1), 3)

	s.pass(c.now(), newProducer(), "")
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h1))
	assert.Len(t, slotNames(t, dir, now), 3, "the current hour is never compacted")
	assert.Empty(t, jsonlNames(t, dir, now))
	assert.ElementsMatch(t, all, uidsOf(t, s), "no event is lost")
}

// TestCompactedHourFileHasOneEventPerLineAndIsOwnerOnly asserts the format
// of the compacted file.
func TestCompactedHourFileHasOneEventPerLineAndIsOwnerOnly(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(2 * time.Hour)
	for i := range 4 {
		putSlot(t, dir, i, runEvent(h))
	}
	openStore(t, dir).pass(c.now(), newProducer(), "")
	file := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	assert.Len(t, lines, 4)
	for _, l := range lines {
		assert.Empty(t, schemaValidator(t).errors(stripLocal(t, l)))
	}
	if !testfs.OnWindows() {
		info, err := os.Stat(file)
		require.NoError(t, err)
		assert.EqualValues(t, 0o600, info.Mode().Perm())
	}
}

func stripLocal(t *testing.T, line string) any {
	t.Helper()
	m := decodeJSON(t, []byte(line)).(map[string]any)
	delete(m, keyLocalFormat)
	return m
}

// TestConcurrentCompactionLosesAndDuplicatesNothing asserts several
// processes compacting one full hour at the same time lose no event, write
// none twice, and count the drops once.
func TestConcurrentCompactionLosesAndDuplicatesNothing(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	var want []string
	for i := range 300 {
		ev := runEvent(h)
		putSlot(t, dir, i, ev)
		want = append(want, ev.UID())
	}
	for i := range 5 {
		putMarker(t, dir, h, strings.Repeat(string(rune('a'+i)), 8))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s, err := OpenStore(dir)
			if err != nil {
				return
			}
			defer func() { _ = s.Close() }()
			s.pass(c.now(), newProducer(), "")
		})
	}
	wg.Wait()

	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	var runs, drops int
	var counted float64
	for _, ev := range list {
		if ev.Name == EventEventsDropped {
			drops++
			counted += ev.Fields[attrDroppedCount].(float64)
		} else {
			runs++
		}
	}
	assert.Equal(t, len(want), runs, "no event lost")
	assert.Equal(t, 1, drops, "one dropped event")
	assert.EqualValues(t, 5, counted, "no drop counted twice")
	assert.Equal(t, []string{hourName(h) + ".jsonl"}, jsonlNames(t, dir, h), "one file")
	assert.ElementsMatch(t, want, slices.DeleteFunc(uidsOf(t, s), func(uid string) bool { return !slices.Contains(want, uid) }))
	total := 0
	for _, n := range readNames(t, hourPath(dir, h)) {
		if strings.HasPrefix(n, droppedPrefix) {
			total++
		}
	}
	assert.Zero(t, total, "the counted markers are gone")
	if !testfs.OnWindows() {
		// On Windows a slot another process holds open cannot be removed
		// and is left for a later pass; the reader removes its duplicate.
		assert.Empty(t, slotNames(t, dir, h))
	}
}

// TestPausedWriterKeepsItsSlot asserts a writer paused between creating
// its slot and writing it keeps the slot through a pass after the hour
// ended: the empty slot is not removed before an hour past the end of its
// hour, and the event is compacted once it is written.
func TestPausedWriterKeepsItsSlot(t *testing.T) {
	enable(t)
	h := realHour(10 * time.Hour)
	c := useClock(t, h.Add(30*time.Minute))
	dir := t.TempDir()
	other := runEvent(h)
	putSlot(t, dir, 1, other)

	reached := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	oldPause := pauseHook
	pauseHook = func(stage string) {
		if stage == "created" && once.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { pauseHook = oldPause })

	slow := runEvent(h.Add(30 * time.Minute))
	var wg sync.WaitGroup
	wg.Go(func() { record(t, dir, slow) })
	<-reached

	// The pass runs half an hour after the hour ended.
	c.set(h.Add(90 * time.Minute))
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{hourName(h) + ".jsonl"}, jsonlNames(t, dir, h), "the written event was compacted")
	require.Len(t, slotNames(t, dir, h), 1, "the paused writer's empty slot stays")
	empty := filepath.Join(hourPath(dir, h), slotNames(t, dir, h)[0])
	info, err := os.Stat(empty)
	require.NoError(t, err)
	assert.Zero(t, info.Size())

	close(release)
	wg.Wait()
	data, err := os.ReadFile(empty)
	require.NoError(t, err)
	assert.Contains(t, string(data), slow.UID(), "the writer wrote into its slot")

	// A later pass, with the main file old enough, adds the late slot once.
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	require.NoError(t, os.Chtimes(mainFile, c.now().Add(-time.Hour), c.now().Add(-time.Hour)))
	c.set(h.Add(5 * time.Hour))
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Equal(t, []string{hourName(h) + "-1.jsonl", hourName(h) + ".jsonl"}, jsonlNames(t, dir, h))
	assert.ElementsMatch(t, []string{other.UID(), slow.UID()}, uidsOf(t, s), "each event once")
}

// TestAnUnparsableSlotIsRemovedOnlyAfterItsSettlingTime asserts an empty
// and a truncated slot are kept until an hour past the end of their hour,
// then removed by the pass; readers skip them meanwhile.
func TestAnUnparsableSlotIsRemovedOnlyAfterItsSettlingTime(t *testing.T) {
	enable(t)
	h := realHour(10 * time.Hour)
	c := useClock(t, h.Add(90*time.Minute))
	dir := t.TempDir()
	putSlot(t, dir, 0, runEvent(h))
	p := hourPath(dir, h)
	require.NoError(t, os.WriteFile(filepath.Join(p, "0001.json"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0002.json"), []byte(`{"event.name":"io.rancher.cli.comm`), 0o600))
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{"0001.json", "0002.json"}, slotNames(t, dir, h))
	assert.Len(t, uidsOf(t, s), 1)

	c.set(h.Add(2 * time.Hour))
	mainFile := filepath.Join(p, hourName(h)+".jsonl")
	require.NoError(t, os.Chtimes(mainFile, c.now().Add(-time.Hour), c.now().Add(-time.Hour)))
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h), "removed once an hour past the end of its hour")
	assert.Len(t, uidsOf(t, s), 1)
}

// TestAFailedHourFileWriteKeepsEverySlot asserts that when writing the
// compacted file fails, no slot is removed and no partial file stays; the
// next pass compacts everything.
func TestAFailedHourFileWriteKeepsEverySlot(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	for i := range 5 {
		putSlot(t, dir, i, runEvent(h))
	}
	putMarker(t, dir, h, "aaaaaaaa")
	oldHook := writeHook
	writeHook = func(name string, _ int) error {
		if strings.HasSuffix(name, jsonlSuffix) {
			return os.ErrClosed
		}
		return nil
	}
	t.Cleanup(func() { writeHook = oldHook })
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Len(t, slotNames(t, dir, h), 5, "every slot kept")
	assert.Empty(t, jsonlNames(t, dir, h), "no partial file left")
	assert.Contains(t, readNames(t, hourPath(dir, h)), droppedPrefix+"aaaaaaaa", "the marker is kept")

	writeHook = oldHook
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Len(t, jsonlNames(t, dir, h), 1)
	assert.Len(t, uidsOf(t, s), 6, "five events and the dropped event")
}

// TestATruncatedHourFileIsRecoveredOnce asserts a compacted file cut short
// by a crash, with its slots still on disk, ends with every event once: the
// slots already in the file are removed, the others go into a later file, an
// unparsable line is skipped. The drop markers the crash left are counted
// again in the later file; that is the documented residual.
func TestATruncatedHourFileIsRecoveredOnce(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(5 * time.Hour)
	evs := make([]CommandRun, 4)
	for i := range evs {
		evs[i] = runEvent(h)
		putSlot(t, dir, i, evs[i])
	}
	dropped := newEventsDropped(h, 2, newProducer())
	putMarker(t, dir, h, "aaaaaaaa")
	putMarker(t, dir, h, "bbbbbbbb")
	partial := string(evs[2].stored())
	content := string(evs[0].stored()) + "\n" + string(evs[1].stored()) + "\n" + string(dropped.stored()) + "\n" + partial[:len(partial)/2]
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	require.NoError(t, os.WriteFile(mainFile, []byte(content), 0o600))
	old := c.now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mainFile, old, old))

	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Equal(t, []string{hourName(h) + "-1.jsonl", hourName(h) + ".jsonl"}, jsonlNames(t, dir, h))
	list, err := s.List()
	require.NoError(t, err)
	var runs []string
	dropped2 := 0
	for _, ev := range list {
		if ev.Name == EventEventsDropped {
			dropped2++
		} else {
			runs = append(runs, ev.UID)
		}
	}
	assert.ElementsMatch(t, []string{evs[0].UID(), evs[1].UID(), evs[2].UID(), evs[3].UID()}, runs, "each event once")
	assert.Equal(t, 2, dropped2, "the crash residual: the markers the first file already counted are counted again in the late file")
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 4, n, "a documented residual: a crash between a file write and its removals can count drops twice")
	for _, name := range readNames(t, hourPath(dir, h)) {
		assert.False(t, strings.HasPrefix(name, droppedPrefix), "markers removed")
	}
}

// TestAYoungHourFileMeansAnotherProcessIsCompacting asserts a pass that
// finds the hour file less than a minute old stops and changes nothing.
func TestAYoungHourFileMeansAnotherProcessIsCompacting(t *testing.T) {
	enable(t)
	c := useClock(t, time.Now().UTC())
	dir := t.TempDir()
	h := realHour(5 * time.Hour)
	putSlot(t, dir, 0, runEvent(h))
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	require.NoError(t, os.WriteFile(mainFile, nil, 0o600))
	openStore(t, dir).pass(c.now(), newProducer(), "")
	assert.Len(t, slotNames(t, dir, h), 1)
	assert.Equal(t, []string{hourName(h) + ".jsonl"}, jsonlNames(t, dir, h))
}

// TestPruneBounds asserts a pass removes at most two compacted hours older
// than the age cap, oldest first, never a newer one, never the current
// hour, and at most eight facts files past 30 days.
func TestPruneBounds(t *testing.T) {
	enable(t)
	now := realHour(0).Add(20 * time.Minute)
	c := useClock(t, now)
	dir := t.TempDir()
	var old []time.Time
	for i := range 10 {
		h := realHour(time.Duration(80+i) * time.Hour)
		old = append(old, h)
		ev := runEvent(h)
		p := hourPath(dir, h)
		require.NoError(t, os.MkdirAll(p, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(p, hourName(h)+".jsonl"), append(ev.stored(), '\n'), 0o600))
	}
	young := realHour(71 * time.Hour)
	current := now.Truncate(time.Hour)
	for _, h := range []time.Time{young, current} {
		p := hourPath(dir, h)
		require.NoError(t, os.MkdirAll(p, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(p, hourName(h)+".jsonl"), []byte("\n"), 0o600))
	}
	s := openStore(t, dir)
	remaining := func() int {
		n := 0
		for _, h := range old {
			if _, err := os.Stat(hourPath(dir, h)); err == nil {
				n++
			}
		}
		return n
	}
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, 8, remaining(), "two hours per pass")
	_, err := os.Stat(hourPath(dir, old[9]))
	require.ErrorIs(t, err, os.ErrNotExist, "the oldest went first")
	_, err = os.Stat(hourPath(dir, old[8]))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(hourPath(dir, old[7]))
	require.NoError(t, err)
	for range 8 {
		s.pass(c.now(), newProducer(), "")
	}
	assert.Zero(t, remaining())
	for _, h := range []time.Time{young, current} {
		_, err := os.Stat(hourPath(dir, h))
		require.NoError(t, err, "%s is not past the age cap or is current", hourName(h))
	}

	for i := range 20 {
		var f Facts
		f.SetVersion("2.15.2", EditionPrime, now.Add(-31*24*time.Hour))
		require.NoError(t, s.WriteFacts("https://old"+string(rune('a'+i))+".example", f))
	}
	var fresh Facts
	fresh.SetVersion("2.15.2", EditionPrime, now.Add(-24*time.Hour))
	require.NoError(t, s.WriteFacts("https://fresh.example", fresh))
	s.pass(c.now(), newProducer(), "")
	assert.Len(t, readNames(t, filepath.Join(dir, "telemetry", "servers")), 13, "eight removed")
	for range 2 {
		s.pass(c.now(), newProducer(), "")
	}
	assert.Equal(t, []string{FactsFileName("https://fresh.example")}, readNames(t, filepath.Join(dir, "telemetry", "servers")))
}

// TestCurrentHourIsNeverPrunedOrCompacted asserts the current hour's
// slots and a current-hour compacted file survive a pass.
func TestCurrentHourIsNeverPrunedOrCompacted(t *testing.T) {
	enable(t)
	now := realHour(0).Add(40 * time.Minute)
	c := useClock(t, now)
	dir := t.TempDir()
	cur := now.Truncate(time.Hour)
	putSlot(t, dir, 0, runEvent(now))
	p := hourPath(dir, cur)
	require.NoError(t, os.WriteFile(filepath.Join(p, hourName(cur)+".jsonl"), []byte("\n"), 0o600))
	s := openStore(t, dir)
	for range 3 {
		s.pass(c.now(), newProducer(), "")
	}
	assert.Len(t, slotNames(t, dir, cur), 1)
	assert.Len(t, jsonlNames(t, dir, cur), 1)
}

// TestPassToleratesDirectoriesThatDisappear asserts a hook that removes an
// hour directory mid-pass does not stop the pass or panic.
func TestPassToleratesDirectoriesThatDisappear(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	putSlot(t, dir, 0, runEvent(h))
	s := openStore(t, dir)
	oldHook := writeHook
	writeHook = func(string, int) error {
		_ = os.RemoveAll(hourPath(dir, h))
		return nil
	}
	t.Cleanup(func() { writeHook = oldHook })
	assert.NotPanics(t, func() { s.pass(c.now(), newProducer(), "") })
}

// TestRecordPrunesOnlyWhileOn asserts the hourly pass runs from Record
// when recording is on, and never when a control turned it off.
func TestRecordPrunesOnlyWhileOn(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(100 * time.Hour)
	p := hourPath(dir, h)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, hourName(h)+".jsonl"), []byte("\n"), 0o600))

	_, _, _ = Record(runEvent(c.now()), Options{ConfigDir: dir, DirExisted: true, Getenv: envOf(map[string]string{EnvTelemetry: "false"})})
	_, err := os.Stat(p)
	require.NoError(t, err, "off: nothing pruned")
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(c.now())))
	_, err = os.Stat(p)
	require.ErrorIs(t, err, os.ErrNotExist, "on: the old hour is pruned")
}
