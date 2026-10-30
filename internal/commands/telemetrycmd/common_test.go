package telemetrycmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// TestConfigDirectoryExistsWarnsWhenItCannotBeRead asserts a missing config
// directory is not an error, an existing one exists, and a stat error other
// than not-exist gives one warning that names the directory and the cause.
func TestConfigDirectoryExistsWarnsWhenItCannotBeRead(t *testing.T) {
	root := t.TempDir()

	exists, warnings := configDirectoryExists(filepath.Join(root, "missing"))
	assert.False(t, exists)
	assert.Nil(t, warnings)

	exists, warnings = configDirectoryExists(root)
	assert.True(t, exists)
	assert.Nil(t, warnings)

	skipOnWindows(t, "a path below a plain file reads as not-exist")
	putFile(t, root, "plain", "x")
	dir := filepath.Join(root, "plain", "config")
	exists, warnings = configDirectoryExists(dir)
	assert.False(t, exists)
	require.Len(t, warnings, 1)
	assert.True(t, strings.HasPrefix(warnings[0], "cannot read the config directory "+dir+": "), warnings[0])
	assert.Contains(t, warnings[0], "not a directory")
}

// TestStoreWarningNamesTheCause asserts a telemetry path that is a symlink or
// not a directory gets its own warning, also when the error is wrapped, and
// any other error is named as a read failure.
func TestStoreWarningNamesTheCause(t *testing.T) {
	dir := newDir(t)
	path := filepath.Join(dir, "telemetry")
	unsafe := path + " is a symlink or not a directory and is not read"
	assert.Equal(t, unsafe, storeWarning(dir, telemetry.ErrUnsafeDir))
	assert.Equal(t, unsafe, storeWarning(dir, fmt.Errorf("open: %w", telemetry.ErrUnsafeDir)))
	assert.Equal(t, "cannot read "+path+": disk failure", storeWarning(dir, errors.New("disk failure")))
}
