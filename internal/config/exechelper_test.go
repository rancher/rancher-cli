package config

import (
	"bufio"
	"encoding/json"
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
)

func buildFakeHelper(t *testing.T) string {
	t.Helper()
	fakeHelperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakehelper") //nolint:usetesting // shared across the whole test binary via sync.Once; t.TempDir() would bind its lifetime to the first caller's test
		if err != nil {
			fakeHelperErr = err
			return
		}
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
		if err := os.WriteFile(filepath.Join(dir, "rancher-credential-fake2"+ext), src, 0o755); err != nil {
			fakeHelperErr = err
			return
		}
		fakeHelperDir = dir
	})
	require.NoError(t, fakeHelperErr)
	return fakeHelperDir
}

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
// helper's lockhold mode prints it AFTER flock acquisition) — so the parent
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
