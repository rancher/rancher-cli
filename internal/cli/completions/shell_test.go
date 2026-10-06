package completions

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireShell returns the path of the first of names found on PATH and logs
// the name it found, so a verbose run shows which shell each test used. A
// missing shell skips the test locally; when the CI environment variable is
// set it fails the test, because a skipped shell test proves nothing about
// that shell. Every shell lookup, and every skip, in this package goes
// through this file.
func requireShell(t *testing.T, names ...string) string {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("requireShell needs at least one shell name")
	}
	name, path, problem := findShell(exec.LookPath, names...)
	if problem != "" {
		shellMissing(t, "%s", problem)
	}
	t.Logf("shell %s: %s", name, path)
	return path
}

// shellMissing ends the test for a precondition the shell must meet: it
// skips locally and fails when the CI environment variable is set.
func shellMissing(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf("CI is set, so a missing shell fails instead of skipping: "+format, args...)
	}
	t.Skipf(format, args...)
}

// findShell looks the names up in order and returns the first one found with
// its path. With none found it returns the problem text, which names every
// shell looked for.
func findShell(lookPath func(string) (string, error), names ...string) (name, path, problem string) {
	for _, n := range names {
		if p, err := lookPath(n); err == nil {
			return n, p, ""
		}
	}
	return "", "", strings.Join(names, " or ") + " not found on PATH"
}

// TestFindShellNamesTheShellFound asserts the lookup returns the first shell
// found, with its name and path, and, when none is found, a problem naming
// every shell it looked for.
func TestFindShellNamesTheShellFound(t *testing.T) {
	t.Parallel()
	onPath := map[string]string{"powershell": "/usr/bin/powershell"}
	lookPath := func(name string) (string, error) {
		if p, ok := onPath[name]; ok {
			return p, nil
		}
		return "", exec.ErrNotFound
	}

	name, path, problem := findShell(lookPath, "pwsh", "powershell")
	assert.Equal(t, "powershell", name)
	assert.Equal(t, "/usr/bin/powershell", path)
	assert.Empty(t, problem)

	name, path, problem = findShell(lookPath, "fish")
	assert.Empty(t, name)
	assert.Empty(t, path)
	assert.Equal(t, "fish not found on PATH", problem)

	_, _, problem = findShell(lookPath, "pwsh", "bash")
	assert.Equal(t, "pwsh or bash not found on PATH", problem)
}

// shellHelperChild names the environment variable that makes
// TestRequireShellChild call requireShell; its value picks the call.
const shellHelperChild = "RANCHER_COMPLETIONS_SHELL_HELPER_CHILD"

// TestRequireShellChild is the subject of TestRequireShellPolicy. It does
// nothing unless that test runs it in a child process.
func TestRequireShellChild(t *testing.T) {
	switch os.Getenv(shellHelperChild) {
	case "missing":
		requireShell(t, "no-such-shell-for-the-test")
	case "fallback":
		requireShell(t, "no-such-shell-for-the-test", "go")
	case "none":
		requireShell(t)
	}
}

// TestRequireShellPolicy asserts the helper's behavior in a child process:
// a missing shell fails the test, naming it, when CI is set and skips it
// when CI is not; the log names the shell that was found, not the first one
// asked for; and a call with no shell name fails with a message, with or
// without CI, instead of panicking.
func TestRequireShellPolicy(t *testing.T) {
	t.Parallel()
	run := func(call, ci string) (string, error) {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRequireShellChild$", "-test.v")
		cmd.Env = append(os.Environ(), shellHelperChild+"="+call, "CI="+ci)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("missing", "1")
	require.Error(t, err)
	assert.Contains(t, out, "--- FAIL: TestRequireShellChild")
	assert.Contains(t, out, "CI is set")
	assert.Contains(t, out, "no-such-shell-for-the-test not found on PATH")

	out, err = run("missing", "")
	require.NoError(t, err)
	assert.Contains(t, out, "--- SKIP: TestRequireShellChild")
	assert.Contains(t, out, "no-such-shell-for-the-test not found on PATH")

	out, err = run("fallback", "1")
	require.NoError(t, err)
	assert.Contains(t, out, "shell go: ")
	assert.NotContains(t, out, "shell no-such-shell-for-the-test")

	for _, ci := range []string{"1", ""} {
		out, err = run("none", ci)
		require.Error(t, err)
		assert.Contains(t, out, "--- FAIL: TestRequireShellChild")
		assert.Contains(t, out, "requireShell needs at least one shell name")
		assert.NotContains(t, out, "panic")
	}
}
