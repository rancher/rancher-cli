//go:build windows

package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// setShareRetryBudget sets shareRetryBudget for one test. The callers do
// not use t.Parallel(): the budget is a package-level seam.
func setShareRetryBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := shareRetryBudget
	shareRetryBudget = d
	t.Cleanup(func() { shareRetryBudget = old })
}

// holdOpen opens path with os.Open, which shares read and write but not
// delete (FILE_SHARE_READ|FILE_SHARE_WRITE), as every reader of a config
// file does, and closes it in cleanup unless the test closes it first.
func holdOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// tempSiblings lists the temporary files atomicWriteFile left beside
// path.
func tempSiblings(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+filepath.Base(path)+"-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// errnoCounts tallies errors by the errno they carry, for t.Log.
type errnoCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *errnoCounts) add(err error) {
	key := "no errno: " + err.Error()
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		key = fmt.Sprintf("errno %d (%v)", uintptr(errno), errno)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[key]++
}

func (c *errnoCounts) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.n {
		n += v
	}
	return n
}

func (c *errnoCounts) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.n) == 0 {
		return "none"
	}
	keys := slices.Sorted(maps.Keys(c.n))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, c.n[k]))
	}
	return strings.Join(parts, ", ")
}

// TestRenameOverAnOpenTargetFailsWithoutRetry asserts the failure the
// retry exists for: with the retry disabled, replacing a file another
// handle holds open fails with ERROR_ACCESS_DENIED, the target keeps its
// bytes and the temporary file is removed.
func TestRenameOverAnOpenTargetFailsWithoutRetry(t *testing.T) {
	setShareRetryBudget(t, 0)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	holdOpen(t, path)

	err := atomicWriteFile(path, []byte("new"), 0o600)
	t.Logf("rename over an open target without retry: %v", err)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Empty(t, tempSiblings(t, path), "the temporary file is removed")

	b, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "old", string(b))
}

// TestRenameOverAnOpenTargetSucceedsOnceReleased asserts the fix: a write
// over a file another handle holds open succeeds once that handle closes
// within the budget, and the file holds the new bytes.
func TestRenameOverAnOpenTargetSucceedsOnceReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	f := holdOpen(t, path)

	var wg sync.WaitGroup
	wg.Go(func() {
		time.Sleep(50 * time.Millisecond)
		_ = f.Close()
	})
	start := time.Now()
	err := atomicWriteFile(path, []byte("new"), 0o600)
	elapsed := time.Since(start)
	wg.Wait()
	t.Logf("rename over a target released after 50ms: err=%v elapsed=%v", err, elapsed)
	require.NoError(t, err)

	b, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "new", string(b))
	assert.Empty(t, tempSiblings(t, path))
}

// TestRenameOverAnOpenTargetGivesUpAfterTheBudget asserts that a handle
// held past the budget fails the write with ERROR_ACCESS_DENIED after
// retrying, within the budget plus slack, with the temporary file
// removed.
func TestRenameOverAnOpenTargetGivesUpAfterTheBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	holdOpen(t, path)

	start := time.Now()
	err := atomicWriteFile(path, []byte("new"), 0o600)
	elapsed := time.Since(start)
	t.Logf("rename over a target held past the budget: err=%v elapsed=%v budget=%v", err, elapsed, shareRetryBudget)
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED)
	assert.Greater(t, elapsed, 500*time.Millisecond, "retried before giving up")
	assert.Less(t, elapsed, shareRetryBudget+5*time.Second, "gave up near the budget")
	assert.Empty(t, tempSiblings(t, path), "the temporary file is removed")
}

// TestShareRetryLeavesOtherErrorsAlone asserts that only the two errors
// another handle causes are retried: a rename whose source is missing and
// a read of a missing file return at once, the same error as without the
// retry.
func TestShareRetryLeavesOtherErrorsAlone(t *testing.T) {
	dir := t.TempDir()
	// A retried call would take the whole budget, so a call that returns
	// in under half of it was not retried, at any machine speed.
	const budget = time.Minute
	setShareRetryBudget(t, budget)

	start := time.Now()
	err := renameFile(filepath.Join(dir, "missing"), filepath.Join(dir, "target"))
	require.ErrorIs(t, err, windows.ERROR_FILE_NOT_FOUND)
	assert.Less(t, time.Since(start), budget/2, "a missing source is not retried")

	start = time.Now()
	_, err = readFile(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Less(t, time.Since(start), budget/2, "a missing file is a real answer")

	start = time.Now()
	_, err = openFile(filepath.Join(dir, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Less(t, time.Since(start), budget/2)
}

// holdExclusive opens path with no sharing at all (share mode 0), so any
// other open of it fails with ERROR_SHARING_VIOLATION until the returned
// release runs. Cleanup releases it if the test has not.
func holdExclusive(t *testing.T, path string) (release func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	var once sync.Once
	release = func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}

// TestReadsRetryASharingViolation asserts the read side of the retry: with
// the retry disabled, a read of a file another handle holds without
// sharing fails with ERROR_SHARING_VIOLATION; with it, readFile and
// openFile succeed once that handle closes within the budget.
func TestReadsRetryASharingViolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-cache.yaml")
	require.NoError(t, os.WriteFile(path, []byte("cached"), 0o600))

	t.Run("without retry", func(t *testing.T) {
		setShareRetryBudget(t, 0)
		release := holdExclusive(t, path)
		defer release()
		_, err := readFile(path)
		t.Logf("read of an exclusively held file without retry: %v", err)
		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
		_, err = openFile(path)
		require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
	})

	for _, tc := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{"readFile", readFile},
		{"openFile", func(name string) ([]byte, error) {
			f, err := openFile(name)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return io.ReadAll(f)
		}},
	} {
		t.Run(tc.name+" once released", func(t *testing.T) {
			release := holdExclusive(t, path)
			var wg sync.WaitGroup
			wg.Go(func() {
				time.Sleep(50 * time.Millisecond)
				release()
			})
			start := time.Now()
			b, err := tc.read(path)
			elapsed := time.Since(start)
			wg.Wait()
			t.Logf("%s of a file released after 50ms: err=%v elapsed=%v", tc.name, err, elapsed)
			require.NoError(t, err)
			assert.Equal(t, "cached", string(b))
		})
	}
}

// TestRemoveOfAnOpenFile asserts the removal side of the retry: with the
// retry disabled, removing a file another handle holds open (os.Open
// shares read and write but not delete) fails with one of the errors
// isShareError accepts; with it, removeFile and removeAll succeed once
// that handle closes within the budget, and the file is gone.
func TestRemoveOfAnOpenFile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(string) error
	}{
		{"removeFile", removeFile},
		{"removeAll", removeAll},
	} {
		t.Run(tc.name+" without retry", func(t *testing.T) {
			setShareRetryBudget(t, 0)
			path := filepath.Join(t.TempDir(), "token-cache.yaml")
			require.NoError(t, os.WriteFile(path, []byte("cached"), 0o600))
			holdOpen(t, path)

			err := tc.remove(path)
			t.Logf("%s of an open file without retry: %v", tc.name, err)
			require.Error(t, err)
			assert.True(t, isShareError(err), "ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION, got %v", err)
			assert.FileExists(t, path)
		})
		t.Run(tc.name+" once released", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token-cache.yaml")
			require.NoError(t, os.WriteFile(path, []byte("cached"), 0o600))
			f := holdOpen(t, path)

			var wg sync.WaitGroup
			wg.Go(func() {
				time.Sleep(50 * time.Millisecond)
				_ = f.Close()
			})
			start := time.Now()
			err := tc.remove(path)
			elapsed := time.Since(start)
			wg.Wait()
			t.Logf("%s of a file released after 50ms: err=%v elapsed=%v", tc.name, err, elapsed)
			require.NoError(t, err)
			assert.NoFileExists(t, path)
		})
	}
}

// TestConcurrentWritersOfOneFile asserts that writers racing to replace
// one file, as the lost-update tests do on purpose, all succeed with the
// retry. With the retry disabled it logs what the renames return, the
// record of which errors a concurrent replace produces.
func TestConcurrentWritersOfOneFile(t *testing.T) {
	const writers, writes = 4, 50
	payloads := []string{"a\n", "b\n", "c\n", "d\n"}
	run := func(t *testing.T) *errnoCounts {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cachebinding-stamp")
		var errs errnoCounts
		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				for range writes {
					if err := atomicWriteFile(path, []byte(payloads[w]), 0o600); err != nil {
						errs.add(err)
					}
				}
			})
		}
		wg.Wait()
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, payloads, string(b), "the file holds one complete write")
		assert.Empty(t, tempSiblings(t, path), "no temporary file is left")
		return &errs
	}

	t.Run("without retry", func(t *testing.T) {
		setShareRetryBudget(t, 0)
		errs := run(t)
		t.Logf("concurrent writers without retry: %d of %d writes failed: %s", errs.total(), writers*writes, errs)
	})
	t.Run("with retry", func(t *testing.T) {
		errs := run(t)
		t.Logf("concurrent writers with retry: %d of %d writes failed: %s", errs.total(), writers*writes, errs)
		assert.Zero(t, errs.total())
	})
}

// TestReadsDuringReplace measures whether a plain read of a file fails
// while a writer replaces it, logging the failures by errno, and asserts
// that the reads this package makes (readFile, openFile) never fail and
// always see one complete write. The readers hold the file open most of
// the time, so the writer's own retries decide how many replaces land;
// the count is logged, and the loop stops at a deadline so a starved
// writer cannot stretch the test past a few seconds.
func TestReadsDuringReplace(t *testing.T) {
	const readers, writes = 2, 100
	const deadline = 4 * time.Second
	payloads := []string{strings.Repeat("a", 512), strings.Repeat("b", 512)}
	path := filepath.Join(t.TempDir(), "token-cache.yaml")
	require.NoError(t, os.WriteFile(path, []byte(payloads[0]), 0o600))

	var plain, retried, opened, writeErrs errnoCounts
	var reads atomic.Int64
	var torn sync.Map
	done := make(chan struct{})
	check := func(b []byte) {
		if !slices.Contains(payloads, string(b)) {
			torn.Store(len(b), true)
		}
	}
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				reads.Add(1)
				if _, err := os.ReadFile(path); err != nil {
					plain.add(err)
				}
				if b, err := readFile(path); err != nil {
					retried.add(err)
				} else {
					check(b)
				}
				f, err := openFile(path)
				if err != nil {
					opened.add(err)
					continue
				}
				b, err := io.ReadAll(f)
				_ = f.Close()
				if err != nil {
					opened.add(err)
				} else {
					check(b)
				}
				runtime.Gosched()
			}
		})
	}
	start := time.Now()
	replaced := 0
	for i := 0; i < writes && time.Since(start) < deadline; i++ {
		if err := atomicWriteFile(path, []byte(payloads[i%2]), 0o600); err != nil {
			writeErrs.add(err)
			continue
		}
		replaced++
	}
	elapsed := time.Since(start)
	close(done)
	wg.Wait()

	t.Logf("reads during replaces: %d replaces of %d in %v, %d reader iterations", replaced, writes, elapsed, reads.Load())
	t.Logf("reads during replaces: plain os.ReadFile errors: %s", &plain)
	t.Logf("reads during replaces: readFile errors: %s; openFile errors: %s; write errors: %s", &retried, &opened, &writeErrs)
	assert.Positive(t, replaced, "at least one replace landed")
	assert.Zero(t, retried.total(), "readFile never fails during a replace")
	assert.Zero(t, opened.total(), "openFile never fails during a replace")
	torn.Range(func(k, _ any) bool {
		t.Errorf("a read returned %v bytes that are neither write", k)
		return true
	})
}

// TestRootReadsDuringRootReplace measures the identity records' and the
// store-change record's access pattern: os.Root opens with
// FILE_SHARE_DELETE and Root.Rename replaces with POSIX semantics, so
// neither side is expected to fail. It logs the failures by errno and
// asserts nothing about them.
func TestRootReadsDuringRootReplace(t *testing.T) {
	const readers, writes = 4, 100
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	require.NoError(t, root.WriteFile("record", []byte("0"), 0o600))

	var readErrs, writeErrs errnoCounts
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := root.ReadFile("record"); err != nil {
					readErrs.add(err)
				}
			}
		})
	}
	for i := range writes {
		tmp := fmt.Sprintf(".record-%d", i)
		if err := root.WriteFile(tmp, []byte(fmt.Sprint(i)), 0o600); err != nil {
			writeErrs.add(err)
			continue
		}
		if err := root.Rename(tmp, "record"); err != nil {
			writeErrs.add(err)
			_ = root.Remove(tmp)
		}
	}
	close(done)
	wg.Wait()

	t.Logf("os.Root reads during %d Root.Rename replaces: read errors: %s; write errors: %s", writes, &readErrs, &writeErrs)
}
