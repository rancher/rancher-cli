package conformance

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMain moves the process into the module root before any test runs:
// every check in this package reads the repository as a whole
// (ARCHITECTURE.md, .golangci.yml, the tracked file list, the internal/
// tree), and `go test` starts it in this package's own directory.
func TestMain(m *testing.M) {
	// A copy of this binary that the paste tests run as the program rancher
	// answers before anything reads the repository: it may run outside the
	// module tree.
	if os.Getenv(pasteHelperEnv) == "1" {
		pasteHelper()
	}
	root, err := moduleRoot()
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(root); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// moduleRoot climbs from the working directory to the directory holding
// go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above " + dir)
		}
		dir = parent
	}
}
