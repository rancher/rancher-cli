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
// failure for the directory it is still called on (the credential identity
// directory that already exists) without a real Windows host.
var hardenACLHook = fsacl.Harden

// createTempHook is fsacl.CreateTemp behind a package var, so a test can fail
// the create, or read the new file's access list right after it, without a
// real Windows host.
var createTempHook = fsacl.CreateTemp

// createFileHook is fsacl.CreateFile behind a package var, for the same
// tests as createTempHook.
var createFileHook = fsacl.CreateFile

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

	// The temp file is created inside an os.Root of the directory: on
	// Windows the create sets the owner-only security descriptor in the same
	// call, so no other local user can open it before the descriptor is set
	// and keep a handle that outlives a later change. The root closes after
	// the create; the file stays open.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	tmp, err := createTempHook(root, "."+filepath.Base(path)+"-*", 0600)
	_ = root.Close()
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// closeAndRemove closes before removing, on every system. The removal goes
	// by path, and on Windows the removal of a file with an open handle can
	// be refused, or the file can stay in the directory until the handle
	// closes, which would leave the temp file behind.
	closeAndRemove := func() {
		_ = tmp.Close()
		_ = removeFile(tmpPath)
	}

	if err := tmp.Chmod(perm); err != nil {
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
