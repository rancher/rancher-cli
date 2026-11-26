//go:build windows

package config

import "io/fs"

// checkIdentityDirOwnership has nothing to check on Windows: the mode bits
// Go reports for a directory there are synthesized, not the access control
// list the filesystem enforces. Containment rests on the same owner-only
// security descriptor every other credential file in the directory is created
// with.
func checkIdentityDirOwnership(fs.FileInfo) error { return nil }
