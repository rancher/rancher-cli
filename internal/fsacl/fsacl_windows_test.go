//go:build windows

package fsacl

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestHardenLeavesOneOwnerOnlyProtectedEntry asserts Harden gives a file a
// protected access list holding one allow entry for the current user, with
// no inheritance flag: the result the credential files already relied on
// before the code moved here.
func TestHardenLeavesOneOwnerOnlyProtectedEntry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.NoError(t, Harden(file))

	got := testfs.ReadACL(t, file)
	require.True(t, got.Protected, "inheritance from the parent must be blocked")
	require.Zero(t, got.Other)
	require.Len(t, got.Allowed, 1)
	require.True(t, got.Allowed[0].Equals(testfs.CurrentUser(t)))
	require.Zero(t, got.Flags[0]&0x3, "the entry must not pass to children")
}

// TestInheritableDirectoryGivesNewFilesOnlyTheCurrentUser asserts a file
// created in a directory hardened with HardenInheritable carries an access
// list that grants only the current user, without a call on the file; the
// directory's own list is protected.
func TestInheritableDirectoryGivesNewFilesOnlyTheCurrentUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, HardenInheritable(dir))

	dirACL := testfs.ReadACL(t, dir)
	require.True(t, dirACL.Protected)
	dirACL.RequireOnlyCurrentUser(t, "the directory")
	require.True(t, slices.ContainsFunc(dirACL.Flags, func(f byte) bool { return f&0x3 == 0x3 }),
		"the directory must carry an entry that passes to new files and subdirectories")

	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	testfs.ReadACL(t, file).RequireOnlyCurrentUser(t, "a new file")

	sub := filepath.Join(dir, "s")
	require.NoError(t, os.Mkdir(sub, 0o700))
	testfs.ReadACL(t, sub).RequireOnlyCurrentUser(t, "a new subdirectory")
}
