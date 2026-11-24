package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exitEnv gives the test an empty config directory, the file credential
// store, an empty home directory and no server, so a line fails or succeeds
// the same on every machine. Tests that use it are not parallel.
func exitEnv(t *testing.T) {
	t.Helper()
	restoreRawArgv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvReadOnly, "")
}

// plainResult returns r without the outcome of its run, for a comparison with
// the result of another run, whose outcome names commands of another tree.
func plainResult(r runResult) runResult {
	r.outcome = outcome{}
	return r
}

// runResultOf runs argv, the words after the program name, on a production
// root through run and returns the result and what reached stderr.
func runResultOf(t *testing.T, argv ...string) (runResult, string) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader("")
	return plainResult(run(t.Context(), root, append([]string{"rancher"}, argv...))), errOut.String()
}

// handBuiltRoot is a root with one Action, for the errors no production line
// returns.
func handBuiltRoot(t *testing.T, action func(context.Context, *Command, *Input) error) (*Command, *bytes.Buffer) {
	t.Helper()
	var errOut bytes.Buffer
	root := link(&Command{
		Name:       "rancher",
		descriptor: &runtime.Descriptor{Name: "rancher"},
		Action:     action,
		Writer:     &bytes.Buffer{},
		ErrWriter:  &errOut,
	})
	return root, &errOut
}

// TestResultOf asserts the exit code and the stderr text for each shape of
// error a run can end with: a message is rendered once as a GENERIC error in
// the requested output format with exit 1 (never a code the error carries,
// such as 3), an empty message writes nothing and keeps its exit
// code, a plain error exits 1, an error that names its *clierr.Error keeps
// that error's code and writes nothing, and a *clierr.Error returned
// unrendered is rendered with its own code. Not parallel: the renderer reads
// the captured argv.
func TestResultOf(t *testing.T) {
	restoreRawArgv(t)
	of := func(err error, argv ...string) (runResult, string) {
		t.Helper()
		root, buildErr := BuildRoot("v-test")
		require.NoError(t, buildErr)
		var errOut bytes.Buffer
		root.ErrWriter = &errOut
		CaptureArgv(argv)
		return resultOf(root, err), errOut.String()
	}

	got, stderr := of(codedError{"No help topic for 'x'", 3})
	assert.Equal(t, 1, got.code, "the exit code of the error must not reach the caller")
	require.NotNil(t, got.err)
	assert.Equal(t, clierr.CodeGeneric, got.err.Code)
	assert.Equal(t, "error (GENERIC): No help topic for 'x'\n", stderr)

	got, stderr = of(errors.New("boom"))
	assert.Equal(t, 1, got.code)
	require.NotNil(t, got.err)
	assert.Equal(t, clierr.CodeGeneric, got.err.Code)
	assert.Equal(t, "error (GENERIC): boom\n", stderr)

	got, stderr = of(statusExit(5))
	assert.Equal(t, runResult{code: 5}, got, "a status the command declares carries its code and no error")
	assert.Empty(t, stderr)

	got, stderr = of(codedError{"No help topic for 'x'", 3}, "-o", "json", "get", "pod")
	assert.Equal(t, 1, got.code)
	var env errEnvelope
	require.NoError(t, json.Unmarshal([]byte(stderr), &env))
	assert.Equal(t, "GENERIC", env.Data.Error.Code)
	assert.Equal(t, "No help topic for 'x'", env.Data.Error.Message)

	ce := clierr.New(clierr.CodeConflict, "already there")
	got, stderr = of(exit(ce))
	assert.Equal(t, runResult{code: 5, err: ce}, got)
	assert.Empty(t, stderr)

	got, stderr = of(fmt.Errorf("wrapped: %w", exit(ce)))
	assert.Equal(t, runResult{code: 5, err: ce}, got, "a wrapped exit error is found")
	assert.Empty(t, stderr)

	got, stderr = of(clierr.New(clierr.CodeAuth, "no access"))
	assert.Equal(t, 3, got.code, "a clierr error keeps its own exit code")
	require.NotNil(t, got.err)
	assert.Equal(t, clierr.CodeAuth, got.err.Code)
	assert.Equal(t, "error (AUTH): no access\n", stderr)
}

// codedError is an error with a message and an exit code.
type codedError struct {
	msg  string
	code int
}

func (e codedError) Error() string { return e.msg }

func (e codedError) ExitCode() int { return e.code }

// TestResolveExit asserts that the error a run returned decides its code,
// and that no error is a success.
func TestResolveExit(t *testing.T) {
	restoreRawArgv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	root.ErrWriter = &bytes.Buffer{}

	got := resolveExit(root, exit(clierr.New(clierr.CodeAuth, "no access")))
	assert.Equal(t, 3, got.code)
	assert.Equal(t, clierr.CodeAuth, got.err.Code)

	assert.Equal(t, runResult{}, resolveExit(root, nil))
}

// TestUnknownCommandEndsWithExit2AndNeverLeaks asserts, through the
// production root and run, that an unknown command ends the run with exit 2
// and its error, and that a valid line run on the same root right after it
// ends with exit 0. Not parallel: it sets the process environment.
func TestUnknownCommandEndsWithExit2AndNeverLeaks(t *testing.T) {
	exitEnv(t)
	for _, argv := range [][]string{
		{"nosuch"},
		{"get", "nosuch"},
		{"help", "get", "nosuch"},
		{"--help", "get", "nosuch"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			var out, errOut bytes.Buffer
			root.Writer, root.ErrWriter = &out, &errOut

			got := run(t.Context(), root, append([]string{"rancher"}, argv...))
			assert.Equal(t, 2, got.code)
			require.NotNil(t, got.err)
			assert.Equal(t, clierr.CodeValidation, got.err.Code)
			assert.Equal(t, 1, strings.Count(errOut.String(), "error (VALIDATION): unknown command"), "stderr: %s", errOut.String())

			errOut.Reset()
			got = plainResult(run(t.Context(), root, []string{"rancher", "--version"}))
			assert.Equal(t, runResult{}, got, "a valid line right after an unknown command succeeds")
			assert.Empty(t, errOut.String())
		})
	}
}

// TestEveryExitPathCarriesItsError asserts, through run, that each way a
// line can end carries the *clierr.Error whose exit code matches the
// result: an error from the dependencies of a command, a refusal before the
// command runs, an error from the command, an unknown command, a flag error,
// the refusal of a line before it runs, an empty --config, and a
// run with no arguments; that a status a command declares carries its code
// and no error; and, on a root built by hand, that a plain error and an
// error with an exit code and a message end as GENERIC with exit 1. Not
// parallel: it sets the process environment.
func TestEveryExitPathCarriesItsError(t *testing.T) {
	exitEnv(t)

	prev := openDirectory
	t.Cleanup(func() { openDirectory = prev })
	openDirectory = func(string, bool) (config.CredStore, *config.CacheAnchor, error) {
		return nil, nil, clierr.New(clierr.CodeCredentialStoreUnavailable, "the store cannot be opened")
	}
	got, stderr := runResultOf(t, "get", "cluster")
	assert.Equal(t, 14, got.code)
	require.NotNil(t, got.err)
	assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.err.Code, "an error from the dependencies of a command")
	assert.Contains(t, stderr, "CREDENTIAL_STORE_UNAVAILABLE")
	openDirectory = prev

	for _, tc := range []struct {
		name string
		argv []string
		code int
		want string
	}{
		{"a refusal before the command runs", []string{"-o", "bogus", "config", "path"}, 2, clierr.CodeValidation},
		{"an error from the command", []string{"get", "cluster"}, 2, clierr.CodeNotLoggedIn},
		{"an unknown command", []string{"nosuch"}, 2, clierr.CodeValidation},
		{"a flag error on a leaf", []string{"get", "pod", "--bogus"}, 2, clierr.CodeValidation},
		{"a flag error on the root", []string{"--bogus"}, 2, clierr.CodeValidation},
		{"the refusal of a line before it runs", []string{"get", "pod", "-", "x"}, 2, clierr.CodeValidation},
		{"an empty --config", []string{"--config", "", "get", "pod", "--cluster", "local"}, 2, clierr.CodeValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, stderr := runResultOf(t, tc.argv...)
			assert.Equal(t, tc.code, got.code, "stderr: %s", stderr)
			require.NotNil(t, got.err, "stderr: %s", stderr)
			assert.Equal(t, tc.want, got.err.Code)
			assert.Equal(t, got.err.ExitCode(), got.code)
			assert.Equal(t, 1, strings.Count(stderr, "error ("+tc.want+")"), "the error is rendered once: %s", stderr)
		})
	}

	t.Run("a run with no arguments", func(t *testing.T) {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer, root.ErrWriter = &out, &bytes.Buffer{}
		assert.Equal(t, runResult{}, plainResult(run(t.Context(), root, nil)))
		assert.NotEmpty(t, out.String())
	})

	t.Run("a status the command declares", func(t *testing.T) {
		got, stderr := runResultOf(t, "skill", "status", "--agent", "claude", "--require-installed")
		assert.Equal(t, runResult{code: 7}, got)
		assert.Empty(t, stderr)
	})

	t.Run("a plain error", func(t *testing.T) {
		root, errOut := handBuiltRoot(t, func(context.Context, *Command, *Input) error { return errors.New("the command says no") })
		got := run(t.Context(), root, []string{"rancher"})
		assert.Equal(t, 1, got.code)
		require.NotNil(t, got.err)
		assert.Equal(t, clierr.CodeGeneric, got.err.Code)
		assert.Equal(t, "error (GENERIC): the command says no\n", errOut.String())
	})

	t.Run("an error with a message and an exit code", func(t *testing.T) {
		root, errOut := handBuiltRoot(t, func(context.Context, *Command, *Input) error { return codedError{"No help topic for 'x'", 3} })
		got := run(t.Context(), root, []string{"rancher"})
		assert.Equal(t, 1, got.code)
		require.NotNil(t, got.err)
		assert.Equal(t, clierr.CodeGeneric, got.err.Code)
		assert.Equal(t, "error (GENERIC): No help topic for 'x'\n", errOut.String())
	})
}

// TestARawClierrReturnedFromACommandIsRenderedOnce asserts that a
// *clierr.Error an Action returns without rendering it is rendered once, in
// its own code and with its own exit code, not as a GENERIC error with exit
// 1. One another error wraps is not: it is rendered as a GENERIC error
// with the whole text, exit 1, as before. Not parallel: the renderer reads
// the captured argv.
func TestARawClierrReturnedFromACommandIsRenderedOnce(t *testing.T) {
	restoreRawArgv(t)
	run1 := func(t *testing.T, returned error) (runResult, string) {
		t.Helper()
		root, errOut := handBuiltRoot(t, func(context.Context, *Command, *Input) error { return returned })
		return run(t.Context(), root, []string{"rancher"}), errOut.String()
	}
	t.Run("direct", func(t *testing.T) {
		got, stderr := run1(t, clierr.New(clierr.CodeAuth, "no access"))
		assert.Equal(t, 3, got.code)
		require.NotNil(t, got.err)
		assert.Equal(t, clierr.CodeAuth, got.err.Code)
		assert.Equal(t, 1, strings.Count(stderr, "error (AUTH): no access"), "stderr: %s", stderr)
		assert.NotContains(t, stderr, "GENERIC")
	})
	t.Run("wrapped", func(t *testing.T) {
		got, stderr := run1(t, fmt.Errorf("while reading: %w", clierr.New(clierr.CodeAuth, "no access")))
		assert.Equal(t, 1, got.code)
		require.NotNil(t, got.err)
		assert.Equal(t, clierr.CodeGeneric, got.err.Code)
		assert.Equal(t, "error (GENERIC): while reading: no access\n", stderr)
	})
}

// TestExitCodeOfKeepsStderr asserts the helper that tests use after
// root.Run: on one root, an unknown command gives 2 and a valid line right
// after it gives 0; and an error that nothing rendered is counted without
// writing to the root's error writer. Not parallel: the renderer reads the
// captured command line.
func TestExitCodeOfKeepsStderr(t *testing.T) {
	restoreRawArgv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut

	CaptureArgv([]string{"nosuch"})
	runErr := root.Run(t.Context(), []string{"rancher", "nosuch"})
	assert.Equal(t, 2, exitCodeOf(root, runErr))

	CaptureArgv([]string{"--version"})
	errOut.Reset()
	runErr = root.Run(t.Context(), []string{"rancher", "--version"})
	assert.Equal(t, 0, exitCodeOf(root, runErr), "the earlier line leaves nothing behind")

	assert.Equal(t, 1, exitCodeOf(root, errors.New("boom")))
	assert.Empty(t, errOut.String(), "the helper writes nothing to the error writer")
}
