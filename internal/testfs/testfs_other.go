//go:build !windows

package testfs

import "testing"

// requireOwnerOnlyACL stands in for the Windows access list check so the
// package builds on every OS. Reaching it means a test asked for the Windows
// check on another OS.
func requireOwnerOnlyACL(tb testing.TB, path string) {
	tb.Helper()
	tb.Fatalf("%s: an access list is read on Windows only", path)
}

// GrantEveryoneRead does nothing here: on Linux and macOS the 0700 mode of
// the directory is what keeps other users out, and a test that needs another
// user's access to a directory runs on Windows only.
func GrantEveryoneRead(testing.TB, string) {}
