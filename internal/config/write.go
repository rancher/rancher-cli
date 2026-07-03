package config

import (
	"fmt"
	"os"
	"path/filepath"
)

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
