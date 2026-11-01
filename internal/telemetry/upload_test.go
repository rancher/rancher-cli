package telemetry

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// useSeam replaces a seam for the test and restores it afterwards. The
// calling test must not use t.Parallel().
func useSeam[T any](t *testing.T, seam *T, value T) {
	t.Helper()
	old := *seam
	*seam = value
	t.Cleanup(func() { *seam = old })
}

// uploadNow is an exact hour, so an age of whole hours is exact.
func uploadNow() time.Time { return fixedNoon.Truncate(time.Hour) }

// hoursAgo is the start of the hour n hours before the exact hour now.
func hoursAgo(now time.Time, n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }

// telemetryDir returns a configuration directory with an empty telemetry
// directory.
func telemetryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))
	return dir
}

// putRaw writes data as a file of an hour directory, with the modification
// time mtime, and returns its slash path below the telemetry directory.
func putRaw(t *testing.T, dir string, hour time.Time, name string, mtime time.Time, data []byte) string {
	t.Helper()
	p := hourPath(dir, hour)
	require.NoError(t, os.MkdirAll(p, 0o700))
	file := filepath.Join(p, name)
	require.NoError(t, os.WriteFile(file, data, 0o600))
	require.NoError(t, os.Chtimes(file, mtime, mtime))
	return path.Join("events", hourName(hour), name)
}

// putCompacted writes a compacted file that holds the events one per line, and
// returns its slash path below the telemetry directory.
func putCompacted(t *testing.T, dir string, hour time.Time, name string, mtime time.Time, events ...Event) string {
	t.Helper()
	var lines [][]byte
	for _, ev := range events {
		lines = append(lines, ev.stored())
	}
	return putRaw(t, dir, hour, name, mtime, append(bytes.Join(lines, []byte{'\n'}), '\n'))
}

func hourFile(hour time.Time) string { return hourName(hour) + jsonlSuffix }

func slotPath(hour time.Time, n int) string {
	return path.Join("events", hourName(hour), slotFileName(n))
}

func eventUIDs(events []StoredEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.UID
	}
	return out
}

func uidsOfEvents(events ...Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.UID()
	}
	return out
}

func idSet(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func mustReady(t *testing.T, s *Store, opts ReadyOptions) *ReadyList {
	t.Helper()
	list, err := s.Ready(opts)
	require.NoError(t, err)
	return list
}

// coveredPaths returns the paths a coverage holds, sorted.
func coveredPaths(c Coverage) []string {
	var out []string
	for _, f := range c.files {
		out = append(out, f.path)
	}
	slices.Sort(out)
	return out
}

// without returns the tree minus the files below telemetry/ named by removed.
func without(tree []string, removed ...string) []string {
	var out []string
	for _, p := range tree {
		if !slices.Contains(removed, strings.TrimPrefix(p, "telemetry/")) {
			out = append(out, p)
		}
	}
	return out
}

// TestSlotEventsOfAFinishedHourAreSent asserts an event still in a slot of a
// finished hour is listed, an event in both a slot and a compacted file of
// the hour is listed once from the compacted file, covering its id covers both
// files, and an unparsable slot is neither listed nor covered.
func TestSlotEventsOfAFinishedHourAreSent(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	hA, hB, hC := hoursAgo(now, 3), hoursAgo(now, 2), hoursAgo(now, 1)
	a, b, c := runEvent(hA), runEvent(hB), runEvent(hC)
	old := now.Add(-time.Hour)
	putSlot(t, dir, 1, a)
	putRaw(t, dir, hA, slotFileName(9), old, []byte(`{"truncated`))
	fileB := putCompacted(t, dir, hB, hourFile(hB), old, b)
	putSlot(t, dir, 2, b)
	fileC := putCompacted(t, dir, hC, hourFile(hC), old, c)
	s := openStore(t, dir)

	list := mustReady(t, s, ReadyOptions{})
	assert.Equal(t, uidsOfEvents(a, b, c), eventUIDs(list.Events), "the duplicate of b is listed once, the bad slot never")

	covered := list.Cover(idSet(b.UID()))
	assert.ElementsMatch(t, []string{fileB, slotPath(hB, 2)}, coveredPaths(covered), "covering b covers its file and its slot")

	all := list.Cover(idSet(a.UID(), b.UID(), c.UID()))
	assert.ElementsMatch(t, []string{fileB, fileC, slotPath(hA, 1), slotPath(hB, 2)}, coveredPaths(all), "the unparsable slot is never covered")

	before := testfs.Tree(t, dir)
	res := s.DeleteCovered(all)
	assert.Empty(t, res.Waiting)
	assert.Empty(t, res.Refused)
	assert.Equal(t, without(before, fileB, fileC, slotPath(hA, 1), slotPath(hB, 2)), testfs.Tree(t, dir))
	assert.Contains(t, testfs.Tree(t, dir), "telemetry/"+path.Join("events", hourName(hA), slotFileName(9)))
}

// TestDeleteOnlyAfterSuccess asserts a deletion removes exactly the files the
// coverage holds and leaves every other file, and the checks it repeats at
// run time.
func TestDeleteOnlyAfterSuccess(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	old := now.Add(-time.Hour)
	h1, h2, h3, h4 := hoursAgo(now, 6), hoursAgo(now, 5), hoursAgo(now, 4), hoursAgo(now, 3)
	var e []Event
	for _, h := range []time.Time{h1, h1, h1, h2, h2, h2, h3, h3, h3, h3} {
		e = append(e, runEvent(h))
	}
	f1 := putCompacted(t, dir, h1, hourFile(h1), old, e[0], e[1])
	putSlot(t, dir, 3, e[2])
	slot3 := slotPath(h1, 3)
	f2 := putCompacted(t, dir, h2, hourFile(h2), old, e[3], e[4])
	putSlot(t, dir, 5, e[5])
	cut := string(e[6].stored()) + "\n" + string(e[7].stored()) + "\n" + `{"event.na`
	f3 := putRaw(t, dir, h3, hourFile(h3), old, []byte(cut))
	middle := string(e[8].stored()) + "\nnot json\n" + string(e[9].stored()) + "\n"
	f4 := putRaw(t, dir, h3, hourName(h3)+"-1.jsonl", old, []byte(middle))
	putRaw(t, dir, h3, hourName(h3)+"-2.jsonl", old, []byte("not json\n"))
	big := putRaw(t, dir, h4, hourFile(h4), old, nil)
	require.NoError(t, os.Truncate(filepath.Join(hourPath(dir, h4), hourFile(h4)), maxHourFileSize+1))
	s := openStore(t, dir)

	list := mustReady(t, s, ReadyOptions{})
	sent := idSet(uidsOfEvents(e[0], e[1], e[2], e[4], e[5], e[6], e[7], e[8], e[9])...)
	cov := list.Cover(sent)
	assert.ElementsMatch(t, []string{f1, f3, slot3, slotPath(h2, 5)}, coveredPaths(cov),
		"f2 holds an event that was not sent, the middle bad line keeps f4, a cut last line does not keep f3")
	assert.NotContains(t, coveredPaths(cov), big, "a file over the size limit is not in the result")

	before := testfs.Tree(t, dir)
	require.NoError(t, os.Remove(filepath.Join(hourPath(dir, h1), hourFile(h1))))
	res := s.DeleteCovered(cov)
	assert.ElementsMatch(t, []string{f1, f3, slot3, slotPath(h2, 5)}, res.Deleted, "a file already gone counts as deleted")
	assert.Empty(t, res.Waiting)
	assert.Empty(t, res.Refused)
	assert.Equal(t, without(before, f1, f3, slot3, slotPath(h2, 5)), testfs.Tree(t, dir))
	assert.Contains(t, testfs.Tree(t, dir), "telemetry/"+f2)
	assert.Contains(t, testfs.Tree(t, dir), "telemetry/"+f4)
}

// TestDeleteLeavesASlotThatChanged asserts a covered slot that now holds
// another event stays and is reported waiting.
func TestDeleteLeavesASlotThatChanged(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	h := hoursAgo(now, 3)
	first, other := runEvent(h), runEvent(h)
	putSlot(t, dir, 4, first)
	s := openStore(t, dir)
	list := mustReady(t, s, ReadyOptions{})
	cov := list.Cover(idSet(first.UID()))
	require.Equal(t, []string{slotPath(h, 4)}, coveredPaths(cov))

	putSlot(t, dir, 4, other)
	res := s.DeleteCovered(cov)
	assert.Equal(t, []string{slotPath(h, 4)}, res.Waiting)
	assert.Empty(t, res.Deleted)
	assert.Equal(t, []string{other.UID()}, uidsOf(t, s), "the slot with the other event stays")
}

// TestDeleteRefusesPathsThatAreNotEventFiles asserts a coverage built around a
// path that is not a slot or a compacted file of an hour is refused at run
// time and the path is left untouched.
func TestDeleteRefusesPathsThatAreNotEventFiles(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	h := hoursAgo(now, 3)
	hour := hourName(h)
	hourDirPath := hourPath(dir, h)
	putFile := func(rel string) {
		file := filepath.Join(dir, "telemetry", filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte("keep"), 0o600))
	}
	for _, rel := range []string{
		"config-directory-id", "pruned", "servers/0123456789abcdef0123456789abcdef.json",
		"upload/state.json", "events/" + hour + "/dropped-0123456789abcdef",
		"events/" + hour + "/" + hour + "-01.jsonl", "events/" + hour + "/" + hour + "-x.jsonl",
		"events/" + hour + "/" + hour + "-0.jsonl", "events/" + hour + "/" + hour + "x.jsonl",
		"events/" + hour + "/notes.txt",
	} {
		putFile(rel)
	}
	require.NoError(t, os.MkdirAll(filepath.Join(hourDirPath, "0001.json"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(hourDirPath, "0001.json", "inner"), []byte("keep"), 0o600))
	s := openStore(t, dir)

	refused := []string{
		"config-directory-id", "pruned", "servers/0123456789abcdef0123456789abcdef.json",
		"upload/state.json", "events/" + hour + "/dropped-0123456789abcdef",
		"events/" + hour, "events/" + hour + "/../../pruned", "events/" + hour + "/0001.json/",
		`events\` + hour + `\0001.json`, "events/" + hour + "/0001.json:x", "events/" + hour + "/0001.json.",
		"events/" + hour + "/" + hour + "-01.jsonl", "events/" + hour + "/" + hour + "-x.jsonl",
		"events/" + hour + "/" + hour + "-0.jsonl", "events/" + hour + "/" + hour + "x.jsonl",
		"events/" + hour + "/notes.txt", "events/notanhour/0001.json", "events/" + hour + "/0001.json/inner",
		"events", "", ".", "/events/" + hour + "/0001.json",
	}
	before := testfs.Snapshot(t, dir)
	for _, p := range refused {
		res := s.DeleteCovered(Coverage{dir: s.configDir, files: []coveredFile{{path: p, id: "x"}}})
		assert.Empty(t, res.Deleted, p)
		assert.Equal(t, []string{p}, res.Refused, p)
		assert.Equal(t, before, testfs.Snapshot(t, dir), "nothing changed: %q", p)
	}

	t.Run("a directory named like a slot", func(t *testing.T) {
		res := s.DeleteCovered(Coverage{dir: s.configDir, files: []coveredFile{{path: slotPath(h, 1), id: "x"}}})
		assert.Equal(t, []string{slotPath(h, 1)}, res.Refused)
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})

	t.Run("a symlink named like a slot", func(t *testing.T) {
		testfs.Symlink(t, filepath.Join(dir, "telemetry", "pruned"), filepath.Join(hourDirPath, slotFileName(2)))
		res := s.DeleteCovered(Coverage{dir: s.configDir, files: []coveredFile{{path: slotPath(h, 2), id: "x"}}})
		assert.Equal(t, []string{slotPath(h, 2)}, res.Refused)
		assert.FileExists(t, filepath.Join(dir, "telemetry", "pruned"))
	})
}

// TestDeleteRefusesACoverageOfAnotherDirectory asserts a coverage made from
// the list of one configuration directory removes nothing through a store on
// another directory, even when the same file names exist there, and every
// path is reported as refused, while another store on the directory of the
// list deletes the same coverage.
func TestDeleteRefusesACoverageOfAnotherDirectory(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	h := hoursAgo(now, 3)
	old := now.Add(-time.Hour)
	ev := runEvent(h)
	listed, other := t.TempDir(), t.TempDir()
	file := putCompacted(t, listed, h, hourFile(h), old, ev)
	putSlot(t, listed, 1, ev)
	putCompacted(t, other, h, hourFile(h), old, runEvent(h))
	putRaw(t, other, h, slotFileName(1), old, runEvent(h).stored())
	a, b := openStore(t, listed), openStore(t, other)
	cov := mustReady(t, a, ReadyOptions{}).Cover(idSet(ev.UID()))
	require.ElementsMatch(t, []string{file, slotPath(h, 1)}, coveredPaths(cov))

	beforeListed, beforeOther := testfs.Snapshot(t, listed), testfs.Snapshot(t, other)
	res := b.DeleteCovered(cov)
	assert.Empty(t, res.Deleted)
	assert.Empty(t, res.Waiting)
	assert.ElementsMatch(t, []string{file, slotPath(h, 1)}, res.Refused)
	assert.Equal(t, beforeListed, testfs.Snapshot(t, listed))
	assert.Equal(t, beforeOther, testfs.Snapshot(t, other))

	again := openStore(t, listed)
	res = again.DeleteCovered(cov)
	assert.ElementsMatch(t, []string{file, slotPath(h, 1)}, res.Deleted, "another store on the same directory deletes")
}

// TestDeleteReportsAFileItCannotRemoveAsWaiting asserts a covered file whose
// removal fails stays and is reported as waiting while the other covered
// files are removed, and no error is returned for the call.
func TestDeleteReportsAFileItCannotRemoveAsWaiting(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	h := hoursAgo(now, 3)
	a, b := runEvent(h), runEvent(h)
	putSlot(t, dir, 1, a)
	putSlot(t, dir, 2, b)
	s := openStore(t, dir)
	cov := mustReady(t, s, ReadyOptions{}).Cover(idSet(a.UID(), b.UID()))
	useSeam(t, &removeFileHook, func(name string) error {
		if name == slotPath(h, 1) {
			return os.ErrPermission
		}
		return nil
	})
	res := s.DeleteCovered(cov)
	assert.Equal(t, []string{slotPath(h, 1)}, res.Waiting)
	assert.Equal(t, []string{slotPath(h, 2)}, res.Deleted)
	assert.Empty(t, res.Refused)
	assert.Equal(t, []string{slotFileName(1)}, slotNames(t, dir, h), "the file that could not be removed stays")
}

// TestDeleteOnlyReachesEventFilesOfTheCoverage asserts a coverage only ever
// holds files the list read.
func TestDeleteOnlyReachesEventFilesOfTheCoverage(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	h := hoursAgo(now, 3)
	ev := runEvent(h)
	putSlot(t, dir, 1, ev)
	putMarker(t, dir, h, "0123456789abcdef")
	other := putRaw(t, dir, h, "0002.json", now, []byte(`{"unrelated":true}`))
	s := openStore(t, dir)
	list := mustReady(t, s, ReadyOptions{})
	cov := list.Cover(idSet(ev.UID(), "an-id-that-is-not-listed"))
	assert.ElementsMatch(t, []string{slotPath(h, 1)}, coveredPaths(cov))
	s.DeleteCovered(cov)
	tree := testfs.Tree(t, dir)
	assert.Contains(t, tree, "telemetry/"+other)
	assert.Contains(t, tree, "telemetry/events/"+hourName(h)+"/dropped-0123456789abcdef")
	assert.NotContains(t, tree, "telemetry/"+slotPath(h, 1))
}

// TestUploadAgeLimits asserts an hour at most MaxUploadAge old is listed and
// an older one is skipped: none of its events is listed and none of its files
// can be covered or deleted.
func TestUploadAgeLimits(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	dir := t.TempDir()
	day := 24 * time.Hour
	listed := map[string]Event{}
	skipped := map[string]Event{}
	for name, age := range map[string]time.Duration{
		"4 days": 4 * day, "29 days": 29 * day, "30 days": MaxUploadAge,
	} {
		ev := runEvent(now.Add(-age))
		putSlot(t, dir, 1, ev)
		listed[name] = ev
	}
	for name, age := range map[string]time.Duration{
		"30 days and 1 hour": MaxUploadAge + time.Hour, "31 days": 31 * day,
	} {
		ev := runEvent(now.Add(-age))
		putSlot(t, dir, 1, ev)
		skipped[name] = ev
	}
	s := openStore(t, dir)

	list := mustReady(t, s, ReadyOptions{})
	got := eventUIDs(list.Events)
	for name, ev := range listed {
		assert.Contains(t, got, ev.UID(), name)
	}
	for name, ev := range skipped {
		assert.NotContains(t, got, ev.UID(), name)
	}
	assert.Len(t, got, len(listed))

	all, err := s.List()
	require.NoError(t, err)
	var ids []string
	for _, ev := range all {
		ids = append(ids, ev.UID)
	}
	before := testfs.Tree(t, dir)
	cov := list.Cover(idSet(ids...))
	assert.Len(t, cov.files, len(listed), "only the files of the listed hours are covered")
	s.DeleteCovered(cov)
	after := testfs.Tree(t, dir)
	for name, ev := range skipped {
		p := "telemetry/" + slotPath(ev.Hour(), 1)
		assert.Contains(t, before, p, name)
		assert.Contains(t, after, p, "%s: the file stays", name)
	}

	t.Run("only old hours", func(t *testing.T) {
		dir := t.TempDir()
		for _, age := range []time.Duration{MaxUploadAge + time.Hour, 40 * day} {
			putSlot(t, dir, 1, runEvent(now.Add(-age)))
		}
		s := openStore(t, dir)
		before := testfs.Snapshot(t, dir)
		list := mustReady(t, s, ReadyOptions{})
		assert.Empty(t, list.Events)
		s.DeleteCovered(list.Cover(idSet("a", "b")))
		assert.Equal(t, before, testfs.Snapshot(t, dir))
	})
}

// TestYoungCompactedFileWaits asserts a compacted file written less than a
// minute ago is not listed and not covered unless the pass of this run named
// it, and that Record names the file its pass finished writing.
func TestYoungCompactedFileWaits(t *testing.T) {
	now := uploadNow()
	useClock(t, now)
	h := hoursAgo(now, 3)

	t.Run("the age boundary", func(t *testing.T) {
		dir := t.TempDir()
		ev := runEvent(h)
		file := putCompacted(t, dir, h, hourFile(h), now.Add(-59*time.Second), ev)
		putSlot(t, dir, 1, ev)
		s := openStore(t, dir)
		list := mustReady(t, s, ReadyOptions{})
		assert.Equal(t, []string{ev.UID()}, eventUIDs(list.Events), "the event is listed from its slot")
		assert.ElementsMatch(t, []string{slotPath(h, 1)}, coveredPaths(list.Cover(idSet(ev.UID()))), "the young file is not covered")

		require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", filepath.FromSlash(file)), now.Add(-time.Minute), now.Add(-time.Minute)))
		list = mustReady(t, s, ReadyOptions{})
		assert.Equal(t, []string{ev.UID()}, eventUIDs(list.Events))
		assert.ElementsMatch(t, []string{file, slotPath(h, 1)}, coveredPaths(list.Cover(idSet(ev.UID()))), "at a minute the file is covered")
	})

	t.Run("a file the pass wrote", func(t *testing.T) {
		enable(t)
		dir := t.TempDir()
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(now)))
		a, b := runEvent(h), runEvent(h)
		putSlot(t, dir, 1, a)
		putSlot(t, dir, 2, b)
		marker := filepath.Join(dir, "telemetry", fileMarker)
		require.NoError(t, os.Chtimes(marker, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))

		_, _, written := Record(runEvent(now), Options{ConfigDir: dir, DirExisted: true})
		want := path.Join("events", hourName(h), hourFile(h))
		require.Equal(t, []string{want}, written)
		file := filepath.Join(dir, "telemetry", filepath.FromSlash(want))
		require.NoError(t, os.Chtimes(file, now, now))
		s := openStore(t, dir)

		assert.Empty(t, mustReady(t, s, ReadyOptions{}).Events, "without the name the young file waits")
		list := mustReady(t, s, ReadyOptions{Written: written})
		assert.ElementsMatch(t, uidsOfEvents(a, b), eventUIDs(list.Events))
		assert.ElementsMatch(t, []string{want}, coveredPaths(list.Cover(idSet(a.UID(), b.UID()))))

		same := mustReady(t, s, ReadyOptions{Written: []string{"events/" + hourName(h) + "/missing.jsonl", fileMarker, "../x", ""}})
		assert.Empty(t, same.Events, "a name the listing does not find has no effect")
	})

	t.Run("a pass that wrote nothing", func(t *testing.T) {
		enable(t)
		dir := t.TempDir()
		_, _, written := Record(runEvent(now), Options{ConfigDir: dir, DirExisted: true})
		assert.Nil(t, written)
	})

	t.Run("a failed write", func(t *testing.T) {
		enable(t)
		dir := t.TempDir()
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(now)))
		putSlot(t, dir, 1, runEvent(h))
		require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", fileMarker), now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
		useSeam(t, &writeHook, func(name string, _ int) error {
			if strings.HasSuffix(name, jsonlSuffix) {
				return os.ErrClosed
			}
			return nil
		})
		_, _, written := Record(runEvent(now), Options{ConfigDir: dir, DirExisted: true})
		assert.Nil(t, written)
		assert.Empty(t, jsonlNames(t, dir, h))
	})

	t.Run("a panic below the pass", func(t *testing.T) {
		enable(t)
		dir := t.TempDir()
		require.Equal(t, StatusRecorded, record(t, dir, runEvent(now)))
		putSlot(t, dir, 1, runEvent(h))
		require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", fileMarker), now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
		useSeam(t, &pauseHook, func(stage string) {
			if stage == "file-closed" {
				panic("stop")
			}
		})
		st, _, written := Record(runEvent(now), Options{ConfigDir: dir, DirExisted: true})
		assert.Equal(t, StatusFailed, st)
		assert.Nil(t, written)
		assert.Len(t, jsonlNames(t, dir, h), 1, "the file was written before the panic")
	})
}

// TestSenderAndPassInterleave asserts a deletion of the slots of a finished
// hour and a pass that compacts the hour, in either order and with the pass
// stopped after its file is written, return no error, count a slot the pass
// already removed as deleted, and leave every event that was not covered.
func TestSenderAndPassInterleave(t *testing.T) {
	now := uploadNow()
	h := hoursAgo(now, 3)
	setup := func(t *testing.T) (dir string, s *Store, evs []Event) {
		t.Helper()
		useClock(t, now)
		dir = t.TempDir()
		for i := range 3 {
			ev := runEvent(h)
			putSlot(t, dir, i+1, ev)
			evs = append(evs, ev)
		}
		return dir, openStore(t, dir), evs
	}
	remaining := func(t *testing.T, s *Store) []string {
		t.Helper()
		return uidsOf(t, s)
	}

	t.Run("delete then pass", func(t *testing.T) {
		_, s, evs := setup(t)
		list := mustReady(t, s, ReadyOptions{})
		res := s.DeleteCovered(list.Cover(idSet(evs[0].UID())))
		assert.Equal(t, []string{slotPath(h, 1)}, res.Deleted)
		assert.NotPanics(t, func() { s.pass(now, newProducer(), "") })
		assert.ElementsMatch(t, uidsOfEvents(evs[1], evs[2]), remaining(t, s))
	})

	t.Run("pass then delete", func(t *testing.T) {
		_, s, evs := setup(t)
		list := mustReady(t, s, ReadyOptions{})
		s.pass(now, newProducer(), "")
		res := s.DeleteCovered(list.Cover(idSet(evs[0].UID())))
		assert.Equal(t, []string{slotPath(h, 1)}, res.Deleted, "a slot the pass removed counts as deleted")
		assert.Empty(t, res.Waiting)
		assert.Empty(t, res.Refused)
		assert.Subset(t, remaining(t, s), uidsOfEvents(evs[1], evs[2]))
	})

	t.Run("pass stopped after its file", func(t *testing.T) {
		dir, s, evs := setup(t)
		useSeam(t, &removeHook, func(string) bool { return false })
		s.pass(now, newProducer(), "")
		assert.Len(t, jsonlNames(t, dir, h), 1)
		assert.Len(t, slotNames(t, dir, h), 3, "the slots and the file are both present")
		list := mustReady(t, s, ReadyOptions{})
		res := s.DeleteCovered(list.Cover(idSet(evs[0].UID())))
		assert.Equal(t, []string{slotPath(h, 1)}, res.Deleted)
		assert.Subset(t, remaining(t, s), uidsOfEvents(evs[1], evs[2]))
	})

	t.Run("delete while the pass is listing", func(t *testing.T) {
		_, s, evs := setup(t)
		list := mustReady(t, s, ReadyOptions{})
		cov := list.Cover(idSet(evs[0].UID(), evs[1].UID()))
		reached, release := make(chan struct{}), make(chan struct{})
		useSeam(t, &pauseHook, func(stage string) {
			if stage == "listed" {
				close(reached)
				<-release
			}
		})
		other := openStore(t, s.configDir)
		var wg sync.WaitGroup
		wg.Go(func() { other.pass(now, newProducer(), "") })
		<-reached
		res := s.DeleteCovered(cov)
		close(release)
		wg.Wait()
		assert.Len(t, res.Deleted, 2)
		assert.Empty(t, res.Waiting)
		assert.Subset(t, remaining(t, s), uidsOfEvents(evs[2]))
	})
}

// TestReadyListTakesFinishedHours asserts the current hour is listed only on
// request and an hour after the current one never.
func TestReadyListTakesFinishedHours(t *testing.T) {
	now := uploadNow().Add(20 * time.Minute)
	useClock(t, now)
	dir := t.TempDir()
	past, current, next := runEvent(hoursAgo(now, 1)), runEvent(now), runEvent(now.Add(time.Hour))
	putSlot(t, dir, 1, past)
	putSlot(t, dir, 1, current)
	putSlot(t, dir, 1, next)
	s := openStore(t, dir)

	assert.Equal(t, uidsOfEvents(past), eventUIDs(mustReady(t, s, ReadyOptions{}).Events))
	assert.Equal(t, uidsOfEvents(past, current), eventUIDs(mustReady(t, s, ReadyOptions{CurrentHour: true}).Events))
}

// TestReadyListFollowsTheWalk asserts that with no young file and no old hour
// the ready list with the current hour holds the events of List, in order.
func TestReadyListFollowsTheWalk(t *testing.T) {
	now := uploadNow().Add(30 * time.Minute)
	useClock(t, now)
	dir := t.TempDir()
	old := now.Add(-time.Hour)
	var evs []Event
	for i := range 4 {
		evs = append(evs, runEvent(hoursAgo(now, 5-i)))
	}
	putSlot(t, dir, 1, evs[0])
	putCompacted(t, dir, hoursAgo(now, 4), hourFile(hoursAgo(now, 4)), old, evs[1])
	putSlot(t, dir, 7, evs[1])
	putSlot(t, dir, 8, runEvent(hoursAgo(now, 4)))
	putCompacted(t, dir, hoursAgo(now, 3), hourFile(hoursAgo(now, 3)), old, evs[2])
	putSlot(t, dir, 2, evs[3])
	putSlot(t, dir, 3, runEvent(now))
	s := openStore(t, dir)

	want, err := s.List()
	require.NoError(t, err)
	require.Len(t, want, 6)
	assert.Equal(t, want, mustReady(t, s, ReadyOptions{CurrentHour: true}).Events)
}

// TestARecordedRunWithAPassTouchesNoUploadFile asserts a recorded run whose
// hourly pass compacts a finished hour makes exactly the file operations the
// store made before the upload files existed, and none names upload.
func TestARecordedRunWithAPassTouchesNoUploadFile(t *testing.T) {
	enable(t)
	useDistinctSlots(t)
	now := fixedNoon
	c := useClock(t, now)
	dir := t.TempDir()
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(now)))
	past := now.Add(-3 * time.Hour)
	putSlot(t, dir, 1, runEvent(past))
	putSlot(t, dir, 2, runEvent(past))
	require.NoError(t, os.Chtimes(filepath.Join(dir, "telemetry", fileMarker), now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
	ops := observeOps(t)

	require.Equal(t, StatusRecorded, record(t, dir, runEvent(c.now())))
	var got []string
	for _, op := range ops.take() {
		got = append(got, strings.ReplaceAll(op, hourFile(past), "<jsonl>"))
	}
	want := []string{
		"openroot .", "lstat telemetry", "openroot telemetry", "readfile config-directory-id", "stat pruned",
		"openfile events/<hour>/<slot>", "write events/<hour>/<slot>", "close events/<hour>/<slot>",
		"openfile pruned", "close pruned", "chtimes pruned",
		"readdir events", "readdir events/<hour>",
		"readfile events/<hour>/<slot>", "readfile events/<hour>/<slot>",
		"openfile events/<hour>/<jsonl>", "write events/<hour>/<jsonl>", "sync events/<hour>/<jsonl>", "close events/<hour>/<jsonl>",
		"remove events/<hour>/<slot>", "remove events/<hour>/<slot>",
		"readdir servers",
	}
	assert.Equal(t, want, shape(got))
	for _, op := range got {
		assert.NotContains(t, op, dirUpload)
	}
}

// TestResetAlsoRemovesTheUploadDirectory asserts Reset removes
// telemetry/upload with the other recorded data and keeps the prune marker.
func TestResetAlsoRemovesTheUploadDirectory(t *testing.T) {
	dir := telemetryDir(t)
	upload := filepath.Join(dir, "telemetry", dirUpload)
	require.NoError(t, os.Mkdir(upload, 0o700))
	for _, name := range []string{fileLock, fileState, ".lock-x", stateTempName} {
		require.NoError(t, os.WriteFile(filepath.Join(upload, name), []byte("x"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry", fileMarker), nil, 0o600))
	s := openStore(t, dir)
	require.NoError(t, s.Reset())
	assert.Equal(t, []string{"telemetry", "telemetry/" + fileMarker}, testfs.Tree(t, dir))
}
