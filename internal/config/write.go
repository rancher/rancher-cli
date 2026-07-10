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
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		_ = tmp.Close()
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		_ = tmp.Close()
		return err
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		_ = tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return err
	}

	return nil
}
