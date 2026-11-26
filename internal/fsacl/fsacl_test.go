package fsacl

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestHardenAcceptsAFileAndADirectory asserts both calls succeed on an
// existing path on every platform and leave the path usable by its owner.
func TestHardenAcceptsAFileAndADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.NoError(t, Harden(file))
	testfs.RequireOwnerOnly(t, file)

	sub := filepath.Join(dir, "d")
	require.NoError(t, os.Mkdir(sub, 0o700))
	require.NoError(t, HardenInheritable(sub))

	inner := filepath.Join(sub, "g")
	require.NoError(t, os.WriteFile(inner, []byte("y"), 0o600))
	testfs.RequireOwnerOnly(t, inner)
	got, err := os.ReadFile(inner)
	require.NoError(t, err)
	require.Equal(t, "y", string(got))
}

func openRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// TestCreateFileCreatesAnExclusiveReadWriteFile asserts the file is created
// empty and writable, that its name is the root's name joined with the name
// asked for, that a second create of the name fails with fs.ErrExist and
// leaves the first file alone, and that the file is owner-only.
func TestCreateFileCreatesAnExclusiveReadWriteFile(t *testing.T) {
	root, dir := openRoot(t)
	f, err := CreateFile(root, ".x-1", 0o600)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, ".x-1"), f.Name())
	_, err = f.WriteString("kept")
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	testfs.RequireOwnerOnly(t, f.Name())

	_, err = CreateFile(root, ".x-1", 0o600)
	require.ErrorIs(t, err, fs.ErrExist)
	pe, ok := errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, ".x-1", pe.Path)
	got, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	assert.Equal(t, "kept", string(got))
}

// TestCreateFileRefusesANameThatIsNotOneComponent asserts a name with a path
// separator, an empty name, "." and ".." create nothing.
func TestCreateFileRefusesANameThatIsNotOneComponent(t *testing.T) {
	root, dir := openRoot(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	for _, name := range []string{"", ".", "..", "sub/f", "sub" + string(filepath.Separator) + "f", "/abs"} {
		f, err := CreateFile(root, name, 0o600)
		require.Error(t, err, "%q", name)
		assert.Nil(t, f)
		_, ok := errors.AsType[*fs.PathError](err)
		assert.True(t, ok, "%q: %v", name, err)
		require.ErrorContains(t, err, "name is not a plain file name", "%q", name)
		require.ErrorContains(t, Mkdir(root, name, 0o700, false), "name is not a plain file name", "Mkdir %q", name)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sub"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestCreateTempNamesTheFileLikeOSCreateTemp asserts the last "*" of the
// pattern is replaced by a number, the number is added at the end when the
// pattern has none, and two calls create two files. Patterns with a second
// "*" are left out: the star stays in the name, which Windows refuses.
func TestCreateTempNamesTheFileLikeOSCreateTemp(t *testing.T) {
	root, dir := openRoot(t)
	for pattern, re := range map[string]string{
		".cfg-*":       `^\.cfg-[0-9]+$`,
		".edit-*.yaml": `^\.edit-[0-9]+\.yaml$`,
		"p*q":          `^p[0-9]+q$`,
		"plain":        `^plain[0-9]+$`,
		"":             `^[0-9]+$`,
	} {
		first, err := CreateTemp(root, pattern, 0o600)
		require.NoError(t, err, pattern)
		second, err := CreateTemp(root, pattern, 0o600)
		require.NoError(t, err, pattern)
		assert.NotEqual(t, first.Name(), second.Name())
		assert.Equal(t, dir, filepath.Dir(first.Name()))
		assert.Regexp(t, re, filepath.Base(first.Name()), pattern)
		require.NoError(t, first.Close())
		require.NoError(t, second.Close())
	}
}

// TestCreateTempRetriesAFileThatExists asserts a name that exists is skipped
// for the next random name, and that after the bound the error is the
// "createtemp" error of os.CreateTemp naming the pattern in the directory,
// after the 10000 names os.CreateTemp tries.
func TestCreateTempRetriesAFileThatExists(t *testing.T) {
	root, dir := openRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p-1"), []byte("old"), 0o600))
	next := []string{"1", "1", "2"}
	orig := randomSuffix
	t.Cleanup(func() { randomSuffix = orig })
	randomSuffix = func() string {
		n := next[0]
		next = next[1:]
		return n
	}
	f, err := CreateTemp(root, "p-*", 0o600)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "p-2"), f.Name())
	require.NoError(t, f.Close())
	old, err := os.ReadFile(filepath.Join(dir, "p-1"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(old))

	tries := 0
	randomSuffix = func() string {
		tries++
		return "1"
	}
	_, err = CreateTemp(root, "p-*", 0o600)
	assert.Equal(t, 10000, tries, "os.CreateTemp tries 10000 names")
	require.ErrorIs(t, err, fs.ErrExist)
	pe, ok := errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, "createtemp", pe.Op)
	assert.Equal(t, filepath.Join(dir, "p-*"), pe.Path)
}

// TestCreateTempErrorsHaveTheShapeOfOSCreateTemp asserts a pattern with a
// separator gives the "createtemp" error, and a create that fails for another
// reason gives the "open" error with the full path of the name.
func TestCreateTempErrorsHaveTheShapeOfOSCreateTemp(t *testing.T) {
	root, dir := openRoot(t)
	_, err := CreateTemp(root, "a"+string(filepath.Separator)+"b-*", 0o600)
	require.Error(t, err)
	pe, ok := errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, "createtemp", pe.Op)
	assert.Equal(t, "a"+string(filepath.Separator)+"b-*", pe.Path)

	require.NoError(t, root.Close())
	_, err = CreateTemp(root, "p-*", 0o600)
	require.Error(t, err)
	pe, ok = errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, "open", pe.Op)
	assert.Equal(t, dir, filepath.Dir(pe.Path))
}

// TestMkdirCreatesOneDirectory asserts the directory is created owner-only
// with either inheritance, that a second create fails with fs.ErrExist
// naming the full path, and that a name of more than one component is
// refused.
func TestMkdirCreatesOneDirectory(t *testing.T) {
	root, dir := openRoot(t)
	for _, inheritable := range []bool{false, true} {
		name := "d" + strconv.FormatBool(inheritable)
		require.NoError(t, Mkdir(root, name, 0o700, inheritable))
		testfs.RequireOwnerOnly(t, filepath.Join(dir, name))

		err := Mkdir(root, name, 0o700, inheritable)
		require.ErrorIs(t, err, fs.ErrExist)
		pe, ok := errors.AsType[*fs.PathError](err)
		require.True(t, ok)
		assert.Equal(t, "mkdir", pe.Op)
		assert.Equal(t, filepath.Join(dir, name), pe.Path)
	}
	for _, name := range []string{"", ".", "..", "a/b"} {
		require.Error(t, Mkdir(root, name, 0o700, false), "%q", name)
	}
	assert.NoDirExists(t, filepath.Join(dir, "a"))
}

// TestMkdirReportsTheCleanedPath asserts the error for a root opened on a
// relative name reads like the error of os.Mkdir: the name without a leading
// "./".
func TestMkdirReportsTheCleanedPath(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := os.OpenRoot(".")
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	require.NoError(t, Mkdir(root, "d", 0o700, false))
	err = Mkdir(root, "d", 0o700, false)
	pe, ok := errors.AsType[*fs.PathError](err)
	require.True(t, ok)
	assert.Equal(t, "d", pe.Path)
	assert.Equal(t, os.Mkdir("d", 0o700).(*fs.PathError).Path, pe.Path)
}
