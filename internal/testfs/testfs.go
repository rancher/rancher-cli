// Package testfs is test support for the places where Windows differs from
// Unix: symlinks need a privilege, mode bits do not deny access, a
// directory's modification time updates late, and an access list can record
// one grant as two entries. A test calls these helpers instead of
// runtime.GOOS, os.Symlink, os.Chmod, os.Geteuid, a modification time read
// or the raw access list calls. Each difference is then handled in one place.
// It also writes a file a test then runs, because Linux refuses to run a file
// that a forked child still has open for writing.
// Tests only; no production package imports it.
package testfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The package's own tests replace these to run the Windows branch of each
// helper on every OS.
var (
	goos    = runtime.GOOS
	geteuid = os.Geteuid
	symlink = os.Symlink
)

// errPrivilegeNotHeld is ERROR_PRIVILEGE_NOT_HELD, the error a Windows
// process gets when it creates a symlink without the privilege. os.Symlink
// returns it wrapped in an *os.LinkError and returns no other error that
// means symlinks are unavailable (syscall.EWINDOWS comes only from
// syscall.Symlink, which os does not call). The value is numeric because
// syscall.ERROR_PRIVILEGE_NOT_HELD exists only in Windows builds and the
// tests of this package run the Windows branch on every OS.
const errPrivilegeNotHeld syscall.Errno = 1314

// OnWindows reports whether the test runs on Windows. Use it for an
// expectation that differs by OS and does not skip.
func OnWindows() bool {
	return goos == "windows"
}

// SkipOnWindows skips the test on Windows with why.
func SkipOnWindows(tb testing.TB, why string) {
	tb.Helper()
	if OnWindows() {
		tb.Skip(why)
	}
}

// Symlink creates a symlink at link that points to target. On Windows the
// test is skipped when the account lacks the privilege to create symlinks;
// any other error fails the test there and on every other OS.
func Symlink(tb testing.TB, target, link string) {
	tb.Helper()
	err := symlink(target, link)
	if err == nil {
		return
	}
	if OnWindows() && errors.Is(err, errPrivilegeNotHeld) {
		tb.Skipf("this Windows account cannot create symlinks: %v", err)
	}
	tb.Fatalf("create the symlink %s: %v", link, err)
}

// RestrictAccess sets the mode of path and restores the previous mode when
// the test ends. It skips the test on Windows, where mode bits do not deny
// access, and when running as root, which ignores them.
func RestrictAccess(tb testing.TB, path string, mode fs.FileMode) {
	tb.Helper()
	if OnWindows() {
		tb.Skip("Windows does not deny access by mode bits")
	}
	if geteuid() == 0 {
		tb.Skip("root ignores mode bits")
	}
	info, err := os.Stat(path)
	require.NoError(tb, err)
	previous := info.Mode().Perm()
	tb.Cleanup(func() { _ = os.Chmod(path, previous) })
	require.NoError(tb, os.Chmod(path, mode))
}

// DirectoryInPlaceOfFile creates a directory at path, with its parents. A
// read of path as a file then fails with an error other than not-exist on
// every OS. A plain file in place of a parent directory reads as not-exist on
// Windows and does not do this.
func DirectoryInPlaceOfFile(tb testing.TB, path string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(path, 0o700))
}

// Snapshot returns one sorted line for every path under dir, dir itself
// included as ".": the slash-separated path relative to dir, the file mode,
// the size, the modification time and, for a regular file, the contents or,
// for a symlink, its target as os.Readlink returns it (not slash-normalized:
// a comparison holds on one OS only). Every entry is read with os.Lstat and not from
// the directory listing, whose size and time can be out of date on NTFS. A
// test compares two snapshots to see that an operation changed nothing. A dir
// that does not exist, or lies below a plain file, gives the one line
// "missing" on every OS.
//
// A directory's own modification time is left out on Windows, where NTFS
// updates it some time after an entry changes. The names below the directory
// still show a created or removed entry.
func Snapshot(tb testing.TB, dir string) []string {
	tb.Helper()
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return []string{"missing"}
	}
	var out []string
	require.NoError(tb, filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		require.NoError(tb, err)
		info, err := os.Lstat(p)
		require.NoError(tb, err)
		rel, err := filepath.Rel(dir, p)
		require.NoError(tb, err)
		modified := info.ModTime().UnixNano()
		if info.IsDir() && OnWindows() {
			modified = 0
		}
		line := fmt.Sprintf("%s %v %d %d", filepath.ToSlash(rel), info.Mode(), info.Size(), modified)
		switch {
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			require.NoError(tb, err)
			line += " " + string(data)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			require.NoError(tb, err)
			line += " -> " + target
		}
		out = append(out, line)
		return nil
	}))
	slices.Sort(out)
	return out
}

// Tree returns the sorted, slash-separated paths of everything below dir,
// relative to dir. dir itself is not listed.
func Tree(tb testing.TB, dir string) []string {
	tb.Helper()
	var out []string
	require.NoError(tb, filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		require.NoError(tb, err)
		rel, err := filepath.Rel(dir, p)
		require.NoError(tb, err)
		if rel != "." {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}))
	slices.Sort(out)
	return out
}

// RequireOwnerOnly fails the test unless only the owner can reach path. On
// Unix that is no group or other permission bit. On Windows it is an access
// list with at least one allow entry, every entry for the current user and
// no entry of another type; one inheritable grant is recorded as two
// entries and passes.
func RequireOwnerOnly(tb testing.TB, path string) {
	tb.Helper()
	if OnWindows() {
		requireOwnerOnlyACL(tb, path)
		return
	}
	info, err := os.Stat(path)
	require.NoError(tb, err)
	require.Zero(tb, info.Mode().Perm()&0o077, "%s must be owner-only, its mode is %v", path, info.Mode().Perm())
}
