package cli_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"
)

// completionLine is one request a shell script sends: the words typed before
// the cursor and the word being completed.
type completionLine struct {
	name    string
	typed   []string
	current string
	// bare is a line that opens with the protocol marker and carries
	// neither the current-word field nor the trailing flag.
	bare bool
}

// argv is the command line the binary receives for the line: what the shipped
// scripts build.
func (l completionLine) argv() []string {
	if l.bare {
		return append([]string{"--rancher-completion-proto=2"}, l.typed...)
	}
	args := append([]string{"--rancher-completion-proto=2"}, l.typed...)
	return append(args, "--rancher-completion-cur="+l.current, "--generate-shell-completion")
}

// onceFlagLines are, for a leaf with a flag that may be given once, that
// flag typed with a value: once before a "--" and once after it (where it is
// an argument), and twice before it (a line the command refuses).
var onceFlagLines = map[string][2]string{
	"create secret":  {"--from-literal", "a=1"},
	"create project": {"--container-default-resource-limit", "limitsCpu=1"},
}

// doubleDashLines are the lines typed after a "--" on the leaf at path, with
// the word being completed empty, partial and flag-like, and a line that opens
// with the protocol marker and does not end with the flag. A leaf in
// onceFlagLines also gets its flag that may be given once before and after the
// "--".
func doubleDashLines(path []string) []completionLine {
	var lines []completionLine
	typedLines := [][]string{
		append(slices.Clone(path), "--"),
		append(slices.Clone(path), "--", "x"),
		append(slices.Clone(path), "--", "x", "y"),
		append(slices.Clone(path), "--apply", "--yes", "--"),
		append(slices.Clone(path), "--apply", "--yes", "--", "x"),
	}
	if once, ok := onceFlagLines[strings.Join(path, " ")]; ok {
		typedLines = append(typedLines,
			append(slices.Clone(path), once[0], once[1], "--"),
			append(slices.Clone(path), once[0], once[1], "--", once[0], once[1]),
			append(slices.Clone(path), once[0], once[1], once[0], once[1], "--"),
		)
	}
	for _, typed := range typedLines {
		for _, current := range []string{"", "x", "--a"} {
			lines = append(lines, completionLine{name: "request", typed: typed, current: current})
		}
	}
	return append(lines, completionLine{name: "bare", typed: append(slices.Clone(path), "--apply", "--yes", "x"), bare: true})
}

// leafPaths returns the command path of every command in root with no
// subcommands, and installs on each a recorder that notes when it runs.
func leafPaths(root *ucli.Command, ran *[]string) [][]string {
	var paths [][]string
	var walk func(c *ucli.Command, path []string)
	walk = func(c *ucli.Command, path []string) {
		if len(c.Commands) == 0 {
			paths = append(paths, path)
			name := strings.Join(path, " ")
			c.Action = func(context.Context, *ucli.Command) error {
				*ran = append(*ran, name)
				return nil
			}
			return
		}
		for _, sub := range c.Commands {
			walk(sub, append(slices.Clone(path), sub.Name))
		}
	}
	for _, c := range root.Commands {
		walk(c, []string{c.Name})
	}
	return paths
}

// everyLeaf returns the command path of every leaf of root, with a recorder
// installed on each (see leafPaths), and asserts they are exactly the leaves
// the schema manifest lists plus the hidden __completion command urfave adds,
// so a command added to the surface is covered.
func everyLeaf(t *testing.T, root *ucli.Command, ran *[]string) [][]string {
	t.Helper()
	// urfave adds its hidden __completion command to the tree on the first
	// run, so run a line once before the leaves get their recorders.
	_, err := runCompletionLine(t, root, completionLine{typed: []string{"completion"}})
	require.NoError(t, err)
	paths := leafPaths(root, ran)

	m, err := cli.BuildManifestForTest("test")
	require.NoError(t, err)
	var surface, leaves []string
	for _, c := range m.Commands {
		surface = append(surface, c.Name)
	}
	for _, p := range paths {
		leaves = append(leaves, strings.Join(p, " "))
	}
	surface = append(surface, "__completion")
	slices.Sort(surface)
	slices.Sort(leaves)
	require.Equal(t, surface, leaves, "the sweep must cover exactly the leaves the schema manifest lists, and urfave's hidden command")
	return paths
}

// listDir returns the paths under dir, relative to it.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			rel, relErr := filepath.Rel(dir, p)
			require.NoError(t, relErr)
			paths = append(paths, rel)
		}
		return nil
	}))
	return paths
}

// buildRoot builds the command tree. A command that runs ends with an error
// the assertions print.
func buildRoot(t *testing.T) *ucli.Command {
	t.Helper()
	root, err := cli.BuildRoot("test")
	require.NoError(t, err)
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	return root
}

// runCompletionLine runs one completion request through the same two calls
// main makes: CaptureArgv, then Run on the argv SanitizedArgs hands urfave. It
// returns what the request wrote to stdout and what Run returned.
func runCompletionLine(t *testing.T, root *ucli.Command, l completionLine) (string, error) {
	t.Helper()
	argv := l.argv()
	cli.CaptureArgvForTest(argv)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = io.Discard
	err := root.Run(t.Context(), cli.SanitizedArgsForTest(append([]string{"rancher"}, argv...)))
	return out.String(), err
}

// TestCompletionAfterDoubleDashRunsNothing asserts, for every leaf of the
// built command tree, that a completion request typed after a "--" never
// runs the command: the leaf's action is not reached, no request is sent to
// the server the environment names, no file appears in the config directory
// or the working directory, and no candidate is a flag, because every word
// after a "--" is an argument. The word being completed is empty, partial and
// flag-like. The leaves are checked against the schema manifest so a command
// added to the surface is covered; the hidden __completion command urfave adds
// is covered too. The tree is built once. NOT parallel: mutates the argv
// globals, env and the working directory.
func TestCompletionAfterDoubleDashRunsNothing(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	t.Setenv(config.EnvCredentialStore, "file")
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
	paths := everyLeaf(t, root, &ran)
	var leaves []string
	for _, p := range paths {
		leaves = append(leaves, strings.Join(p, " "))
	}
	for name := range onceFlagLines {
		require.Contains(t, leaves, name, "onceFlagLines names a leaf of the tree")
	}

	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			for _, l := range doubleDashLines(path) {
				out, runErr := runCompletionLine(t, root, l)
				desc := l.name + " " + strings.Join(l.argv(), " ")
				require.NoError(t, runErr, desc)
				assert.Empty(t, ran, "the command ran: %s", desc)
				ran = nil
				for _, candidate := range extractTokens(out) {
					assert.False(t, strings.HasPrefix(candidate, "-"), "offered the flag %q after --: %s", candidate, desc)
				}
			}
			assert.Zero(t, requests.Load(), "a request reached the server")
			assert.Empty(t, listDir(t, cfgDir), "a file appeared in the config directory")
			assert.Empty(t, listDir(t, workDir), "a file appeared in the working directory")
		})
	}
}

// TestCompletionAfterDoubleDashOffersTheArgumentSlot asserts that a leaf
// offers, after a "--", what it accepts at that argument slot: words typed
// after the "--" count as arguments, a flag typed before it still applies, a
// word that looks like a flag is an argument and never a flag name, and a
// "--" that is a flag's value ends nothing.
// NOT parallel: mutates the argv globals and env.
func TestCompletionAfterDoubleDashOffersTheArgumentSlot(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	t.Setenv(config.EnvCredentialStore, "file")
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers: map[string]config.Server{
			"prod": {URL: "https://prod.invalid"},
			"qa":   {URL: "https://qa.invalid"},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	cli.SetStoredConfigForTest(t, cfg, dir)
	root := buildRoot(t)

	shells := []string{"bash", "fish", "pwsh", "zsh"}
	tests := []struct {
		name string
		line completionLine
		want []string
	}{
		{"empty word", completionLine{typed: []string{"completion", "--"}}, shells},
		{"a slot past the declared arguments", completionLine{typed: []string{"completion", "--", "bash"}}, nil},
		{"a config-local slot", completionLine{typed: []string{"config", "use-server", "--"}}, []string{"prod", "qa"}},
		{"a config-local slot past the declared arguments", completionLine{typed: []string{"config", "use-server", "--", "qa"}}, nil},
		{"too many words after the dashes", completionLine{typed: []string{"completion", "--", "bash", "zsh"}}, nil},
		{"the dashes are a flag value before the command name", completionLine{typed: []string{"-c", "--", "completion"}}, nil},
		{"the dashes are a flag value and a flag name follows", completionLine{typed: []string{"config", "add-server", "--url", "--", "name"}, current: "--ap"}, []string{"--apply"}},
		{"a bool value before the dashes", completionLine{typed: []string{"completion", "--apply", "true", "--"}}, nil},
		{"partial word", completionLine{typed: []string{"completion", "--"}, current: "z"}, []string{"zsh"}},
		{"a flag before the dashes", completionLine{typed: []string{"completion", "--apply", "--"}}, shells},
		{"a flag with a value before the dashes", completionLine{typed: []string{"completion", "-o", "json", "--"}}, shells},
		{"a flag name is an argument", completionLine{typed: []string{"completion", "--"}, current: "--apply"}, nil},
		{"a dash is an argument", completionLine{typed: []string{"completion", "--"}, current: "-"}, nil},
		{"the dashes are a flag value", completionLine{typed: []string{"access", "can", "--subresource", "--"}}, slices.Sorted(slices.Values(access.ResourceVerbs))},
		{"a word typed after the dashes is an argument", completionLine{typed: []string{"access", "can", "--", "get"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, runErr := runCompletionLine(t, root, tc.line)
			require.NoError(t, runErr)
			assert.Equal(t, tc.want, extractTokens(out), strings.Join(tc.line.argv(), " "))
		})
	}
}

// TestCompletionAfterDoubleDashOffersNothingAtGroups asserts that the root and
// every group offer nothing after a "--": every word after it is an argument,
// so none names a command or a flag. The "--" may precede the group name, and
// the line may hold a flag before it. NOT parallel: mutates the argv globals
// and env.
func TestCompletionAfterDoubleDashOffersNothingAtGroups(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvConfigDir, t.TempDir())
	root := buildRoot(t)

	paths := [][]string{nil}
	for _, c := range root.Commands {
		if len(c.Commands) > 0 {
			paths = append(paths, []string{c.Name})
		}
	}
	require.Greater(t, len(paths), 1)
	for _, path := range paths {
		for _, l := range doubleDashLines(path) {
			out, runErr := runCompletionLine(t, root, l)
			desc := strings.Join(l.argv(), " ")
			require.NoError(t, runErr, desc)
			assert.Empty(t, extractTokens(out), desc)
		}
		before := completionLine{typed: append([]string{"--"}, path...), current: ""}
		out, runErr := runCompletionLine(t, root, before)
		require.NoError(t, runErr)
		assert.Empty(t, extractTokens(out), strings.Join(before.argv(), " "))
	}
}

// TestCompletionAfterDoubleDashReadsFlagsFromBeforeIt asserts that live
// completion after a "--" takes the flags from the words before it and counts
// the words after it as arguments only: a flag typed before the "--" names the
// cluster the argument is listed from, while the same words typed after it name
// nothing and make no request. NOT parallel: mutates the argv globals and env.
func TestCompletionAfterDoubleDashReadsFlagsFromBeforeIt(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"default"}},{"metadata":{"name":"kube-system"}}]}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvToken, "tok")
	cli.SetStoredConfigForTest(t, cfg, dir)
	root := buildRoot(t)

	// A source reads its cluster from the flags before the "--" and, for the
	// resource of access can, from the second argument slot.
	out, runErr := runCompletionLine(t, root, completionLine{typed: []string{"get", "namespace", "--cluster", "c-abc", "--"}, current: "kube"})
	require.NoError(t, runErr)
	assert.Equal(t, []string{"default", "kube-system"}, extractTokens(out))
	assert.Positive(t, requests.Load(), "the cluster typed before the dashes names where to list from")

	requests.Store(0)
	_, runErr = runCompletionLine(t, root, completionLine{typed: []string{"access", "can", "--cluster", "c-abc", "--", "get"}})
	require.NoError(t, runErr)
	assert.Positive(t, requests.Load(), "the cluster typed before the dashes names where the second argument is listed from")

	requests.Store(0)
	out, runErr = runCompletionLine(t, root, completionLine{typed: []string{"access", "can", "--", "--cluster=c-abc"}})
	require.NoError(t, runErr)
	assert.Empty(t, extractTokens(out))
	assert.Zero(t, requests.Load(), "words typed after the dashes are arguments, never a flag")
}

// TestCompletionFlagAsAFlagValueIsAnOrdinaryLine asserts that the word
// --generate-shell-completion typed as a flag's value in the middle of a line
// leaves the line an ordinary command: the flag guard and the command see the
// word where it was typed, so the exit code and the message are the command's
// own.
// The first line reaches the server, which answers 401; the second fails
// validation on its URL. NOT parallel: mutates the argv globals and env.
func TestCompletionFlagAsAFlagValueIsAnOrdinaryLine(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(func() { cli.CaptureArgvForTest(nil) })
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvConfigDir, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvToken, "token-abc:secret")
	t.Chdir(t.TempDir())

	tests := []struct {
		name     string
		words    []string
		wantCode int
		wantMsg  string
	}{
		{"a cluster named by the word", []string{"get", "pod", "--cluster", "--generate-shell-completion", "--namespace", "x", "mypod"}, 3, "rancher root GET /v1/management.cattle.io.clusters: the server rejected the credential (401)"},
		{"a URL given as the word", []string{"config", "add-server", "--url", "--generate-shell-completion", "--apply", "myserver"}, 2, "the server URL in --url cannot be used: it must be absolute (scheme://host)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, err := cli.BuildRoot("test")
			require.NoError(t, err)
			root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
			var stderr bytes.Buffer
			root.Writer = io.Discard
			root.ErrWriter = &stderr
			cli.CaptureArgvForTest(tc.words)
			runErr := root.Run(t.Context(), cli.SanitizedArgsForTest(append([]string{"rancher"}, tc.words...)))
			require.Error(t, runErr)
			coder, ok := errors.AsType[ucli.ExitCoder](runErr)
			require.True(t, ok, "the error carries an exit code")
			assert.Equal(t, tc.wantCode, coder.ExitCode())
			assert.Contains(t, stderr.String(), tc.wantMsg)
		})
	}
}
