//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// onlyCurrentUser reports whether the list has allow entries for the current
// user alone and no other kind of entry.
func onlyCurrentUser(tb testing.TB, a testfs.ACL) bool {
	tb.Helper()
	if a.Other != 0 || len(a.Allowed) == 0 {
		return false
	}
	user := testfs.CurrentUser(tb)
	for _, sid := range a.Allowed {
		if !sid.Equals(user) {
			return false
		}
	}
	return true
}

// TestEnsureDirSetsTheOwnerOnlyAccessList asserts a directory EnsureDir
// creates grants the current user alone, and that a file and a folder
// created in it afterwards inherit that list.
func TestEnsureDirSetsTheOwnerOnlyAccessList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	require.NoError(t, EnsureDir(dir))
	testfs.RequireOwnerOnly(t, dir)
	assert.True(t, testfs.ReadACL(t, dir).Protected, "the directory must not inherit from its parent")

	file := filepath.Join(dir, "file")
	f, err := os.Create(file)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	testfs.RequireOwnerOnly(t, file)

	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(sub, 0o700))
	testfs.RequireOwnerOnly(t, sub)
}

// TestEnsureDirKeepsTheAccessListOfAnExistingDirectory asserts a directory
// that exists keeps the list it had. The directory is created in the test's
// temporary folder and inherits its list, which grants more than the current
// user.
func TestEnsureDirKeepsTheAccessListOfAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "existing")
	require.NoError(t, os.Mkdir(dir, 0o700))
	before := testfs.ReadACL(t, dir)
	require.False(t, onlyCurrentUser(t, before), "the directory must start with an inherited list that grants more than the current user")

	require.NoError(t, EnsureDir(dir))

	after := testfs.ReadACL(t, dir)
	assert.Equal(t, before.Protected, after.Protected)
	assert.Equal(t, before.Flags, after.Flags)
	assert.Equal(t, before.Other, after.Other)
	require.Len(t, after.Allowed, len(before.Allowed))
	for i, sid := range before.Allowed {
		assert.Equal(t, sid.String(), after.Allowed[i].String())
	}
}
