//go:build windows

package fsacl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// aclSummary is what a test needs to know about one object's access list.
type aclSummary struct {
	protected bool
	allowed   []*windows.SID
	other     int
	flags     []byte
}

func summarize(t *testing.T, path string) aclSummary {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(t, err)
	control, _, err := sd.Control()
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NotNil(t, dacl, "%s has no DACL", path)
	out := aclSummary{protected: control&windows.SE_DACL_PROTECTED != 0}
	for i := range dacl.AceCount {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(t, windows.GetAce(dacl, uint32(i), &ace))
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			out.other++
			continue
		}
		out.allowed = append(out.allowed, (*windows.SID)(unsafe.Pointer(&ace.SidStart)))
		out.flags = append(out.flags, ace.Header.AceFlags)
	}
	return out
}

func currentUser(t *testing.T) *windows.SID {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	return user.User.Sid
}

// TestHardenLeavesOneOwnerOnlyProtectedEntry asserts Harden gives a file a
// protected access list holding one allow entry for the current user, with
// no inheritance flag: the result the credential files already relied on
// before the code moved here.
func TestHardenLeavesOneOwnerOnlyProtectedEntry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.NoError(t, Harden(file))

	got := summarize(t, file)
	require.True(t, got.protected, "inheritance from the parent must be blocked")
	require.Zero(t, got.other)
	require.Len(t, got.allowed, 1)
	require.True(t, got.allowed[0].Equals(currentUser(t)))
	require.Zero(t, got.flags[0]&0x3, "the entry must not pass to children")
}

// requireOnlyCurrentUser asserts an access list grants at least one entry,
// every entry to the current user and nothing to anyone else. Windows can
// record an inherited grant as more than one entry for the same user.
func requireOnlyCurrentUser(t *testing.T, got aclSummary, what string) {
	t.Helper()
	require.Zero(t, got.other, "%s must grant nothing to anyone else", what)
	require.NotEmpty(t, got.allowed, "%s must inherit the owner entry", what)
	user := currentUser(t)
	for _, sid := range got.allowed {
		require.True(t, sid.Equals(user), "%s grants an entry to %s", what, sid)
	}
}

// TestInheritableDirectoryGivesNewFilesOnlyTheCurrentUser asserts a file
// created in a directory hardened with HardenInheritable carries an access
// list that grants only the current user, without a call on the file; the
// directory's own list is protected.
func TestInheritableDirectoryGivesNewFilesOnlyTheCurrentUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, HardenInheritable(dir))

	dirACL := summarize(t, dir)
	require.True(t, dirACL.protected)
	requireOnlyCurrentUser(t, dirACL, "the directory")
	require.True(t, slices.ContainsFunc(dirACL.flags, func(f byte) bool { return f&0x3 == 0x3 }),
		"the directory must carry an entry that passes to new files and subdirectories")

	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	got := summarize(t, file)
	requireOnlyCurrentUser(t, got, "a new file")

	sub := filepath.Join(dir, "s")
	require.NoError(t, os.Mkdir(sub, 0o700))
	requireOnlyCurrentUser(t, summarize(t, sub), "a new subdirectory")
}
