package config

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startConfigWrite launches the fakehelper binary in "configwrite" mode for
// the given dir and server name. It returns a channel that receives stdout
// lines and a channel that receives the process exit error. The process is
// killed in t.Cleanup so goroutines always delete.
func startConfigWrite(t *testing.T, dir, serverName string, nolock bool) (<-chan string, <-chan error) {
	t.Helper()
	args := []string{"configwrite"}
	if nolock {
		args = append(args, "-nolock")
	}
	args = append(args, dir, serverName)

	cmd := exec.Command(fakeHelperBin(t), args...)
	// Child stderr streams through the test binary's stderr: a child that
	// exits on an error path explains itself in the test output instead of
	// vanishing (each line is already prefixed "configwrite:").
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	lines := make(chan string, 32)
	wait := make(chan error, 1)

	// One goroutine, strictly ordered: drain stdout to EOF, only then Wait.
	// Wait closes the StdoutPipe when the process exits, so a concurrent
	// Wait races the scanner and can kill it mid-drain ("read |0: file
	// already closed"), silently dropping the child's final lines — the
	// os/exec-documented misuse this harness shipped with, caught in CI by
	// the in-band scanner-error instrumentation.
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		// A scanner read error would close the lines channel while the child
		// may still be running — indistinguishable from a silent child exit.
		// Surface it as an in-band line so whatever assertion trips next
		// names the real cause.
		if err := sc.Err(); err != nil {
			lines <- "scanner-error: " + err.Error()
		}
		close(lines)
		wait <- cmd.Wait()
	}()

	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return lines, wait
}

// expectLine reads the next line from ch within timeout and asserts it equals
// want. who is used in fatal messages only.
func expectLine(t *testing.T, ch <-chan string, want, who string, timeout time.Duration) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("%s: child stdout closed before %q", who, want)
		}
		if got != want {
			t.Fatalf("%s: got line %q, want %q", who, got, want)
		}
	case <-time.After(timeout):
		t.Fatalf("%s: timed out after %v waiting for %q", who, timeout, want)
	}
}

// createSignal writes an empty sentinel file at filepath.Join(dir, name).
func createSignal(t *testing.T, dir, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
}

// waitExit reads from waitCh within timeout and requires no error. who is used
// in fatal messages only.
func waitExit(t *testing.T, waitCh <-chan error, who string, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-waitCh:
		require.NoErrorf(t, err, "%s: subprocess exited with error", who)
	case <-time.After(timeout):
		t.Fatalf("%s: subprocess did not exit within %v", who, timeout)
	}
}

// TestConfigWriteNoLockLosesUpdate is the red test that pins the lost-update
// bug class: two processes that load config.yaml before either writes will
// clobber each other's entry. The test PASSES by asserting the deterministic
// loss — exactly one of two server entries survives. This is the bug class
// that WithLock closes.
//
// Protocol: both children print "ready" → parent creates start → both poll
// and load (printing "loaded") → parent creates release → both write → one
// entry survives.
//
// No t.Parallel(): fakehelper is compiled once via a shared package-level
// sync.Once; the process-level configLockMu would also serialize -nolock
// children that somehow called WithLock, but they don't here.
func TestConfigWriteNoLockLosesUpdate(t *testing.T) {
	dir := t.TempDir()

	linesA, waitA := startConfigWrite(t, dir, "server-a", true)
	linesB, waitB := startConfigWrite(t, dir, "server-b", true)

	// Wait for both children to announce readiness before releasing start.
	expectLine(t, linesA, "ready", "A", 10*time.Second)
	expectLine(t, linesB, "ready", "B", 10*time.Second)

	// Both are running. Release the start barrier so they both load.
	createSignal(t, dir, "start")

	// Both must report "loaded" — they now hold stale snapshots (both read
	// an empty Servers map before either wrote).
	expectLine(t, linesA, "loaded", "A", 10*time.Second)
	expectLine(t, linesB, "loaded", "B", 10*time.Second)

	// Release the write barrier. Both children will now write concurrently.
	createSignal(t, dir, "release")

	waitExit(t, waitA, "A", 10*time.Second)
	waitExit(t, waitB, "B", 10*time.Second)

	// Load the resulting config. Because both children loaded an empty map
	// and each wrote a 1-entry map, the second write atomically overwrites
	// the first — exactly one entry survives. The assertion documents the
	// bug class: concurrent unprotected writes produce a deterministic
	// lost update.
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Len(t, cfg.Servers, 1, "without the lock, one of the two concurrent writes overwrites the other; exactly one entry survives")
}

// TestConfigWriteWithLockPreservesUpdate is the green test that pins the fix:
// two processes each wrapped in WithLock serialize through the flock so the
// second's load happens after the first's write, and both entries survive.
//
// The contention window (300ms) proves the second process is parked on the
// flock and has not yet loaded — its "loaded" must appear after the first's
// "done" in the combined event stream.
//
// No t.Parallel(): uses fakeHelperOnce (shared package-level binary build).
func TestConfigWriteWithLockPreservesUpdate(t *testing.T) {
	dir := t.TempDir()

	linesA, waitA := startConfigWrite(t, dir, "server-a", false)
	linesB, waitB := startConfigWrite(t, dir, "server-b", false)

	expectLine(t, linesA, "ready", "A", 10*time.Second)
	expectLine(t, linesB, "ready", "B", 10*time.Second)

	// Both are running and blocked on start. Release start — both will now
	// race for the flock. One acquires it and loads; the other blocks.
	createSignal(t, dir, "start")

	// Collect events from both children in arrival order so we can assert
	// the serialization ordering after the release barrier.
	type tagged struct {
		who, line string
	}
	combined := make(chan tagged, 32)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for l := range linesA {
			combined <- tagged{"A", l}
		}
	}()
	go func() {
		defer wg.Done()
		for l := range linesB {
			combined <- tagged{"B", l}
		}
	}()
	go func() {
		wg.Wait()
		close(combined)
	}()

	// Wait for the FIRST "loaded" — the child that won the flock race. Note
	// its identity as "first".
	var firstWho string
	select {
	case ev, ok := <-combined:
		require.True(t, ok, "combined channel closed before first loaded")
		require.Equal(t, "loaded", ev.line, "expected loaded from winner, got %q from %s", ev.line, ev.who)
		firstWho = ev.who
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for first loaded")
	}
	secondWho := "B"
	if firstWho == "B" {
		secondWho = "A"
	}

	// Contention window: the second child is parked on the flock and must
	// not have printed "loaded" yet. A 300ms silence proves it.
	select {
	case ev, ok := <-combined:
		if ok {
			t.Fatalf("%s: unexpected event %q during contention window (second child must be blocked on the flock, not yet loaded)", ev.who, ev.line)
		}
	case <-time.After(300 * time.Millisecond):
		// expected: the flock is still held by the first child
	}

	// Release the write barrier. The first child will write its server entry
	// and exit WithLock, releasing the flock. The second child will then
	// acquire the flock, load the updated config (with the first child's
	// entry), print "loaded", write its own entry, and print "done".
	createSignal(t, dir, "release")

	// Collect all remaining events until both children have printed "done".
	var eventLog []tagged
	doneCount := 0
	deadline := time.After(15 * time.Second)
	for doneCount < 2 {
		select {
		case ev, ok := <-combined:
			if !ok {
				t.Fatalf("combined channel closed with only %d done events (a child exited without done, or its scanner died); log: %v", doneCount, eventLog)
			}
			eventLog = append(eventLog, ev)
			if ev.line == "done" {
				doneCount++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both children to finish; got %d done events, log: %v", doneCount, eventLog)
		}
	}

	waitExit(t, waitA, "A", 10*time.Second)
	waitExit(t, waitB, "B", 10*time.Second)

	// Assert event ordering: the second child's "loaded" must appear after
	// the first child's "done". This proves the second read happened after
	// the first write — the serialization the flock provides.
	firstDoneIdx := -1
	secondLoadedIdx := -1
	for i, ev := range eventLog {
		if ev.who == firstWho && ev.line == "done" && firstDoneIdx < 0 {
			firstDoneIdx = i
		}
		if ev.who == secondWho && ev.line == "loaded" && secondLoadedIdx < 0 {
			secondLoadedIdx = i
		}
	}
	require.GreaterOrEqual(t, firstDoneIdx, 0, "first child (%s) must print done; log: %v", firstWho, eventLog)
	require.GreaterOrEqual(t, secondLoadedIdx, 0, "second child (%s) must print loaded after release; log: %v", secondWho, eventLog)
	assert.Less(t, firstDoneIdx, secondLoadedIdx,
		"second child's loaded (idx %d) must come after first child's done (idx %d); log: %v",
		secondLoadedIdx, firstDoneIdx, eventLog)

	// Both server entries must survive — no lost update.
	cfg, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, cfg.Servers, "server-a", "server-a must survive")
	assert.Contains(t, cfg.Servers, "server-b", "server-b must survive")
}
