package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteRawConfig atomically writes already-validated config YAML bytes
// verbatim to path (0600) — `config edit`'s write-back path. It writes the
// user's edited buffer byte-for-byte rather than re-marshaling through a
// parsed *Config (which Config.Write does): re-marshaling would silently
// drop any field the parsed Config struct does not carry (the same
// forward-compatible fields Load's lenient parse accepts), plus any
// comments, discarding part of a user's finished edit. Shares
// atomicWriteFile's symlink refusal and temp-sibling-then-rename semantics.
func WriteRawConfig(path string, data []byte) error {
	return atomicWriteFile(path, data, 0600)
}

// hardenACLHook is hardenACL behind a package var, so a test can inject a
// failure to exercise atomicWriteFile's error-cleanup ordering without a
// real Windows host.
var hardenACLHook = hardenACL

// atomicWriteFile writes data to path with perm atomically, refusing symlinks and enforcing perms.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
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
	// closeAndRemove closes before removing — Windows can't remove an open
	// file (no FILE_SHARE_DELETE by default), so Remove-before-Close
	// silently leaks the temp file there.
	closeAndRemove := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
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
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return nil
}
