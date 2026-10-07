package cli_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"
)

// outdatedHint is the line an outdated completion script gets.
const outdatedHint = "The rancher completion script in this shell is out of date. To regenerate it, see: rancher completion --help\n"

// runLineAsMain runs words, the command line after the program name, the way
// main does: OutdatedCompletionScript first and, only when it reports false,
// CaptureArgv and Run on the argv SanitizedArgs hands urfave. It returns what
// reached stdout, whether the line stopped before the command tree, and what
// Run returned.
func runLineAsMain(t *testing.T, root *ucli.Command, words []string) (string, bool, error) {
	t.Helper()
	var out bytes.Buffer
	if cli.OutdatedCompletionScript(words, &out) {
		return out.String(), true, nil
	}
	cli.CaptureArgvForTest(words)
	root.Writer = &out
	root.ErrWriter = io.Discard
	err := root.Run(t.Context(), cli.SanitizedArgsForTest(append([]string{"rancher"}, words...)))
	return out.String(), false, err
}

// outdatedLines are the lines an outdated completion script could send for
// the command at path (empty for the root): each ends with
// --generate-shell-completion and none opens with the protocol marker. They
// cover the flag alone after the path, a partial word, a "--" (after which
// urfave runs the command it reaches), a bare "-", global flags before the
// command name, a flag after it, the current-word field, and the marker
// somewhere other than first. At the root that last line opens with the
// marker, so it is left out.
func outdatedLines(path []string) [][]string {
	lines := [][]string{
		path,
		append(slices.Clone(path), "x"),
		append(slices.Clone(path), "--"),
		append(slices.Clone(path), "-"),
		append(slices.Clone(path), "--apply", "--yes", "--", "x"),
		append([]string{"-o", "json"}, path...),
		append([]string{"--cluster", "c-1"}, path...),
		append(slices.Clone(path), "--rancher-completion-cur=x"),
	}
	if len(path) > 0 {
		lines = append(lines, append(slices.Clone(path), "--rancher-completion-proto=2"))
	}
	for i, words := range lines {
		lines[i] = append(slices.Clone(words), "--generate-shell-completion")
	}
	return lines
}

// groupPaths returns the command path of the root (empty) and of every
// command in root that has subcommands.
func groupPaths(root *ucli.Command) [][]string {
	paths := [][]string{nil}
	var walk func(c *ucli.Command, path []string)
	walk = func(c *ucli.Command, path []string) {
		if len(c.Commands) == 0 {
			return
		}
		paths = append(paths, path)
		for _, sub := range c.Commands {
			walk(sub, append(slices.Clone(path), sub.Name))
		}
	}
	for _, c := range root.Commands {
		walk(c, []string{c.Name})
	}
	return paths
}

// TestOutdatedCompletionScriptRunsNothing asserts, for the root, every group
// and every leaf of the built command tree, that a line ending with
// --generate-shell-completion without the protocol marker never runs a
// command: it stops before the command tree with exactly the one hint line,
// no leaf's action is reached, no request is sent to the server the
// environment names, the keychain is not touched, and no file appears in the
// config directory or the working directory. The tree is built once.
// NOT parallel: mutates the argv globals, env and the working directory.
func TestOutdatedCompletionScriptRunsNothing(t *testing.T) {
	var keyringOps atomic.Int32
	config.SetKeyringObserverForTest(func(string) { keyringOps.Add(1) })
	t.Cleanup(func() {
		config.SetKeyringObserverForTest(nil)
		keyring.MockInit()
	})
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	cfgDir := t.TempDir()
	t.Setenv(config.EnvConfigDir, cfgDir)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvToken, "token-abc:secret")
	workDir := t.TempDir()
	t.Chdir(workDir)

	root := buildRoot(t)
	var ran []string
	paths := append(groupPaths(root), everyLeaf(t, root, &ran)...)
	require.Greater(t, len(paths), 2, "the root, groups and leaves are swept")
	keyringOps.Store(0)

	for _, path := range paths {
		for _, words := range outdatedLines(path) {
			desc := strings.Join(words, " ")
			out, stopped, runErr := runLineAsMain(t, root, words)
			require.NoError(t, runErr, desc)
			assert.True(t, stopped, "the line reached the command tree: %s", desc)
			assert.Equal(t, outdatedHint, out, desc)
			assert.Empty(t, ran, "the command ran: %s", desc)
			ran = nil
		}
	}
	assert.Zero(t, requests.Load(), "a request reached the server")
	assert.Zero(t, keyringOps.Load(), "the keychain was touched")
	assert.Empty(t, listDir(t, cfgDir), "a file appeared in the config directory")
	assert.Empty(t, listDir(t, workDir), "a file appeared in the working directory")
}

// TestOutdatedCompletionScriptTextIgnoresTheShell asserts that the hint is
// the same line whatever SHELL holds: the request line does not tell which
// shell sent it, and SHELL names the login shell. NOT parallel: sets SHELL.
func TestOutdatedCompletionScriptTextIgnoresTheShell(t *testing.T) {
	for _, shell := range []string{"/bin/zsh", "/usr/local/bin/fish", "pwsh.exe", "/bin/tcsh", ""} {
		t.Run(shell, func(t *testing.T) {
			t.Setenv("SHELL", shell)
			var out bytes.Buffer
			require.True(t, cli.OutdatedCompletionScript([]string{"get", "--generate-shell-completion"}, &out))
			assert.Equal(t, outdatedHint, out.String())
		})
	}
}

// TestOutdatedCompletionScriptLeavesOtherLines asserts that a line from a
// current script, an ordinary command line, and a line holding the flag
// anywhere but last are not taken for an outdated script, and that nothing
// is written for them.
func TestOutdatedCompletionScriptLeavesOtherLines(t *testing.T) {
	t.Parallel()
	for _, words := range [][]string{
		nil,
		{"--rancher-completion-proto=2", "get", "--rancher-completion-cur=", "--generate-shell-completion"},
		{"--rancher-completion-proto=2", "get", "--", "x", "--rancher-completion-cur=", "--generate-shell-completion"},
		{"get", "pod"},
		{"get", "pod", "--cluster", "--generate-shell-completion", "--namespace", "x"},
	} {
		var out bytes.Buffer
		assert.False(t, cli.OutdatedCompletionScript(words, &out), strings.Join(words, " "))
		assert.Empty(t, out.String())
	}
}
