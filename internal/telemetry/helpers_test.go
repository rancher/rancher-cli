package telemetry

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// clock is a settable clock for the nowFunc seam.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// useClock replaces the clock seam for the test.
func useClock(t *testing.T, start time.Time) *clock {
	t.Helper()
	c := &clock{t: start}
	old := nowFunc
	nowFunc = c.now
	t.Cleanup(func() { nowFunc = old })
	return c
}

// enable lets recording run in the test binary and clears the server holder.
func enable(t *testing.T) {
	t.Helper()
	restore := SetEnabledForTest()
	ResetServerForTest()
	t.Cleanup(func() {
		restore()
		ResetServerForTest()
	})
}

// opLog collects the observed file operations.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *opLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.ops)
	l.ops = nil
	return out
}

var (
	opHourRe  = regexp.MustCompile(`events/[0-9]{10}`)
	opSlotRe  = regexp.MustCompile(`[0-9]{4}\.json`)
	opFactsRe = regexp.MustCompile(`servers/[0-9a-f]{32}\.json`)
)

// shape replaces the parts of observed paths that vary between runs.
func shape(ops []string) []string {
	out := make([]string, len(ops))
	for i, o := range ops {
		o = opHourRe.ReplaceAllString(o, "events/<hour>")
		o = opSlotRe.ReplaceAllString(o, "<slot>")
		o = opFactsRe.ReplaceAllString(o, "servers/<facts>")
		out[i] = o
	}
	return out
}

func observeOps(t *testing.T) *opLog {
	t.Helper()
	l := &opLog{}
	t.Cleanup(SetObserverForTest(l.add))
	return l
}

func testContext() invocation.Context {
	return invocation.Context{Version: "0.1.0-alpha.8", OS: "linux", Arch: "amd64"}
}

func runEvent(at time.Time) CommandRun {
	return NewCommandRun(RunInput{
		Time:       at,
		Invocation: testContext(),
		Command:    "get project",
		Outcome:    OutcomeOK,
		ErrorCodes: testCodes,
	})
}

func record(t *testing.T, dir string, ev Event) Status {
	t.Helper()
	return Record(ev, Options{ConfigDir: dir, DirExisted: true})
}

// hourPath is the path of an hour directory under a config directory.
func hourPath(dir string, hour time.Time) string {
	return filepath.Join(dir, "telemetry", "events", hourName(hour))
}

// putSlot writes the stored form of ev as slot n of its hour.
func putSlot(t *testing.T, dir string, n int, ev Event) string {
	t.Helper()
	p := hourPath(dir, ev.Hour())
	require.NoError(t, os.MkdirAll(p, 0o700))
	name := filepath.Join(p, slotFileName(n))
	require.NoError(t, os.WriteFile(name, ev.stored(), 0o600))
	return name
}

func slotFileName(n int) string { return fmt.Sprintf("%04d%s", n, slotSuffix) }

func readNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func skipOnWindows(t *testing.T, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip(why)
	}
}

// realHour returns the start of the UTC hour that began ago before now, by
// the real clock.
func realHour(ago time.Duration) time.Time {
	return time.Now().UTC().Add(-ago).Truncate(time.Hour)
}

func newProducer() producer {
	p, _ := producerOf(testContext(), "")
	return p
}

// testUUID returns a random version 4 UUID for a test.
func testUUID() string {
	id, err := newUUID()
	if err != nil {
		panic(err)
	}
	return id
}

// useDistinctSlots makes every slot choice a new slot number, in order. A
// test that asserts exact file operations would otherwise see an extra
// create when two random choices land on the same slot.
func useDistinctSlots(t *testing.T) {
	t.Helper()
	old := randIntn
	next := 0
	randIntn = func(n int) int {
		v := next % n
		next++
		return v
	}
	t.Cleanup(func() { randIntn = old })
}
