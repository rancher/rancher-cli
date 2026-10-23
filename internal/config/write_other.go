//go:build !windows

package config

import "os"

// renameFile is os.Rename. A rename or a removal here succeeds even
// while other processes hold the file open, so there is nothing to
// retry; the Windows versions (write_windows.go) retry.
func renameFile(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// readFile is os.ReadFile; see renameFile.
func readFile(name string) ([]byte, error) { return os.ReadFile(name) }

// openFile is os.Open; see renameFile.
func openFile(name string) (*os.File, error) { return os.Open(name) }

// removeFile is os.Remove; see renameFile.
func removeFile(name string) error { return os.Remove(name) }

// removeAll is os.RemoveAll; see renameFile.
func removeAll(path string) error { return os.RemoveAll(path) }
