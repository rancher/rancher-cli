//go:build !windows

package config

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// checkIdentityDirOwnership refuses credential-identity state this user
// does not own or that anyone else can reach: the identity directory, and
// the transition manifest, which is held to the same bar. Both name the
// keychain accounts this directory's credentials live under, so a file
// another user can write is a file another user can point at accounts of
// their own.
func checkIdentityDirOwnership(fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return errors.New("readable or writable by users other than its owner")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if int(st.Uid) != os.Getuid() {
		return errors.New("owned by another user")
	}
	return nil
}
