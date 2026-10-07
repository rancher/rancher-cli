package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mainChildEnv set to "1" makes the test binary run main instead of the
// tests (see TestMain).
const mainChildEnv = "RANCHER_MAIN_TEST_CHILD"

// TestMain runs main itself when runMainProcess starts the test binary as a
// child, so a test sees main as a process does: its exit code, stdout and
// stderr. The child's arguments are the command line main reads.
func TestMain(m *testing.M) {
	if os.Getenv(mainChildEnv) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMainProcess runs main in a child process for words, the command line
// after the program name, with env as its whole environment and dir as its
// working directory, and returns its stdout, stderr and exit code.
func runMainProcess(t *testing.T, env []string, dir string, words ...string) (string, string, int) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), exe, words...)
	cmd.Env = append(slices.Clone(env), mainChildEnv+"=1")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := 0
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		code = exit.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return stdout.String(), stderr.String(), code
}

// TestOutdatedCompletionScriptStopsTheProcess asserts, for main run as a
// process, that a line ending with --generate-shell-completion without the
// protocol marker - at the root, after a bare "-", and after a "--" on a
// mutating leaf with global flags first - exits 0 with the one hint line on
// stderr and nothing on stdout, sends no request to the server the
// environment names, and changes no file, with a corrupt config.yaml in the
// config directory. Every group and leaf is swept in process by
// TestOutdatedCompletionScriptRunsNothing in internal/cli.
func TestOutdatedCompletionScriptStopsTheProcess(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cfgDir := t.TempDir()
	corrupt := []byte("not: [valid yaml\n")
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), corrupt, 0o600))
	workDir := t.TempDir()
	env := []string{
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		// A test binary built with -cover warns on stderr at exit
		// when this is unset.
		"GOCOVERDIR=" + t.TempDir(),
		config.EnvConfigDir + "=" + cfgDir,
		config.EnvCredentialStore + "=file",
		config.EnvURL + "=" + srv.URL,
		config.EnvToken + "=token-abc:secret",
	}

	t.Run("lines", func(t *testing.T) {
		for _, words := range [][]string{
			{"--generate-shell-completion"},
			{"get", "pod", "-", "--generate-shell-completion"},
			{"--cluster", "c-1", "-o", "json", "config", "add-server", "--url", "https://x.invalid", "--apply", "--yes", "--", "x", "--generate-shell-completion"},
		} {
			t.Run(strings.Join(words, " "), func(t *testing.T) {
				t.Parallel()
				stdout, stderr, code := runMainProcess(t, env, workDir, words...)
				assert.Equal(t, 0, code)
				assert.Empty(t, stdout)
				assert.Equal(t, "The rancher completion script in this shell is out of date. To regenerate it, see: rancher completion --help\n", stderr)
			})
		}
	})
	assert.Zero(t, requests.Load(), "a request reached the server")
	entries, err := os.ReadDir(cfgDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "a file appeared in the config directory")
	got, err := os.ReadFile(filepath.Join(cfgDir, "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, corrupt, got, "config.yaml changed")
	entries, err = os.ReadDir(workDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a file appeared in the working directory")
}

// TestMainChecksForAnOutdatedScriptFirst asserts the order of main's
// statements: before the OutdatedCompletionScript check only the skill
// source is set, and the check returns when it reports true. Reading the
// line (CaptureArgv), building the command tree, loading config or touching
// credentials must all come after it, so an outdated script's line runs
// none of them.
func TestMainChecksForAnOutdatedScriptFirst(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	require.NoError(t, err)
	var body []ast.Stmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "main" {
			body = fn.Body.List
		}
	}
	require.NotEmpty(t, body, "main has a body")

	check := -1
	for i, stmt := range body {
		if ifStmt, ok := stmt.(*ast.IfStmt); ok && calls(ifStmt.Cond, "cli", "OutdatedCompletionScript") {
			check = i
			require.Len(t, ifStmt.Body.List, 1, "the check's body is a lone return")
			_, isReturn := ifStmt.Body.List[0].(*ast.ReturnStmt)
			assert.True(t, isReturn, "the check's body is a lone return")
			break
		}
	}
	require.GreaterOrEqual(t, check, 0, "main calls cli.OutdatedCompletionScript in an if")
	for _, stmt := range body[:check] {
		expr, ok := stmt.(*ast.ExprStmt)
		assert.True(t, ok && calls(expr.X, "skillinstall", "SetSource"), "only skillinstall.SetSource may run before the outdated-script check")
	}
}

// calls reports whether expr is a call of pkg.name.
func calls(expr ast.Expr, pkg, name string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}
