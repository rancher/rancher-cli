//go:build !windows

package fsacl

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Harden is a no-op on non-Windows platforms: the 0600 mode the caller set
// is the restriction there. On Windows (fsacl_windows.go) NTFS ignores Unix
// mode bits; an explicit owner-only access list is set instead.
func Harden(string) error { return nil }

// HardenInheritable is a no-op on non-Windows platforms: a directory created
// with mode 0700 keeps its entries private there, and files created in it
// take the mode their creator asks for.
func HardenInheritable(string) error { return nil }

// createFile is an exclusive create with mode perm: the mode is the
// restriction on these platforms.
func createFile(root *os.Root, name string, perm fs.FileMode) (*os.File, error) {
	return root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
}

// mkdir creates the directory with mode perm: the mode is the restriction on
// these platforms.
func mkdir(root *os.Root, name string, perm fs.FileMode, _ bool) error {
	if err := root.Mkdir(name, perm); err != nil {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return newPathError("mkdir", filepath.Join(root.Name(), name), err)
	}
	return nil
}
