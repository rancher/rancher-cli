//go:build windows

package config

// permCheckApplicable is false on Windows: os.FileInfo.Mode() synthesizes
// mode bits that don't reflect the real NTFS ACL, so warnLoosePerm's
// group/world-readable check would fire false positives there. The
// owner-only security descriptor a file is created with (fsacl.CreateFile and
// fsacl.CreateTemp) is the guarantee on this platform instead.
func permCheckApplicable() bool { return false }
