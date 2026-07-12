//go:build !windows

package config

// hardenACL is a no-op on non-Windows platforms: atomicWriteFile's 0600
// chmod is the restriction mechanism there. On Windows (write_windows.go)
// NTFS ignores Unix mode bits, so an explicit owner-only DACL is set
// instead.
func hardenACL(string) error { return nil }
