package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rancher/rancher-cli/internal/fsacl"
)

// ReadRawConfig reads config YAML bytes verbatim from path - `config
// edit`'s read of the real file before the editor opens. It retries on
// Windows while another process holds the file or replaces it, as every
// read of the config directory's files in this package does.
func ReadRawConfig(path string) ([]byte, error) {
	return readFile(path)
}

// WriteRawConfig atomically writes already-validated config YAML bytes
// verbatim to path (0600) - `config edit`'s write-back path. It writes the
// user's edited buffer byte-for-byte rather than re-marshaling through a
// parsed *Config (which Config.Write does): re-marshaling would silently
// drop any field the parsed Config struct does not carry (the same
// forward-compatible fields Load's lenient parse accepts), plus any
// comments, discarding part of a user's finished edit. Shares
// atomicWriteFile's symlink refusal and temp-sibling-then-rename semantics.
func WriteRawConfig(path string, data []byte) error {
	return atomicWriteFile(path, data, 0600)
}

// hardenACLHook is fsacl.Harden behind a package var, so a test can inject a
// failure to exercise atomicWriteFile's error-cleanup ordering without a
// real Windows host.
var hardenACLHook = fsacl.Harden

// hardenDirHook is fsacl.HardenInheritable behind a package var, so a test
// can record the call EnsureDir makes on a new directory, or fail it, without
// a real Windows host.
var hardenDirHook = fsacl.HardenInheritable

// HardenTempFileACL exposes fsacl.Harden to other internal packages whose own
// temp files hold config/credential content but which don't go through
// atomicWriteFile - e.g. the config-edit scratch file (newTempCopy), which
// sits open in the editor for the duration of an edit and deserves the same
// owner-only DACL as atomicWriteFile's real writes.
func HardenTempFileACL(path string) error {
	return hardenACLHook(path)
}

// afterWriteHook runs after each write this package makes that a process
// taking no lock can observe: a file written by atomic rename, an identity
// record written or removed, the store-change record written or removed, a
// keychain set or delete, a credential helper store or erase. what names
// the write. A test runs a reader from it to ask what that reader resolves
// to at that moment. Nil in production.
var afterWriteHook func(what string)

// SetAfterWriteHookForTest sets afterWriteHook; call the returned restore
// in cleanup. The calling test must not use t.Parallel(): the hook is a
// package-level seam.
func SetAfterWriteHookForTest(fn func(what string)) (restore func()) {
	old := afterWriteHook
	afterWriteHook = fn
	return func() { afterWriteHook = old }
}

func afterWrite(what string) {
	if afterWriteHook != nil {
		afterWriteHook(what)
	}
}

// atomicWriteFile writes data to path with perm atomically, refusing symlinks and enforcing perms.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := EnsureDir(dir); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write through symlink: %s", path)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// closeAndRemove closes before removing - Windows can't remove an open
	// file (no FILE_SHARE_DELETE by default), so Remove-before-Close
	// silently leaks the temp file there.
	closeAndRemove := func() {
		_ = tmp.Close()
		_ = removeFile(tmpPath)
	}

	if err := tmp.Chmod(perm); err != nil {
		closeAndRemove()
		return err
	}

	if err := hardenACLHook(tmpPath); err != nil {
		closeAndRemove()
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		closeAndRemove()
		return err
	}

	if err := tmp.Sync(); err != nil {
		closeAndRemove()
		return err
	}

	if err := tmp.Close(); err != nil {
		_ = removeFile(tmpPath)
		return err
	}

	if err := renameFile(tmpPath, path); err != nil {
		_ = removeFile(tmpPath)
		return err
	}
	afterWrite(filepath.Base(path))
	return nil
}
