//go:build windows

package telemetry

import (
	"errors"
	"syscall"
)

const (
	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
)

// isShareViolation reports whether err is what Windows returns while another
// handle holds the file open: a read of a slot that is being written, or a
// removal of one that is open.
func isShareViolation(err error) bool {
	errno, ok := errors.AsType[syscall.Errno](err)
	return ok && (errno == errorAccessDenied || errno == errorSharingViolation)
}
