package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemedyDirForIsTheDirectoryMadeAbsolute asserts the directory the
// remedies name is the resolved one made absolute and cleaned, from
// whatever working directory: a relative spelling, one through .., one
// with a trailing separator, and an absolute path as it is.
func TestRemedyDirForIsTheDirectoryMadeAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	realWD, err := os.Getwd()
	require.NoError(t, err)
	want := filepath.Join(realWD, "acct")
	for _, spelling := range []string{"acct", "./x/../acct", "acct" + string(os.PathSeparator)} {
		assert.Equal(t, want, remedyDirFor(spelling), spelling)
	}
	abs := filepath.Join(t.TempDir(), "d")
	assert.Equal(t, abs, remedyDirFor(abs))
}
