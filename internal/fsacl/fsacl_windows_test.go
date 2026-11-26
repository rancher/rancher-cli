//go:build windows

package fsacl

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

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

// sharedDirectory returns a directory with an inheritable grant to Everyone,
// as a config directory has when another local user can reach it, and a root
// opened on it.
func sharedDirectory(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	testfs.GrantEveryoneRead(t, dir)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// createKind says what a create makes, which decides the access list the
// object must have.
type createKind int

const (
	kindFile createKind = iota
	kindPlainDirectory
	kindInheritableDirectory
)

// requireOwnerOnlyList asserts an access list is protected, grants the
// current user and nobody else, and has the inheritance flags of kind. A file
// has exactly one entry and none passes to children. A directory made without
// inheritance has no entry that passes to children. A directory made with it
// has at least one entry that passes to new files and directories: Windows
// can record one inheritable grant as more than one entry.
func requireOwnerOnlyList(t *testing.T, got testfs.ACL, kind createKind) {
	t.Helper()
	require.True(t, got.Protected, "inheritance from the parent must be blocked")
	require.False(t, got.HasEntryForAnotherUser(t), "the list must grant nothing to anyone else")
	require.NotEmpty(t, got.Allowed)
	passing := 0
	for _, flags := range got.Flags {
		if flags&0x3 == 0x3 {
			passing++
		}
	}
	switch kind {
	case kindFile:
		require.Len(t, got.Allowed, 1)
		require.Zero(t, got.Flags[0]&0x3, "the entry must not pass to children")
	case kindPlainDirectory:
		for _, flags := range got.Flags {
			require.Zero(t, flags&0x3, "an entry must not pass to children")
		}
	case kindInheritableDirectory:
		require.NotZero(t, passing, "an entry must pass to new files and directories")
	}
}

// checkAtCreate installs the seam that reads the access list through the
// handle NtCreateFile returned, as soon as it is returned and before the
// helper does anything else with it, and asserts it has the list kind needs
// there. It restores the seam when the test ends and returns the number of
// creates checked. A create that makes the object without the descriptor and
// sets the list afterwards fails here, because the object is read before
// that call; a create that does not reach the seam leaves the count at zero.
// The caller must not use t.Parallel(): the seam is a package-level variable.
func checkAtCreate(t *testing.T, kind createKind) *int {
	t.Helper()
	orig := afterCreateHook
	t.Cleanup(func() { afterCreateHook = orig })
	count := new(int)
	afterCreateHook = func(h windows.Handle) {
		t.Helper()
		*count++
		requireOwnerOnlyList(t, testfs.ReadHandleACL(t, h), kind)
	}
	return count
}

// TestCreateFileHasOnlyTheCurrentUserBeforeAnyWrite asserts that in a
// directory with an inheritable grant to Everyone the file CreateFile
// returns has, as the create returns and before anything is written to it, a
// protected access list with one entry for the current user and none that
// passes to children, and that the handle writes, syncs, changes the mode,
// closes and renames.
func TestCreateFileHasOnlyTheCurrentUserBeforeAnyWrite(t *testing.T) {
	root, dir := sharedDirectory(t)
	checked := checkAtCreate(t, kindFile)
	f, err := CreateFile(root, ".x-1", 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.Equal(t, 1, *checked, "the create must reach the seam")

	requireOwnerOnlyList(t, testfs.ReadACL(t, f.Name()), kindFile)

	_, err = f.WriteString("data")
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Chmod(0o600))
	require.NoError(t, f.Close())
	require.NoError(t, os.Rename(f.Name(), filepath.Join(dir, "final")))
	testfs.ReadACL(t, filepath.Join(dir, "final")).RequireOnlyCurrentUser(t, "the renamed file")
	data, err := os.ReadFile(filepath.Join(dir, "final"))
	require.NoError(t, err)
	require.Equal(t, "data", string(data))
}

// TestCreateTempHasOnlyTheCurrentUserBeforeAnyWrite asserts the same for the
// file CreateTemp returns.
func TestCreateTempHasOnlyTheCurrentUserBeforeAnyWrite(t *testing.T) {
	root, _ := sharedDirectory(t)
	checked := checkAtCreate(t, kindFile)
	f, err := CreateTemp(root, ".y-*", 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.Equal(t, 1, *checked, "the create must reach the seam")
	requireOwnerOnlyList(t, testfs.ReadACL(t, f.Name()), kindFile)
}

// TestCreatedFileIsRemovableAfterClose asserts a file the helper created can
// be removed once its handle is closed, which the sites do on a failed write.
func TestCreatedFileIsRemovableAfterClose(t *testing.T) {
	root, _ := sharedDirectory(t)
	f, err := CreateTemp(root, ".z-*", 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(f.Name()))
}

// TestMkdirHasOnlyTheCurrentUserBeforeAnythingIsCreatedInIt asserts that in a
// directory with an inheritable grant to Everyone a directory made by Mkdir
// has, as the create returns, a protected access list for the current user
// alone: with an entry that passes to children when inheritable is true, and
// with none when false. A file made in the inheritable directory holds no
// entry for anyone else.
func TestMkdirHasOnlyTheCurrentUserBeforeAnythingIsCreatedInIt(t *testing.T) {
	root, dir := sharedDirectory(t)

	checked := checkAtCreate(t, kindPlainDirectory)
	require.NoError(t, Mkdir(root, "plain", 0o700, false))
	require.Equal(t, 1, *checked, "the create must reach the seam")
	requireOwnerOnlyList(t, testfs.ReadACL(t, filepath.Join(dir, "plain")), kindPlainDirectory)

	checked = checkAtCreate(t, kindInheritableDirectory)
	require.NoError(t, Mkdir(root, "inherit", 0o700, true))
	require.Equal(t, 1, *checked, "the create must reach the seam")
	requireOwnerOnlyList(t, testfs.ReadACL(t, filepath.Join(dir, "inherit")), kindInheritableDirectory)
	child := filepath.Join(dir, "inherit", "f")
	require.NoError(t, os.WriteFile(child, []byte("x"), 0o600))
	testfs.ReadACL(t, child).RequireOnlyCurrentUser(t, "a file in the inheritable directory")
	require.False(t, testfs.ReadACL(t, child).HasEntryForAnotherUser(t))
}

// TestCreateOldWayLeavesTheEntryForEveryone is the control of the tests
// above: a file made with os.WriteFile in the same kind of directory, before
// any call that sets its access list, holds an inherited entry for Everyone,
// and the check those tests use reports it. It asserts that the check can
// fail, so the tests above cannot pass on a create that sets the list
// afterwards.
func TestCreateOldWayLeavesTheEntryForEveryone(t *testing.T) {
	_, dir := sharedDirectory(t)
	old := filepath.Join(dir, "old")
	require.NoError(t, os.WriteFile(old, []byte("x"), 0o600))
	got := testfs.ReadACL(t, old)
	require.True(t, got.HasEntryForAnotherUser(t), "the file must inherit the grant to Everyone")
	require.False(t, got.Protected)
	require.NoError(t, Harden(old))
	require.False(t, testfs.ReadACL(t, old).HasEntryForAnotherUser(t))

	oldDir := filepath.Join(dir, "olddir")
	require.NoError(t, os.Mkdir(oldDir, 0o700))
	require.True(t, testfs.ReadACL(t, oldDir).HasEntryForAnotherUser(t), "the directory must inherit the grant to Everyone")

	// The handle the seam reads is the one a plain create returns, and it
	// reports the same entry: the seam can tell a create without the
	// descriptor from one with it.
	handleFile, err := os.OpenFile(filepath.Join(dir, "handle"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = handleFile.Close() })
	require.True(t, testfs.ReadHandleACL(t, windows.Handle(handleFile.Fd())).HasEntryForAnotherUser(t),
		"a plain create must show the inherited entry through its handle")
}

// TestCreateFileDoesNotFollowALinkAtTheName asserts that with a junction at
// the name, both creates fail, and that the directory the junction's target
// would be created in stays empty and the junction stays in place. The
// junction points to a name that does not exist inside target, so a create
// that followed the junction would succeed and leave that name in target. The
// test does not assert fs.ErrExist: for a create-new of a file at a name that
// is a directory reparse point, Windows answers access denied (the call asks
// for a non-directory file), not a collision, and the helper does not turn
// that status into "exists" because CreateTemp would then retry on a real
// permission error.
func TestCreateFileDoesNotFollowALinkAtTheName(t *testing.T) {
	root, dir := sharedDirectory(t)
	target := t.TempDir()
	link := filepath.Join(dir, "link")
	testfs.Junction(t, filepath.Join(target, "missing"), link)

	f, err := CreateFile(root, "link", 0o600)
	require.Error(t, err)
	require.Nil(t, f)
	t.Logf("CreateFile at a junction: %v", err)
	err = Mkdir(root, "link", 0o700, false)
	require.Error(t, err)
	t.Logf("Mkdir at a junction: %v", err)

	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Empty(t, entries)
	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeIrregular, "the junction must stay a junction")
}

// invalidNameText is the text of the error for a refused name.
const invalidNameText = "the name ends in a dot or a space, has a colon, or is a device name such as NUL"

// TestCreateRefusesANameWin32WouldChange asserts a name with a colon (a
// stream of a file), a trailing dot or space (which Win32 paths drop) or a
// device name without an extension creates nothing, for a file and for a
// directory, and that the error is not a name that exists, matches
// ERROR_INVALID_NAME and says what makes the name invalid. Each row, against
// filepath.IsLocal of the Go version in go.mod, which is the rule os.Root
// applies: "f:stream" has a colon (refused by the helper and by IsLocal);
// "d.", "d " and "d. ." end in a dot or space (refused by the helper only,
// IsLocal accepts them); "NUL", "nul", "COM1" and "aux" are device names
// with no extension (refused by IsLocal on every Windows version).
func TestCreateRefusesANameWin32WouldChange(t *testing.T) {
	root, dir := sharedDirectory(t)
	for _, name := range []string{"f:stream", "d.", "d ", "d. .", "NUL", "nul", "COM1", "aux"} {
		f, err := CreateFile(root, name, 0o600)
		require.Error(t, err, "CreateFile %q", name)
		require.Nil(t, f)
		require.NotErrorIs(t, err, fs.ErrExist, "CreateFile %q", name)
		require.ErrorIs(t, err, windows.ERROR_INVALID_NAME, "CreateFile %q", name)
		require.ErrorContains(t, err, invalidNameText, "CreateFile %q", name)
		err = Mkdir(root, name, 0o700, false)
		require.Error(t, err, "Mkdir %q", name)
		require.NotErrorIs(t, err, fs.ErrExist, "Mkdir %q", name)
		require.ErrorIs(t, err, windows.ERROR_INVALID_NAME, "Mkdir %q", name)
		require.ErrorContains(t, err, "mkdir "+filepath.Join(dir, name)+": "+invalidNameText, "Mkdir %q", name)
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestCreateAgreesWithIsLocalForADeviceNameWithAnExtension asserts that for a
// device name with an extension the helper does what filepath.IsLocal says,
// which is what os.Root does: since Windows 11 such a name ("con.txt") is a
// plain file name and is created, and on an older Windows it is a device and
// is refused. The answer depends on the Windows version, so the test reads it
// from IsLocal instead of fixing it. A name that is created is a plain file
// or directory of that name, and is removed.
func TestCreateAgreesWithIsLocalForADeviceNameWithAnExtension(t *testing.T) {
	root, dir := sharedDirectory(t)
	for name, directory := range map[string]bool{"con.txt": false, "prn.log": true} {
		var err error
		if directory {
			err = Mkdir(root, name, 0o700, false)
		} else {
			var f *os.File
			f, err = CreateFile(root, name, 0o600)
			if err == nil {
				require.NoError(t, f.Close())
			}
		}
		if !filepath.IsLocal(name) {
			require.Error(t, err, "%q is refused where IsLocal refuses it", name)
			continue
		}
		require.NoError(t, err, "%q is created where IsLocal accepts it", name)
		entries, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		require.Len(t, entries, 1)
		require.Equal(t, name, entries[0].Name())
		require.Equal(t, directory, entries[0].IsDir())
		require.NoError(t, root.Remove(name))
	}
}

// TestCreateFileIsCaseInsensitive asserts a name that differs only in case
// from an existing file is a name that exists.
func TestCreateFileIsCaseInsensitive(t *testing.T) {
	root, _ := sharedDirectory(t)
	f, err := CreateFile(root, "Case", 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = CreateFile(root, "case", 0o600)
	require.ErrorIs(t, err, fs.ErrExist)
}
