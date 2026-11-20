//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// TestCloudCredentialFromFileOpenedWithoutSharing asserts that a file another
// handle holds open with no read sharing is refused with the same message as a
// file that does not exist, and that no request is sent.
func TestCloudCredentialFromFileOpenedWithoutSharing(t *testing.T) {
	restoreRawArgv(t)
	path := filepath.Join(t.TempDir(), "locked.txt")
	require.NoError(t, os.WriteFile(path, []byte("fake-locked-secret-0001\n"), 0o600))
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	defer func() { _ = windows.CloseHandle(handle) }()
	stdout, stderr, _, posts := ccFileRun(t, path)
	assert.Empty(t, posts)
	assert.Contains(t, stdout+stderr, "cannot read the --from-file file "+path)
}
