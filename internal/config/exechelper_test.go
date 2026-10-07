package config

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/gofrs/flock" // keep go.mod aware of flock; testdata/fakehelper imports it
	"github.com/stretchr/testify/require"
)

// buildFakeHelper compiles testdata/fakehelper once per test binary into a
// shared temp dir as rancher-credential-fake AND rancher-credential-fake2
// (a byte-identical copy; the binary selects its env-var prefix from its
// own basename). Returns the directory for PATH injection.
var (
	fakeHelperOnce sync.Once
	fakeHelperDir  string
	fakeHelperErr  error
	// fakeHelperRoot is the directory buildFakeHelper created, kept even when
	// the build fails so TestMain removes it.
	fakeHelperRoot string
)

func buildFakeHelper(t *testing.T) string {
	t.Helper()
	fakeHelperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakehelper") //nolint:usetesting // shared across the whole test binary via sync.Once; t.TempDir() would bind its lifetime to the first caller's test
		if err != nil {
			fakeHelperErr = err
			return
		}
		fakeHelperRoot = dir
		ext := ""
		if runtime.GOOS == "windows" {
			ext = ".exe"
		}
		bin := filepath.Join(dir, "rancher-credential-fake"+ext)
		if out, err := exec.Command("go", "build", "-o", bin, "./testdata/fakehelper").CombinedOutput(); err != nil {
			fakeHelperErr = &buildError{out: string(out), err: err}
			return
		}
		src, err := os.ReadFile(bin)
		if err != nil {
			fakeHelperErr = err
			return
		}
		bin2 := filepath.Join(dir, "rancher-credential-fake2"+ext)
		if err := os.WriteFile(bin2, src, 0o755); err != nil {
			fakeHelperErr = err
			return
		}
		if err := firstRun(t.Context(), bin, bin2); err != nil {
			fakeHelperErr = err
			return
		}
		fakeHelperDir = dir
	})
	require.NoError(t, fakeHelperErr)
	return fakeHelperDir
}

// firstRun runs each helper binary once with the noop verb and a time limit
// of firstRunLimit. macOS checks every newly written executable on its first
// run, one at a time across the whole system: while many new binaries start
// at once (a full test run) that first run can take many seconds, and a
// test's helper call would spend it inside execHelperTimeout. The limit is
// far above the longest first run measured (8 s under load) and only turns a
// hung helper into a clear failure.
func firstRun(ctx context.Context, bins ...string) error {
	ctx, cancel := context.WithTimeout(ctx, firstRunLimit)
	defer cancel()
	for _, bin := range bins {
		out, err := exec.CommandContext(ctx, bin, "noop").CombinedOutput()
		if ctx.Err() != nil {
			return fmt.Errorf("first run of %s did not finish within %s", bin, firstRunLimit)
		}
		if err != nil {
			return fmt.Errorf("first run of %s: %w: %s", bin, err, out)
		}
	}
	return nil
}

const firstRunLimit = 5 * time.Minute

type buildError struct {
	out string
	err error
}

func (b *buildError) Error() string { return "building fake helper: " + b.err.Error() + "\n" + b.out }

// fakeHelperBin returns the compiled fake helper's full path.
func fakeHelperBin(t *testing.T) string {
	t.Helper()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return filepath.Join(buildFakeHelper(t), "rancher-credential-fake"+ext)
}

// useFakeHelper is every exec-store test's setup line: fake helper on PATH,
// mode + fresh state file wired, state path returned for assertions.
func useFakeHelper(t *testing.T, mode string) (statePath string) {
	t.Helper()
	t.Setenv("PATH", buildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", mode)
	statePath = filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	return statePath
}

// execRef is the canonical test identity.
func execRef() ServerRef {
	return ServerRef{Name: "prod", URL: "https://rancher.example.com"}
}

// fakeHelperCreds decodes the fake helper's state file credential map.
func fakeHelperCreds(t *testing.T, statePath string) map[string]string {
	t.Helper()
	creds, _ := fakeHelperState(t, statePath)
	return creds
}

// fakeHelperLog returns the fake helper's invocation log ("verb url name" lines).
func fakeHelperLog(t *testing.T, statePath string) []string {
	t.Helper()
	_, log := fakeHelperState(t, statePath)
	return log
}

func fakeHelperState(t *testing.T, statePath string) (map[string]string, []string) {
	t.Helper()
	b, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	require.NoError(t, err)
	var s struct {
		Creds map[string]string `json:"creds"`
		Log   []string          `json:"log"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	return s.Creds, s.Log
}

// waitForLocked starts cmd and blocks until it prints "locked" (the fake
// helper's lockhold mode prints it AFTER flock acquisition) - so the parent
// can never race the child to the lock. The read runs in a goroutine and is
// raced against a 5s timer: a blocking pipe read cannot observe a deadline
// by itself, and a child that never writes would otherwise hang the test.
// The Cleanup kill unblocks the reader goroutine on every exit path.
func waitForLocked(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		if sc.Scan() {
			got <- sc.Text()
			return
		}
		got <- "" // stdout closed without a line (child exited/failed)
	}()
	select {
	case line := <-got:
		require.Equal(t, "locked", line, "child must report lock acquisition")
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child never reported lock acquisition within 5s")
	}
}
