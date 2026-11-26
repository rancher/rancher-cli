package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/rancher/rancher-cli/internal/fsacl"
)

// mkdirDir is fsacl.Mkdir behind a package var, so a test can make the create
// of a directory report that it exists, fail it, or record it.
var mkdirDir = fsacl.Mkdir

// EnsureDir creates the config directory dir when it is missing; every place
// that creates it calls this function.
//
// A directory that exists is left as it is on every system, with no change to
// its permissions or its access list: it may have been created by something
// else. A missing directory is created with mode 0700, and its missing
// parents with mode 0700 and nothing else. On Linux and macOS the mode makes
// the directory owner-only. On Windows the mode does nothing, so the new
// directory is created with an access list that grants full control to the
// current user only and that files and folders created in it afterwards
// inherit; the list is part of the create, so the directory is never visible
// without it. Lock files created in the directory inherit that list; data
// files get their own owner-only list when they are created.
//
// Two fallbacks call os.MkdirAll on the directory itself to return the error
// os.MkdirAll returns for an existing path that is not a directory, or to
// accept a directory another process created first. They create the
// directory only in a rare race: another process created it and removed it,
// or a file in its place was removed, between this call's check and the
// fallback. The directory then has no list, and the command still succeeds.
//
// An empty dir returns the error os.MkdirAll returns for it. Any other dir is
// cleaned first, so "a/b/.." is the directory "a".
func EnsureDir(dir string) error {
	if dir == "" {
		return os.MkdirAll(dir, 0o700)
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err == nil && info.IsDir() {
		return nil
	}
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	switch err := mkdirDir(root, filepath.Base(dir), 0o700, true); {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return os.MkdirAll(dir, 0o700)
	default:
		return err
	}
}
