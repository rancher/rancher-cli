//go:build !windows

package fsacl

// Harden is a no-op on non-Windows platforms: the 0600 mode the caller set
// is the restriction there. On Windows (fsacl_windows.go) NTFS ignores Unix
// mode bits; an explicit owner-only access list is set instead.
func Harden(string) error { return nil }

// HardenInheritable is a no-op on non-Windows platforms: a directory created
// with mode 0700 keeps its entries private there, and files created in it
// take the mode their creator asks for.
func HardenInheritable(string) error { return nil }
