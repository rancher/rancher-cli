package testfs

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// WriteExecutableFile creates path, or truncates it, writes data and closes
// it. The mode is 0o755 before the umask and applies when the file is
// created; a file that exists keeps its mode. Use it where a caller must
// handle the error and WriteExecutable everywhere else.
//
// No fork may happen while the file is open for writing, because Linux
// refuses to run a file that any process has open for writing ("text file
// busy"), and a child forked in that window keeps its copy of the descriptor
// until it runs its program. The function holds the read side of
// syscall.ForkLock from before the open until after the close. Every process
// start takes the write side of the lock, so no process starts while the file
// is open for writing, and a start that began earlier has finished before this
// function takes the read side. Nothing in the function may take the read side
// a second time or start a process: a read lock taken twice while a process
// start waits for the write side deadlocks.
func WriteExecutableFile(path string, data []byte) error {
	return writeExecutableFile(path, data, (*os.File).Close)
}

// WriteExecutable is WriteExecutableFile that fails the test on an error.
func WriteExecutable(tb testing.TB, path string, data []byte) {
	tb.Helper()
	require.NoError(tb, WriteExecutableFile(path, data))
}

// writeExecutableFile is WriteExecutableFile with the function that closes
// the file passed in, so a test can see that the lock is still held at the
// close.
func writeExecutableFile(path string, data []byte, closeFile func(*os.File) error) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create the executable file: %w", err)
	}
	_, writeErr := f.Write(data)
	closeErr := closeFile(f)
	if writeErr != nil {
		return fmt.Errorf("write the executable file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close the executable file: %w", closeErr)
	}
	return nil
}
