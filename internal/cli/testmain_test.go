package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
)

// withoutUpdatedGoldens returns the new paths without the ones under
// testdata/help and testdata/parse when updating is set: a run with -update
// writes those files. Without -update every new path is kept.
func withoutUpdatedGoldens(paths []string, updating bool) []string {
	if !updating {
		return paths
	}
	return slices.DeleteFunc(slices.Clone(paths), func(p string) bool {
		p = filepath.ToSlash(p)
		for _, dir := range []string{"testdata/help", "testdata/parse"} {
			if p == dir || strings.HasPrefix(p, dir+"/") {
				return true
			}
		}
		return false
	})
}

// TestWithoutUpdatedGoldens asserts that the check for files the tests leave
// behind ignores the paths under testdata/help and testdata/parse in a run with
// -update only: without it every path is kept, and a path that only starts
// like one of the directories is kept in both.
func TestWithoutUpdatedGoldens(t *testing.T) {
	t.Parallel()
	golden := []string{
		filepath.Join("testdata", "help"),
		filepath.Join("testdata", "help", "get-pod.txt"),
		filepath.Join("testdata", "parse"),
		filepath.Join("testdata", "parse", "get-pod.jsonl"),
	}
	others := []string{
		"stray.txt",
		filepath.Join("testdata", "stray.txt"),
		filepath.Join("testdata", "helpful.txt"),
		filepath.Join("testdata", "parsed", "get-pod.jsonl"),
	}
	all := slices.Concat(golden, others)
	assert.Equal(t, all, withoutUpdatedGoldens(all, false), "without -update every new path is reported")
	assert.Equal(t, others, withoutUpdatedGoldens(all, true), "with -update only the paths outside the golden directories are reported")
	assert.Empty(t, withoutUpdatedGoldens(nil, true))
}

// TestMain clears the RANCHER_CLI_* variables this package's tests exercise
// as overrides, so a normal interactive shell with them set cannot silently
// change what a test observes, and points the config directory at a
// temporary one, so a test that forgets its own cannot reach the
// operator's real one. A test that leaves a new file in the package
// directory fails the package, and so does one that sends a request through
// http.DefaultTransport: closing any httptest server closes that transport's
// idle connections; a test uses its server's own client instead. It also
// shortens the interval of the cluster commands' waits once, before any test
// runs; the wait tests then run in parallel. A run with -update does not count
// the files it writes under testdata/help and testdata/parse.
func TestMain(m *testing.M) {
	provisioning.SetClusterWaitIntervalForTest(time.Millisecond)
	cmdtest.UnsetResolutionEnv()
	cleanup := cmdtest.IsolateConfigDir()
	since := cmdtest.NewFiles()
	restore := testtransport.GuardDefaultTransport()
	code := m.Run()
	code = testrancher.FailOnWritesWithoutFieldManager(code, os.Stderr)
	if err := restore(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	code = cmdtest.FailOnNewFiles(code, func() ([]string, error) {
		added, err := since()
		return withoutUpdatedGoldens(added, *update), err
	}, os.Stderr)
	cleanup()
	os.Exit(code)
}
