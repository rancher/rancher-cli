package testfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWriteExecutableCreatesAFileWithTheExecuteBit asserts the writer
// stores the data, sets the owner execute bit on a new file (off Windows) and
// replaces the data of a file it created earlier.
func TestWriteExecutableCreatesAFileWithTheExecuteBit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")

	WriteExecutable(t, path, []byte("first"))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "first", string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.NotZero(t, info.Mode()&0o100, "the owner execute bit is set, the mode is %v", info.Mode())
	}

	WriteExecutable(t, path, []byte("second, longer than the first"))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "second, longer than the first", string(got))
}

// TestWriteExecutableFileNamesThePathOfAFailure asserts the error names the
// path when the directory is missing.
func TestWriteExecutableFileNamesThePathOfAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "tool")

	err := WriteExecutableFile(path, []byte("x"))

	require.ErrorContains(t, err, path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestWriteExecutableFileWaitsForAFork asserts the writer waits while a test
// holds the write side of syscall.ForkLock, the side every process start
// takes, which shows the writer holds the read side while it works, and that
// it has not created the file before it has the lock.
func TestWriteExecutableFileWaitsForAFork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	finished := make(chan struct{})
	var writeErr error

	syscall.ForkLock.Lock()
	locked := true
	defer func() {
		if locked {
			syscall.ForkLock.Unlock()
		}
		<-finished
	}()
	go func() {
		defer close(finished)
		writeErr = WriteExecutableFile(path, []byte("x"))
	}()

	isFinished := func() bool {
		select {
		case <-finished:
			return true
		default:
			return false
		}
	}
	require.Never(t, isFinished, 100*time.Millisecond, 10*time.Millisecond, "the writer finished while the test held the write side of the lock")
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "the writer created the file before it had the lock")
	syscall.ForkLock.Unlock()
	locked = false

	require.Eventually(t, isFinished, 5*time.Second, 10*time.Millisecond, "the writer did not finish after the lock was released")
	require.NoError(t, writeErr)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
}

// TestWriteExecutableFileHoldsTheLockUntilTheFileIsClosed asserts the read
// side of syscall.ForkLock is still held when the file is closed, so no fork
// can start while the file is open for writing.
func TestWriteExecutableFileHoldsTheLockUntilTheFileIsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	closed := false

	err := writeExecutableFile(path, []byte("x"), func(f *os.File) error {
		closed = true
		if syscall.ForkLock.TryLock() {
			syscall.ForkLock.Unlock()
			assert.Fail(t, "the write side of the lock was free when the file was closed")
		}
		return f.Close()
	})

	require.NoError(t, err)
	assert.True(t, closed, "the file was closed")
	require.True(t, syscall.ForkLock.TryLock(), "the writer released the lock")
	syscall.ForkLock.Unlock()
}

// stressLimit is the time the stress test allows. It is short enough to fail
// at once when a writer waits on the lock forever, and long enough for the
// slower process starts when the package runs under strace.
const stressLimit = 20 * time.Second

// TestWriteExecutableFileNeverLeaksADescriptorToAFork asserts that scripts
// written while other goroutines start processes run without the "text file
// busy" error. Linux refuses to run a file while any process, a forked child
// included, has it open for writing. It runs on Linux only: macOS and Windows
// have no such error. It fails when the writers do not finish within
// stressLimit.
func TestWriteExecutableFileNeverLeaksADescriptorToAFork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS and Windows do not refuse to run a file that is open for writing")
	}
	truePath, err := exec.LookPath("true")
	require.NoError(t, err)
	dir := t.TempDir()

	var mu sync.Mutex
	var failures []error
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), stressLimit)
	defer cancel()
	var starters, writers sync.WaitGroup
	for range 4 {
		starters.Go(func() {
			for ctx.Err() == nil {
				_ = exec.Command(truePath).Run()
			}
		})
	}
	for w := range 8 {
		writers.Go(func() {
			for i := range 20 {
				path := filepath.Join(dir, "script-"+strconv.Itoa(w)+"-"+strconv.Itoa(i))
				if err := WriteExecutableFile(path, []byte("#!/bin/sh\nexit 0\n")); err != nil {
					record(err)
					continue
				}
				if err := exec.CommandContext(ctx, path).Run(); err != nil {
					record(err)
				}
			}
		})
	}
	written := make(chan struct{})
	go func() {
		writers.Wait()
		close(written)
	}()
	select {
	case <-written:
	case <-ctx.Done():
		t.Fatalf("the writers did not finish within %v", stressLimit)
	}
	cancel()
	starters.Wait()

	for _, err := range failures {
		require.NotErrorIs(t, err, syscall.ETXTBSY, "text file busy")
		require.NoError(t, err)
	}
}
