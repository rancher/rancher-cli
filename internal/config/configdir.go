package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// mkdirDir is os.Mkdir behind a package var, so a test can make the create
// of the directory report that it exists.
var mkdirDir = os.Mkdir

// EnsureDir creates the config directory dir when it is missing; every place
// that creates it calls this function.
//
// A directory that exists is left as it is on every system, with no change to
// its permissions or its access list: it may have been created by something
// else. A missing directory is created with mode 0700, and its missing
// parents with mode 0700 and nothing else. On Linux and macOS the mode makes
// the directory owner-only. On Windows the mode does nothing, so the new
// directory also gets an access list that grants full control to the current
// user only and that files and folders created in it afterwards inherit, set
// once right after the create. If setting the list fails, the new directory
// is removed and the error is returned.
//
// Only the process whose create succeeds sets the list. A process that dies
// between the create and the list call leaves the directory without it.
// Another process that finds the directory between the two can create lock
// files in it that keep the list they inherit; data files get their own
// owner-only list on every write. If the list fails after another process
// has written into the directory, the removal fails, the directory stays
// without the list and the error is still returned.
//
// Two fallbacks call os.MkdirAll on the directory itself to return the error
// os.MkdirAll returns for an existing path that is not a directory, or to
// accept a directory another process created first. They create the
// directory only in a rare race: another process created it, failed to set
// the list and removed it, or a file in its place was removed, between this
// call's check and the fallback. The directory then has no list, and the
// command still succeeds.
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
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	switch err := mkdirDir(dir, 0o700); {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		return os.MkdirAll(dir, 0o700)
	default:
		return err
	}
	if err := hardenDirHook(dir); err != nil {
		_ = removeFile(dir)
		return fmt.Errorf("cannot set the owner-only access list of %s: %w", dir, err)
	}
	return nil
}
