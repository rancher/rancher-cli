package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// failingRandom makes the random source fail until the test ends.
func failingRandom(t *testing.T) {
	t.Helper()
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	t.Cleanup(func() { randRead = old })
}

// TestARandomSourceFailureDropsTheEvent asserts a failed random source gives
// an event Record drops: no id is invented, no file is created, the process
// instance id is not remembered, and the error stays inside the package.
func TestARandomSourceFailureDropsTheEvent(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()

	instanceMu.Lock()
	saved := instanceVal
	instanceVal = ""
	instanceMu.Unlock()
	t.Cleanup(func() {
		instanceMu.Lock()
		instanceVal = saved
		instanceMu.Unlock()
	})

	old := randRead
	failingRandom(t)
	ev := runEvent(fixedNoon)
	assert.True(t, ev.failed(), "the constructor marks the event")
	assert.Empty(t, ev.UID(), "no id is invented")
	assert.Equal(t, StatusFailed, record(t, dir, ev))
	assert.Empty(t, readNames(t, dir), "nothing is created")
	assert.True(t, NewEventsDropped(fixedNoon, 1, testContext()).failed())
	instanceMu.Lock()
	assert.Empty(t, instanceVal, "a failure is not remembered")
	instanceMu.Unlock()

	randRead = old
	ok := runEvent(fixedNoon)
	assert.False(t, ok.failed())
	assert.Regexp(t, randomIDPattern, ok.UID())
	assert.Equal(t, StatusRecorded, record(t, dir, ok))
}

// TestARandomSourceFailureWhileCreatingTheDirectoryID asserts no id file is
// created when the random source fails at that point, and the event is
// recorded without a directory id.
func TestARandomSourceFailureWhileCreatingTheDirectoryID(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	ev := runEvent(fixedNoon)
	failingRandom(t)
	assert.Equal(t, StatusRecorded, record(t, dir, ev))
	_, err := os.Stat(filepath.Join(dir, "telemetry", fileID))
	require.ErrorIs(t, err, os.ErrNotExist)
	list, err := openStore(t, dir).List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotContains(t, list[0].Fields, attrConfigDirID)
}

// TestOnlyAVersion4IDIsRead asserts the instance id given to a constructor
// and the id read back from the id file must be version 4: a version 1 id is
// not used, an old one in the file is replaced, a young one leaves the id out
// of the event, and every stored event passes the schema.
func TestOnlyAVersion4IDIsRead(t *testing.T) {
	enable(t)
	c := useClock(t, fixedNoon)
	const v1 = "12345678-1234-1234-1234-123456789abc"

	in := baseInput()
	in.InstanceID = v1
	assert.NotEqual(t, v1, decodeEvent(t, NewCommandRun(in).Flat())[attrInstanceID], "a version 1 instance id input is refused")
	assert.NotEqual(t, v1, decodeEvent(t, NewCommandRun(in).withDirectoryID(v1).Flat())[attrConfigDirID])
	assert.NotContains(t, decodeEvent(t, NewCommandRun(in).withDirectoryID(v1).Flat()), attrConfigDirID)

	dir := t.TempDir()
	idPath := filepath.Join(dir, "telemetry", fileID)
	require.NoError(t, os.MkdirAll(filepath.Dir(idPath), 0o700))
	require.NoError(t, os.WriteFile(idPath, []byte(v1), 0o600))
	require.NoError(t, os.Chtimes(idPath, c.now().Add(-10*time.Second), c.now().Add(-10*time.Second)))
	young := runEvent(fixedNoon)
	require.Equal(t, StatusRecorded, record(t, dir, young))
	got, err := os.ReadFile(idPath)
	require.NoError(t, err)
	assert.Equal(t, v1, string(got), "a young file is left alone")

	require.NoError(t, os.Chtimes(idPath, c.now().Add(-5*time.Minute), c.now().Add(-5*time.Minute)))
	replaced := runEvent(fixedNoon)
	require.Equal(t, StatusRecorded, record(t, dir, replaced))
	got, err = os.ReadFile(idPath)
	require.NoError(t, err)
	assert.Regexp(t, randomIDPattern, string(got), "an old one is replaced")

	list, err := openStore(t, dir).List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	v := schemaValidator(t)
	for _, ev := range list {
		assert.Empty(t, v.errors(ev.Fields), "a stored event passes the published schema")
	}
	for _, ev := range list {
		switch ev.UID {
		case young.UID():
			assert.NotContains(t, ev.Fields, attrConfigDirID, "the event written while the id was in doubt has none")
		case replaced.UID():
			assert.Equal(t, string(got), ev.Fields[attrConfigDirID])
		default:
			t.Errorf("unexpected event %s", ev.UID)
		}
	}
}

// TestTwoProcessesCreatingTheIDOnANewDirectoryEndWithOne asserts the id file
// is created exclusively: a process that paused before its create finds the
// winner's id and uses it; every event carries the id the directory keeps.
func TestTwoProcessesCreatingTheIDOnANewDirectoryEndWithOne(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "telemetry"), 0o700))

	reached := make(chan struct{})
	release := make(chan struct{})
	var first atomic.Bool
	old := pauseHook
	pauseHook = func(stage string) {
		if stage == "create-id" && first.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { pauseHook = old })

	var wg sync.WaitGroup
	wg.Go(func() { record(t, dir, runEvent(fixedNoon)) })
	<-reached
	require.Equal(t, StatusRecorded, record(t, dir, runEvent(fixedNoon)))
	close(release)
	wg.Wait()

	onDisk, err := os.ReadFile(filepath.Join(dir, "telemetry", fileID))
	require.NoError(t, err)
	list, err := openStore(t, dir).List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, ev := range list {
		assert.Equal(t, string(onDisk), ev.Fields[attrConfigDirID])
	}
}

// pausedCompactors runs two compactors on one hour: the first pauses at the
// named stage; the second runs to the end; then the first resumes. It returns
// both results.
func pausedCompactors(t *testing.T, dir string, h time.Time, now time.Time, stage string) (first, second hourResult) {
	t.Helper()
	reached := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	old := pauseHook
	pauseHook = func(s string) {
		if s == stage && once.CompareAndSwap(false, true) {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { pauseHook = old })
	hd := hourDir{name: hourName(h), t: h}
	sFirst, sSecond := openStore(t, dir), openStore(t, dir)
	var wg sync.WaitGroup
	wg.Go(func() { first = sFirst.compactHour(hd, now, newProducer(), "") })
	<-reached
	second = sSecond.compactHour(hd, now, newProducer(), "")
	close(release)
	wg.Wait()
	return first, second
}

func countNames(t *testing.T, s *Store, name string) (n int) {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	for _, ev := range list {
		if ev.Name == name {
			n++
		}
	}
	return n
}

func hourLines(t *testing.T, dir string, h time.Time, file string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(hourPath(dir, h), file))
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// TestASecondCompactorThatListedEarlierCannotOverwriteTheHourFile asserts
// the hour file is created exclusively: a compactor that listed the hour
// before another finished, and whose stale listing still names slots and
// markers, finds the name taken and stops; the file keeps the first
// compactor's events and its one dropped event.
func TestASecondCompactorThatListedEarlierCannotOverwriteTheHourFile(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	for i := range 5 {
		putSlot(t, dir, i, runEvent(h))
	}
	putMarker(t, dir, h, "aaaaaaaa")
	putMarker(t, dir, h, "bbbbbbbb")

	// The first process to reach "listed" waits; the second runs to the
	// end. So the waiting one, the one that resumes, is the one with the
	// stale listing: here "first" resumes last and must fail to create.
	first, second := pausedCompactors(t, dir, h, c.now(), "listed")
	assert.Equal(t, hourDone, second)
	assert.Equal(t, hourStopped, first, "the late compactor's create found the name taken")

	file := hourName(h) + ".jsonl"
	assert.Equal(t, []string{file}, jsonlNames(t, dir, h))
	lines := hourLines(t, dir, h, file)
	assert.Len(t, lines, 6, "five events and one dropped event")
	s := openStore(t, dir)
	assert.Equal(t, 1, countNames(t, s, EventEventsDropped))
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// TestASecondCompactorOfALateFileStopsOnAYoungFile asserts a compactor that
// finds a late file of the hour less than a minute old, because another
// process created it and is still writing, stops: no second late file, no
// second dropped event, no event written twice.
func TestASecondCompactorOfALateFileStopsOnAYoungFile(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(4 * time.Hour)
	early := runEvent(h)
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(mainFile), 0o700))
	require.NoError(t, os.WriteFile(mainFile, append(early.stored(), '\n'), 0o600))
	old := c.now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mainFile, old, old))
	late := runEvent(h)
	putSlot(t, dir, 7, late)
	putMarker(t, dir, h, "cccccccc")

	first, second := pausedCompactors(t, dir, h, c.now(), "file-created")
	assert.Equal(t, hourDone, first)
	assert.Equal(t, hourStopped, second, "the young late file stopped the second compactor")

	assert.Equal(t, []string{hourName(h) + "-1.jsonl", hourName(h) + ".jsonl"}, jsonlNames(t, dir, h))
	lines := hourLines(t, dir, h, hourName(h)+"-1.jsonl")
	assert.Len(t, lines, 2, "the late event and one dropped event")
	s := openStore(t, dir)
	assert.Equal(t, 1, countNames(t, s, EventEventsDropped))
	assert.ElementsMatch(t, []string{early.UID(), late.UID()}, withoutDropped(t, s))
}

func withoutDropped(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.List()
	require.NoError(t, err)
	var out []string
	for _, ev := range list {
		if ev.Name != EventEventsDropped {
			out = append(out, ev.UID)
		}
	}
	return out
}

// TestALateDropMarkerIsCounted asserts a marker that appears after the hour
// was compacted is counted in a dropped event of the late file, and removed
// only by that pass.
func TestALateDropMarkerIsCounted(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(4 * time.Hour)
	first := runEvent(h)
	putSlot(t, dir, 0, first)
	putMarker(t, dir, h, "aaaaaaaa")
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	old := c.now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mainFile, old, old))

	putMarker(t, dir, h, "bbbbbbbb")
	putMarker(t, dir, h, "cccccccc")
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{hourName(h) + "-1.jsonl", hourName(h) + ".jsonl"}, jsonlNames(t, dir, h))
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 3, n, "one counted with the hour file and two in the late file")
	assert.Equal(t, 2, countNames(t, s, EventEventsDropped), "one dropped event per written file")
	for _, name := range readNames(t, hourPath(dir, h)) {
		assert.False(t, strings.HasPrefix(name, droppedPrefix), "the counted markers are gone")
	}
}

// TestASlotThatCannotBeReadIsKept asserts the pass leaves a slot whose read
// fails, as a Windows sharing violation does, and still compacts the others;
// the slot is compacted on a later pass.
func TestASlotThatCannotBeReadIsKept(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(5 * time.Hour)
	var evs []CommandRun
	for i := range 4 {
		evs = append(evs, runEvent(h))
		putSlot(t, dir, i, evs[i])
	}
	old := readHook
	readHook = func(name string) error {
		if strings.HasSuffix(name, "0001.json") {
			return errors.New("sharing violation")
		}
		return nil
	}
	t.Cleanup(func() { readHook = old })
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{"0001.json"}, slotNames(t, dir, h), "the unreadable slot stays")
	assert.Len(t, hourLines(t, dir, h, hourName(h)+".jsonl"), 3)

	readHook = old
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	past := c.now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mainFile, past, past))
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Len(t, hourLines(t, dir, h, hourName(h)+"-1.jsonl"), 1)
}

// TestAFailedSyncKeepsEverySlot asserts a sync failure of the compacted file
// keeps every slot and leaves no file, as a write failure does.
func TestAFailedSyncKeepsEverySlot(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	for i := range 4 {
		putSlot(t, dir, i, runEvent(h))
	}
	old := syncHook
	syncHook = func(string) error { return errors.New("sync failed") }
	t.Cleanup(func() { syncHook = old })
	openStore(t, dir).pass(c.now(), newProducer(), "")
	assert.Len(t, slotNames(t, dir, h), 4)
	assert.Empty(t, jsonlNames(t, dir, h))
}

// TestALateSlotWhoseEventIsInTheHourFileIsRemoved asserts a slot whose
// record id is already in a file of the hour is removed as a duplicate, and
// the new late file holds exactly the events that were not in the hour file.
func TestALateSlotWhoseEventIsInTheHourFileIsRemoved(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(5 * time.Hour)
	inFile, fresh := runEvent(h), runEvent(h)
	mainFile := filepath.Join(hourPath(dir, h), hourName(h)+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(mainFile), 0o700))
	require.NoError(t, os.WriteFile(mainFile, append(inFile.stored(), '\n'), 0o600))
	old := c.now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(mainFile, old, old))
	putSlot(t, dir, 1, inFile)
	putSlot(t, dir, 2, fresh)

	openStore(t, dir).pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h), "the duplicate slot is removed")
	lines := hourLines(t, dir, h, hourName(h)+"-1.jsonl")
	require.Len(t, lines, 1, "the late file holds only the event that was not in the hour file")
	assert.Contains(t, lines[0], fresh.UID())
	assert.NotContains(t, lines[0], inFile.UID())
}

func writeTemp(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, "telemetry", "servers", name)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte("{"), 0o600))
	at := time.Now().Add(-age)
	require.NoError(t, os.Chtimes(p, at, at))
}

// TestFactsTemporaryFilesAreRemovedWhenOld asserts the pass removes facts
// temporary files an hour old or more, at most eight per pass and apart
// from the cap on facts files, and leaves a younger one, which a writer may
// still be using.
func TestFactsTemporaryFilesAreRemovedWhenOld(t *testing.T) {
	enable(t)
	now := time.Now().UTC()
	c := useClock(t, now)
	dir := t.TempDir()
	s := openStore(t, dir)
	for i := range 12 {
		writeTemp(t, dir, fmt.Sprintf(".%032x.json-%08x", i, i), 2*time.Hour)
	}
	writeTemp(t, dir, ".young.json-aaaaaaaa", 10*time.Minute)
	for i := range 10 {
		var f Facts
		f.SetVersion("2.15.2", EditionPrime, now.Add(-40*24*time.Hour))
		require.NoError(t, s.WriteFacts(fmt.Sprintf("https://old%d.example", i), f))
	}
	s.pass(c.now(), newProducer(), "")
	names := readNames(t, filepath.Join(dir, "telemetry", "servers"))
	var temps, facts int
	for _, n := range names {
		if strings.HasPrefix(n, ".") {
			temps++
		} else {
			facts++
		}
	}
	assert.Equal(t, 5, temps, "eight of twelve old temporary files removed; the young one stays")
	assert.Equal(t, 2, facts, "eight of ten old facts files removed: separate caps")
	assert.Contains(t, names, ".young.json-aaaaaaaa")
	s.pass(c.now(), newProducer(), "")
	names = readNames(t, filepath.Join(dir, "telemetry", "servers"))
	assert.Equal(t, []string{".young.json-aaaaaaaa"}, names)
}

func bigEvent(t *testing.T, at time.Time, size int) (CommandRun, []byte) {
	t.Helper()
	ev := runEvent(at)
	m := decodeEvent(t, ev.stored())
	m["io.rancher.future.note"] = strings.Repeat("x", size)
	data, err := json.Marshal(m)
	require.NoError(t, err)
	return ev, data
}

// TestAnOversizedSlotIsNotAnEventForReaders asserts List and Count skip a
// slot of 20 KiB.
func TestAnOversizedSlotIsNotAnEventForReaders(t *testing.T) {
	enable(t)
	useClock(t, fixedNoon)
	dir := t.TempDir()
	ev, data := bigEvent(t, fixedNoon, 20*1024)
	require.Greater(t, len(data), 20*1024)
	p := hourPath(dir, fixedNoon)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0001.json"), data, 0o600))
	putSlot(t, dir, 2, runEvent(fixedNoon))
	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.NotEqual(t, ev.UID(), list[0].UID)
	n, err := s.Count(CountLimit)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestAnOversizedSlotIsKeptThenRemovedByThePass asserts the pass does not
// compact a 20 KiB slot, keeps it until its settling time, and then removes
// it.
func TestAnOversizedSlotIsKeptThenRemovedByThePass(t *testing.T) {
	enable(t)
	h := realHour(10 * time.Hour)
	c := useClock(t, h.Add(90*time.Minute))
	dir := t.TempDir()
	_, data := bigEvent(t, h, 20*1024)
	p := hourPath(dir, h)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0001.json"), data, 0o600))
	good := runEvent(h)
	putSlot(t, dir, 2, good)
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Equal(t, []string{"0001.json"}, slotNames(t, dir, h), "kept before its settling time")
	assert.Equal(t, []string{good.UID()}, uidsOf(t, s))
	assert.NotContains(t, strings.Join(hourLines(t, dir, h, hourName(h)+".jsonl"), "\n"), "future.note")

	c.set(h.Add(2 * time.Hour))
	mainFile := filepath.Join(p, hourName(h)+".jsonl")
	require.NoError(t, os.Chtimes(mainFile, c.now().Add(-time.Hour), c.now().Add(-time.Hour)))
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h), "removed once settled")
}

// TestAnEventFromANewerCLIIsKept asserts the structural minimum is all a
// reader and the pass check: an event with a key and values this version does
// not know, and a newer local_format, is listed with its keys and copied
// into the hour file unchanged.
func TestAnEventFromANewerCLIIsKept(t *testing.T) {
	enable(t)
	h := realHour(5 * time.Hour)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	ev := runEvent(h)
	m := decodeEvent(t, ev.stored())
	m["io.rancher.future.thing"] = "value"
	m[attrClientType] = "robot"
	m[attrErrorType] = "A_NEW_CODE"
	m[keyLocalFormat] = float64(2)
	data, err := json.Marshal(m)
	require.NoError(t, err)
	p := hourPath(dir, h)
	require.NoError(t, os.MkdirAll(p, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(p, "0003.json"), data, 0o600))

	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "value", list[0].Fields["io.rancher.future.thing"])
	assert.Equal(t, "robot", list[0].Fields[attrClientType])

	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	lines := hourLines(t, dir, h, hourName(h)+".jsonl")
	require.Len(t, lines, 1)
	got := decodeEvent(t, []byte(lines[0]))
	assert.Equal(t, "value", got["io.rancher.future.thing"])
	assert.Equal(t, "A_NEW_CODE", got[attrErrorType])
	list, err = s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "value", list[0].Fields["io.rancher.future.thing"], "still shown after compaction")
}

// TestTheStructuralMinimum asserts what a stored event must have: each
// missing or mistyped part fails, an unknown key does not.
func TestTheStructuralMinimum(t *testing.T) {
	good := func() map[string]any { return decodeEvent(t, runEvent(fixedNoon).stored()) }
	cases := map[string]func(m map[string]any){
		"no name":           func(m map[string]any) { delete(m, keyEventName) },
		"name not string":   func(m map[string]any) { m[keyEventName] = 3 },
		"no time":           func(m map[string]any) { delete(m, keyTime) },
		"no schema":         func(m map[string]any) { delete(m, keySchemaVersion) },
		"no uid":            func(m map[string]any) { delete(m, attrRecordUID) },
		"uid not version 4": func(m map[string]any) { m[attrRecordUID] = "12345678-1234-1234-1234-123456789abc" },
	}
	for name, mutate := range cases {
		m := good()
		mutate(m)
		data, err := json.Marshal(m)
		require.NoError(t, err)
		_, ok := parseEvent(data)
		assert.False(t, ok, name)
	}
	_, ok := parseEvent([]byte(`[1]`))
	assert.False(t, ok)
	m := good()
	m["anything"] = []any{1, "two"}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	_, ok = parseEvent(data)
	assert.True(t, ok, "an unknown key is kept")
}

// stepClock returns a clock that moves forward by step on every call.
func stepClock(t *testing.T, start time.Time, step time.Duration) {
	t.Helper()
	var mu sync.Mutex
	cur := start
	old := nowFunc
	nowFunc = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(step)
		return cur
	}
	t.Cleanup(func() { nowFunc = old })
}

// setOld moves the modification time of every compacted file of an hour
// back past the young-file minute, as an hour of waiting between two passes
// would.
func setOld(t *testing.T, dir string, h time.Time, now time.Time) {
	t.Helper()
	for _, n := range jsonlNames(t, dir, h) {
		past := now.Add(-time.Hour)
		require.NoError(t, os.Chtimes(filepath.Join(hourPath(dir, h), n), past, past))
	}
}

// TestAPartialPassWritesWhatItReadAndLeavesTheRest asserts that when the read
// phase runs out of time halfway, the pass writes the slots it read and
// removes exactly those, carries no dropped event and removes no marker, and
// leaves the unread slots untouched; repeated passes end with every event in
// a file once, no slot left, and the markers counted once by the pass that
// read the whole hour.
func TestAPartialPassWritesWhatItReadAndLeavesTheRest(t *testing.T) {
	enable(t)
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	var want []string
	for i := range 10 {
		ev := runEvent(h)
		want = append(want, ev.UID())
		putSlot(t, dir, i, ev)
	}
	for _, m := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		putMarker(t, dir, h, m)
	}
	// The clock moves 40 ms per call; the limit is 150 ms; the pass checks
	// after each slot read; the deadline call and four checks pass it;
	// it reads four slots.
	stepClock(t, time.Now().UTC(), 40*time.Millisecond)
	s := openStore(t, dir)
	s.pass(nowFunc(), newProducer(), "")

	assert.Equal(t, []string{"0004.json", "0005.json", "0006.json", "0007.json", "0008.json", "0009.json"}, slotNames(t, dir, h), "the unread slots stay")
	assert.Len(t, hourLines(t, dir, h, hourName(h)+".jsonl"), 4, "the four slots read were written")
	assert.Len(t, readMarkers(t, dir, h), 3, "a partial write removes no marker")
	assert.Zero(t, countNames(t, s, EventEventsDropped), "and carries no dropped event")

	for range 6 {
		setOld(t, dir, h, nowFunc())
		s.pass(nowFunc(), newProducer(), "")
	}
	assert.Empty(t, slotNames(t, dir, h))
	assert.Empty(t, readMarkers(t, dir, h))
	assert.ElementsMatch(t, want, withoutDropped(t, s), "every event once")
	assert.Equal(t, 1, countNames(t, s, EventEventsDropped), "the markers are counted once")
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func readMarkers(t *testing.T, dir string, h time.Time) []string {
	t.Helper()
	var out []string
	for _, n := range readNames(t, hourPath(dir, h)) {
		if strings.HasPrefix(n, droppedPrefix) {
			out = append(out, n)
		}
	}
	return out
}

// TestEveryPassReadsAtLeastOneSlot asserts a clock already past the limit at
// the start of the read phase still leaves each pass one slot of progress;
// so repeated passes finish the hour.
func TestEveryPassReadsAtLeastOneSlot(t *testing.T) {
	enable(t)
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	var want []string
	for i := range 6 {
		ev := runEvent(h)
		want = append(want, ev.UID())
		putSlot(t, dir, i, ev)
	}
	stepClock(t, time.Now().UTC(), time.Second)
	s := openStore(t, dir)
	s.pass(nowFunc(), newProducer(), "")
	assert.Len(t, hourLines(t, dir, h, hourName(h)+".jsonl"), 1, "one slot of progress")
	assert.Len(t, slotNames(t, dir, h), 5)
	for range 8 {
		setOld(t, dir, h, nowFunc())
		s.pass(nowFunc(), newProducer(), "")
	}
	assert.Empty(t, slotNames(t, dir, h))
	assert.ElementsMatch(t, want, withoutDropped(t, s))
}

// TestTheWritePhaseIgnoresTheClock asserts that once the compacted file is
// closed, the removals run to the end even when the clock is past the limit.
func TestTheWritePhaseIgnoresTheClock(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	for i := range 6 {
		putSlot(t, dir, i, runEvent(h))
	}
	putMarker(t, dir, h, "aaaaaaaa")
	old := pauseHook
	pauseHook = func(stage string) {
		if stage == "file-closed" {
			c.set(c.now().Add(time.Second))
		}
	}
	t.Cleanup(func() { pauseHook = old })
	openStore(t, dir).pass(c.now().Add(-time.Second), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Empty(t, readMarkers(t, dir, h))
	assert.Len(t, hourLines(t, dir, h, hourName(h)+".jsonl"), 7)
}

// TestMarkersAreRemovedBeforeSlots asserts a pass that stops after as many
// removals as there are markers has removed the markers and kept the slots;
// so the next pass removes the slots as duplicates and the drops are counted
// once.
func TestMarkersAreRemovedBeforeSlots(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	for i := range 5 {
		putSlot(t, dir, i, runEvent(h))
	}
	for _, m := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"} {
		putMarker(t, dir, h, m)
	}
	removals := 0
	old := removeHook
	removeHook = func(string) bool {
		removals++
		return removals <= 3
	}
	t.Cleanup(func() { removeHook = old })
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, readMarkers(t, dir, h), "the markers went first")
	assert.Len(t, slotNames(t, dir, h), 5, "the slots stayed")

	removeHook = old
	setOld(t, dir, h, c.now())
	s.pass(c.now(), newProducer(), "")
	assert.Empty(t, slotNames(t, dir, h))
	assert.Equal(t, []string{hourName(h) + ".jsonl"}, jsonlNames(t, dir, h), "no second file")
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 3, n, "counted once")
}

// TestAFileCountsAtMostTheMarkerCap asserts a file counts and removes no more
// than the cap of markers; the next file counts the rest.
func TestAFileCountsAtMostTheMarkerCap(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	putSlot(t, dir, 0, runEvent(h))
	for _, m := range []string{"aaaaaaaa", "bbbbbbbb", "cccccccc", "dddddddd", "eeeeeeee"} {
		putMarker(t, dir, h, m)
	}
	old := markerCap
	markerCap = 2
	t.Cleanup(func() { markerCap = old })
	s := openStore(t, dir)
	s.pass(c.now(), newProducer(), "")
	assert.Len(t, readMarkers(t, dir, h), 3)
	for range 2 {
		setOld(t, dir, h, c.now())
		s.pass(c.now(), newProducer(), "")
	}
	assert.Empty(t, readMarkers(t, dir, h))
	n, err := s.DroppedCount()
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 3, countNames(t, s, EventEventsDropped))
}

// TestADroppedEventHasItsOwnInstanceID asserts the dropped event the pass
// writes carries a random version 4 instance id that differs from the id of
// the recording process and of its command run event, and that the exported
// constructor does the same.
func TestADroppedEventHasItsOwnInstanceID(t *testing.T) {
	enable(t)
	useClock(t, realHour(0).Add(time.Minute))
	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	putSlot(t, dir, 0, runEvent(h))
	putMarker(t, dir, h, "aaaaaaaa")
	run := runEvent(time.Now())
	require.Equal(t, StatusRecorded, record(t, dir, run))

	own, err := processInstanceID()
	require.NoError(t, err)
	list, err := openStore(t, dir).List()
	require.NoError(t, err)
	var dropped []StoredEvent
	var runID any
	for _, ev := range list {
		switch {
		case ev.Name == EventEventsDropped:
			dropped = append(dropped, ev)
		case ev.UID == run.UID():
			runID = ev.Fields[attrInstanceID]
		}
	}
	require.Len(t, dropped, 1)
	id, _ := dropped[0].Fields[attrInstanceID].(string)
	assert.Regexp(t, randomIDPattern, id)
	assert.NotEqual(t, own, id)
	assert.Equal(t, own, runID, "the run event carries the process id")
	assert.NotEqual(t, runID, id)

	got := decodeEvent(t, NewEventsDropped(fixedNoon, 1, testContext()).Flat())[attrInstanceID]
	assert.NotEqual(t, own, got, "the exported constructor too")
	failingRandom(t)
	assert.True(t, NewEventsDropped(fixedNoon, 1, testContext()).failed())
}

// TestAPanicWhileRecordingDropsTheEvent asserts Record recovers a panic from
// anywhere below it, in the write path and in the hourly pass, and reports
// a failed recording.
func TestAPanicWhileRecordingDropsTheEvent(t *testing.T) {
	enable(t)
	c := useClock(t, realHour(0).Add(time.Minute))
	old := pauseHook
	t.Cleanup(func() { pauseHook = old })

	pauseHook = func(stage string) {
		if stage == "created" {
			panic("boom")
		}
	}
	assert.NotPanics(t, func() {
		assert.Equal(t, StatusFailed, record(t, t.TempDir(), runEvent(c.now())))
	})

	dir := t.TempDir()
	h := realHour(3 * time.Hour)
	putSlot(t, dir, 0, runEvent(h))
	pauseHook = func(stage string) {
		if stage == "listed" {
			panic("boom in the pass")
		}
	}
	assert.NotPanics(t, func() {
		assert.Equal(t, StatusFailed, record(t, dir, runEvent(c.now())))
	})
}

// TestStoreMethodsOnAMissingTelemetryDirectoryCreateNothing asserts the
// readers return empty, Reset and the deletes do nothing, and the directory
// is not created.
func TestStoreMethodsOnAMissingTelemetryDirectoryCreateNothing(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	list, err := s.List()
	require.NoError(t, err)
	assert.Empty(t, list)
	n, err := s.Count(CountLimit)
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = s.DroppedCount()
	require.NoError(t, err)
	assert.Zero(t, n)
	_, ok := s.ReadFacts("https://rancher.example.com")
	assert.False(t, ok)
	require.NoError(t, s.Reset())
	require.NoError(t, s.DeleteEvents())
	assert.Empty(t, readNames(t, dir))
}

// TestAnInstanceIDNotFromTheCallerIsTheProcessOwn asserts an instance id
// input that is a version 4 UUID is used and anything else is replaced by the
// process's own.
func TestAnInstanceIDNotFromTheCallerIsTheProcessOwn(t *testing.T) {
	in := baseInput()
	in.Invocation = invocation.Context{Version: "1.2.3"}
	own, err := processInstanceID()
	require.NoError(t, err)
	in.InstanceID = "3f2a6c1e-8b7d-4e5a-9c10-2d4f6a8b0c1e"
	assert.Equal(t, in.InstanceID, decodeEvent(t, NewCommandRun(in).Flat())[attrInstanceID])
	in.InstanceID = "text"
	assert.Equal(t, own, decodeEvent(t, NewCommandRun(in).Flat())[attrInstanceID])
}
