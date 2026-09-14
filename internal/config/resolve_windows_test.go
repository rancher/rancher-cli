//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveDirWithSourceWindowsSpellings asserts that the canonical
// value settles the spellings Windows accepts for one path: a forward
// separator, a doubled separator and a redundant element all reach the
// same value, and the volume is kept.
func TestResolveDirWithSourceWindowsSpellings(t *testing.T) {
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", "")

	volume := filepath.VolumeName(t.TempDir())
	want := volume + `\x\y`

	for _, spelling := range []string{
		volume + `\x\y`,
		volume + `/x/y`,
		volume + `\x\\y`,
		volume + `\x\z\..\y`,
		volume + `\x\y\`,
	} {
		t.Run(spelling, func(t *testing.T) {
			dir, source, err := ResolveDirWithSource(spelling)
			require.NoError(t, err)
			assert.Equal(t, DirSourceFlag, source)
			assert.Equal(t, want, dir)
		})
	}
}

// TestIsDefaultDirWindowsCasing asserts that %APPDATA%\rancher spelled in
// another casing is still the default directory: the filesystem decides
// the comparison, not the strings.
func TestIsDefaultDirWindowsCasing(t *testing.T) {
	t.Setenv(EnvConfigDir, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", t.TempDir())

	def, err := PlatformDefaultDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(def, 0700))

	isDefault, err := IsDefaultDir(strings.ToUpper(def))
	require.NoError(t, err)
	assert.True(t, isDefault)

	isDefault, err = IsDefaultDir(strings.ToLower(def))
	require.NoError(t, err)
	assert.True(t, isDefault)
}
