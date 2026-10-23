//go:build !windows

package telemetry

// isShareViolation reports whether err is a file another handle holds open.
// Only Windows has that failure.
func isShareViolation(error) bool { return false }
