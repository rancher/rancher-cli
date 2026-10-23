//go:build windows

package config

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// shareRetryBudget bounds how long renameFile, readFile, openFile,
// removeFile and removeAll keep retrying a file another handle has open. A variable so a test can set
// it to zero, which makes each call a single attempt.
var shareRetryBudget = 2 * time.Second

// isShareError reports whether err is what Windows returns while another
// handle blocks the call and may close soon. os.Rename is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which fails with ERROR_ACCESS_DENIED while
// any handle to the target was opened without FILE_SHARE_DELETE - and
// os.Open and os.ReadFile open with FILE_SHARE_READ|FILE_SHARE_WRITE
// only, so every reader of a file blocks its replacement. os.Remove
// (DeleteFile) needs the same delete access and fails with
// ERROR_SHARING_VIOLATION while such a handle is open. An open of the
// file while it is being replaced can fail with ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION.
//
// Windows returns ERROR_ACCESS_DENIED for a real permission failure too
// (a DACL that denies the user, a rename onto a directory), and the two
// causes cannot be told apart, so such a failure is retried as well and
// surfaces after the full budget, about 2 s, as the same error.
//
// ERROR_FILE_NOT_FOUND is not retried: the source of every rename here
// is a temporary file this process created under a unique name, so its
// absence is a real failure, and for a read or a removal a missing file
// is a real answer.
func isShareError(err error) bool {
	errno, ok := errors.AsType[windows.Errno](err)
	return ok && (errno == windows.ERROR_ACCESS_DENIED || errno == windows.ERROR_SHARING_VIOLATION)
}

// renameFile is os.Rename, retrying for up to shareRetryBudget while
// another handle to the target keeps the replace from happening.
func renameFile(oldpath, newpath string) error {
	return retryTransient(shareRetryBudget, isShareError, func() error {
		return os.Rename(oldpath, newpath)
	})
}

// readFile is os.ReadFile, retrying like renameFile while the file is
// being replaced.
func readFile(name string) ([]byte, error) {
	var b []byte
	err := retryTransient(shareRetryBudget, isShareError, func() error {
		var err error
		b, err = os.ReadFile(name)
		return err
	})
	return b, err
}

// openFile is os.Open, retrying like readFile.
func openFile(name string) (*os.File, error) {
	var f *os.File
	err := retryTransient(shareRetryBudget, isShareError, func() error {
		var err error
		f, err = os.Open(name)
		return err
	})
	return f, err
}

// removeFile is os.Remove, retrying like renameFile while another handle
// to the file keeps it from being deleted.
func removeFile(name string) error {
	return retryTransient(shareRetryBudget, isShareError, func() error {
		return os.Remove(name)
	})
}

// removeAll is os.RemoveAll, retrying like removeFile.
func removeAll(path string) error {
	return retryTransient(shareRetryBudget, isShareError, func() error {
		return os.RemoveAll(path)
	})
}
