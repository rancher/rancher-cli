//go:build unix

package telemetrycmd

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStatusDoesNotWarnForAStateFileThatIsNeitherADirectoryNorASymlink asserts
// a socket in place of the state file adds no warning: only a directory and a
// symlink are files the state write cannot replace.
func TestStatusDoesNotWarnForAStateFileThatIsNeitherADirectoryNorASymlink(t *testing.T) {
	controlsEnv(t)
	dir := newDir(t)
	putSlots(t, dir, 3, 1, 1)
	putFile(t, dir, "telemetry/upload/keep", "x")
	// A relative name keeps the socket path inside the limit of the system.
	t.Chdir(filepath.Join(dir, "telemetry", "upload"))
	l, err := net.Listen("unix", "state.json")
	if err != nil {
		t.Skipf("this system cannot create a socket here: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	_, _, warnings := statusOf(t, dir, uploadView)
	assert.Empty(t, warnings)
}
