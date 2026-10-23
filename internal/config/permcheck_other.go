//go:build !windows

package config

// permCheckApplicable reports whether warnLoosePerm's Unix-permission
// check is meaningful on this platform. True everywhere except Windows,
// where os.FileInfo.Mode() synthesizes mode bits that don't reflect the
// real NTFS ACL (see internal/fsacl's Harden, the actual guarantee there).
func permCheckApplicable() bool { return true }
