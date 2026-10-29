//go:build !windows

package testfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequireOwnerOnlyOnWindowsReadsTheAccessList asserts that with the OS
// set to windows the helper takes the access list branch, which on another OS
// is a stub that fails with a message saying so.
func TestRequireOwnerOnlyOnWindowsReadsTheAccessList(t *testing.T) {
	setGOOS(t, "windows")
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	r := run(t, func(tb testing.TB) { tb.Helper(); RequireOwnerOnly(tb, file) })
	assert.True(t, r.failed)
	assert.Contains(t, r.text(), "an access list is read on Windows only")
}
