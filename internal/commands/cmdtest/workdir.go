package cmdtest

import (
	"io"
	"io/fs"
	"path/filepath"
	"slices"
)

// NewFiles records every path under the working directory - the package
// directory, where go test runs a package's tests - and returns a func that
// lists the paths that appeared since. A test that runs a command which
// writes a file where it runs must give it a temporary working directory;
// a TestMain that calls this fails the package when one does not. A
// directory that cannot be listed is an error, never an empty listing.
func NewFiles() (since func() ([]string, error)) {
	before, beforeErr := walk()
	return func() ([]string, error) {
		if beforeErr != nil {
			return nil, beforeErr
		}
		after, err := walk()
		if err != nil {
			return nil, err
		}
		var added []string
		for p := range after {
			if !before[p] {
				added = append(added, p)
			}
		}
		slices.Sort(added)
		return added, nil
	}
}

func walk() (map[string]bool, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		seen[p] = true
		return nil
	})
	return seen, err
}

// FailOnNewFiles is the exit code for a TestMain whose tests ran with code:
// code itself, or 1 when since reports a new path or cannot list the
// directory, which it writes to w.
func FailOnNewFiles(code int, since func() ([]string, error), w io.Writer) int {
	added, err := since()
	if err != nil {
		_, _ = io.WriteString(w, "tests could not list the package directory to check for files they left: "+err.Error()+"\n")
		return 1
	}
	if len(added) == 0 {
		return code
	}
	_, _ = io.WriteString(w, "tests left files in the package directory; run the command from a temporary working directory:\n")
	for _, p := range added {
		_, _ = io.WriteString(w, "  "+p+"\n")
	}
	return 1
}
