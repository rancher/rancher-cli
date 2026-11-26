// Package fsacl creates files and directories that are owner-only from the
// moment they exist, and sets owner-only access on paths that already exist,
// where the operating system ignores Unix mode bits. On Windows an NTFS
// security descriptor replaces the 0600 and 0700 modes; on every other
// platform the mode the caller passes is the restriction and the calls do
// nothing else. The package imports only the standard library and
// golang.org/x/sys/windows; any package may use it without taking on
// configuration, credential or network code.
package fsacl

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxTempTries is how many names CreateTemp tries before it reports that the
// name exists, as os.CreateTemp does.
const maxTempTries = 10000

var (
	errNotOneComponent = errors.New("name is not a plain file name")
	errPatternHasSep   = errors.New("pattern contains path separator")
)

// randomSuffix returns the random number CreateTemp puts in a name; a test
// replaces it to force a name that exists.
var randomSuffix = func() string { return strconv.FormatUint(uint64(rand.Uint32()), 10) }

// CreateFile creates the file name inside the directory root and opens it for
// reading and writing. The create fails when name exists, even as a link, and
// never leaves the directory. name is one path component; on Windows a name
// that ends in a dot or a space, a device name such as NUL, and a name with a
// colon are refused too, because Win32 paths would not reach the file created
// under that name.
//
// On Windows the file has a protected security descriptor with one entry that
// grants the current user full control, set by the same call that creates the
// file: no other user can open a handle before it is set. The mode bits
// do nothing there, except that a perm without the owner write bit sets the
// read-only attribute. On every other platform the file gets mode perm
// (before the umask) and nothing else.
//
// The errors are *fs.PathError values; one for an existing name matches
// fs.ErrExist.
func CreateFile(root *os.Root, name string, perm fs.FileMode) (*os.File, error) {
	if !isOneComponent(name) {
		return nil, newPathError("openat", name, errNotOneComponent)
	}
	return createFile(root, name, perm)
}

// CreateTemp creates a new file in the directory root as CreateFile does,
// with a name that pattern gives, the way os.CreateTemp names a file: the
// last "*" in pattern is replaced by a random number, or the number is added
// at the end when there is no "*". pattern has no path separator. A name
// that exists is replaced by another random name, up to a bound, as
// os.CreateTemp does. The name of the returned file is root.Name() joined
// with the name created.
//
// The errors have the shape os.CreateTemp gives them: an *fs.PathError with
// the operation "createtemp" when the pattern has a separator or every name
// tried exists, and with the operation "open" and the full path otherwise.
func CreateTemp(root *os.Root, pattern string, perm fs.FileMode) (*os.File, error) {
	if hasSeparator(pattern) {
		return nil, newPathError("createtemp", pattern, errPatternHasSep)
	}
	prefix, suffix := pattern, ""
	if before, after, ok := strings.CutLast(pattern, "*"); ok {
		prefix, suffix = before, after
	}
	for try := 0; ; try++ {
		name := prefix + randomSuffix() + suffix
		f, err := createFile(root, name, perm)
		if errors.Is(err, fs.ErrExist) {
			if try+1 < maxTempTries {
				continue
			}
			return nil, newPathError("createtemp", joinPath(root.Name(), prefix+"*"+suffix), fs.ErrExist)
		}
		if err != nil {
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				err = pe.Err
			}
			return nil, newPathError("open", joinPath(root.Name(), name), err)
		}
		return f, nil
	}
}

// Mkdir creates the directory name inside the directory root. The create
// fails when name exists, even as a link, and never leaves the directory.
// name is one path component, with the same refusals on Windows as CreateFile.
//
// On Windows the directory has a protected security descriptor with one
// entry that grants the current user full control, set by the same call that
// creates the directory. With inheritable the entry also passes to the files
// and directories created in it afterwards; without it the entry does not.
// The mode bits do nothing there. On every other platform the directory gets
// mode perm (before the umask), which is the restriction, and inheritable
// has no effect.
//
// The errors are *fs.PathError values with the operation "mkdir" and the path
// of root joined and cleaned with name, as os.Mkdir reports it; one for an
// existing name matches fs.ErrExist.
func Mkdir(root *os.Root, name string, perm fs.FileMode, inheritable bool) error {
	if !isOneComponent(name) {
		return newPathError("mkdir", filepath.Join(root.Name(), name), errNotOneComponent)
	}
	return mkdir(root, name, perm, inheritable)
}

// isOneComponent reports whether name is a plain file name: not empty, not
// "." or "..", and without a path separator.
func isOneComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !hasSeparator(name)
}

// hasSeparator reports whether s has a path separator of the platform.
func hasSeparator(s string) bool {
	for i := range len(s) {
		if os.IsPathSeparator(s[i]) {
			return true
		}
	}
	return false
}

// joinPath joins a directory name and a file name the way os.CreateTemp
// does, without cleaning the result.
func joinPath(dir, name string) string {
	if len(dir) > 0 && os.IsPathSeparator(dir[len(dir)-1]) {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// newPathError returns the *fs.PathError of an operation on path.
func newPathError(op, path string, err error) error {
	return &fs.PathError{Op: op, Path: path, Err: err}
}
