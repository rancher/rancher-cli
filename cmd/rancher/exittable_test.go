package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateExitTable = flag.Bool("update", false, "rewrite the exit table golden")

// exitTableStdoutEnv, when set to a directory, makes TestExitTable save the
// stdout of every line there (one numbered file per line, the temporary
// paths replaced by the placeholder) for a one-time comparison across a
// change. The golden itself holds no stdout: help, completion and version
// text change with every change to the command surface.
const exitTableStdoutEnv = "RANCHER_EXIT_TABLE_STDOUT_DIR"

const exitTablePlaceholder = "<TMP>"

// exitRow is one command line of the exit table: the words after the
// program name, extra environment variables, and the body of a config.yaml
// to write into the line's own config directory first.
type exitRow struct {
	words  []string
	env    []string
	config string
}

func line(words ...string) exitRow { return exitRow{words: words} }

func (r exitRow) withEnv(env ...string) exitRow {
	r.env = env
	return r
}

func (r exitRow) withConfig(body string) exitRow {
	r.config = body
	return r
}

const unreachableURL = "https://127.0.0.1:1"

// exitTableRows lists the lines the table runs: every kind of exit main can
// make for a command line, and at least one line for every command group.
func exitTableRows() []exitRow {
	rows := []exitRow{
		line(),
		line(""),
		line("-h"),
		line("-v"),
		line("--help"),
		line("--version"),
		line("get", "--help"),
		line("get", "pod", "--help"),
		line("get", "pod", "-h"),
		line("help"),

		line("config", "path"),
		line("config", "status"),
		line("schema"),
		line("skill", "status"),
		line("completion", "bash"),
		line("completion", "bogus"),

		line("skill", "status", "--agent", "claude", "--require-installed"),
		line("skill", "status", "--agent", "claude", "--require-installed", "-o", "json"),

		line("auth", "get-token"),
		line("auth", "get-token", "--server", unreachableURL, "--cluster", "c-1"),
		line("token"),
		line("token", "--server", unreachableURL),

		line("bogus"),
		line("bogus", "-o", "json"),
		line("bogus", "--help"),
		line("help", "bogus"),
		line("get", "bogus"),
		line("get", "bogus", "-o", "json"),
		line("get", "bogus", "--help"),
		line("help", "get", "bogus"),

		line("--bogus"),
		line("get", "--bogus"),
		line("get", "pod", "--bogus"),
		line("get", "pod", "--bogus", "-o", "json"),
		line("__completion", "--bogus"),
		line("get", "pod", "--output"),
		line("get", "pod", "--cluster", "local", "--config"),

		line("get", "pod", "a", "b", "c"),
		line("get", "pod", "a", "b", "c", "-o", "json"),
		line("delete", "pod"),
		line("get", "pod"),

		line("--config", "", "config", "path"),
		line("--config", " ", "get", "pod", "--cluster", "local"),
		line("-o", "bogus", "config", "path"),
		line("--color", "bogus", "config", "path"),

		line("get", "pod", "-", "x"),
		line("-", "get", "pod"),
		line("config", "path", "--yes="),
		line("delete", "pod", "x", "--cluster", "local", "--apply", "--yes= "),

		line("config", "path", "--read-only"),
		line("delete", "pod", "x", "--cluster", "local", "--apply", "--yes").withEnv("RANCHER_CLI_READONLY=1"),

		line("get", "cluster"),
		line("get", "cluster").withConfig("not: [valid yaml\n"),
		line("get", "cluster").withEnv(config.EnvURL+"="+unreachableURL, config.EnvToken+"=id:secret"),

		line("--rancher-completion-proto=2", "--rancher-completion-cur=g"),
		line("--rancher-completion-proto=2", "--rancher-completion-cur=", "get", "pod", "--"),
	}
	for _, group := range []string{
		"access", "auth", "cluster", "completion", "config", "create", "delete",
		"get", "grant", "images", "inspect", "kubeconfig", "logs", "oidc-client",
		"patch", "project", "revoke", "schema", "skill", "support-bundle",
		"telemetry", "token", "usage",
	} {
		rows = append(rows, line(group))
	}
	return rows
}

// exitTableEnv builds a child's whole environment: the parent's PATH and the
// variables a test binary needs, HOME and the config directory under root,
// and nothing else from the developer's shell.
func exitTableEnv(root, cfgDir string, extra []string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + root,
		"USERPROFILE=" + root,
		config.EnvConfigDir + "=" + cfgDir,
		config.EnvCredentialStore + "=file",
		"RANCHER_CLI_TELEMETRY=false",
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP", "SystemRoot"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	// A test binary built with -cover warns on stderr at exit when
	// GOCOVERDIR is unset.
	cover, ok := os.LookupEnv("GOCOVERDIR")
	if !ok {
		cover = filepath.Join(root, "cover")
	}
	env = append(env, "GOCOVERDIR="+cover)
	return append(env, extra...)
}

var dialErrorRE = regexp.MustCompile(`(dial tcp 127\.0\.0\.1:1: ).*`)

// exitTableText replaces the temporary directory root, in both path
// spellings and with either separator, by the placeholder, and the part of
// a dial error that differs between operating systems.
func exitTableText(text, root string) string {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	for _, r := range roots {
		text = strings.ReplaceAll(text, r, exitTablePlaceholder)
		text = strings.ReplaceAll(text, filepath.ToSlash(r), exitTablePlaceholder)
	}
	return dialErrorRE.ReplaceAllString(text, "${1}<dial error>")
}

func exitTableWord(w string) string {
	if w == "" || strings.ContainsAny(w, " \t\"'\\") {
		return strconv.Quote(w)
	}
	return w
}

// exitTableEntry renders one line of the golden: the words, the extra
// environment and config, the exit code and every stderr line prefixed by
// "> ".
func exitTableEntry(r exitRow, code int, stderr string) string {
	var b strings.Builder
	words := make([]string, len(r.words))
	for i, w := range r.words {
		words[i] = exitTableWord(w)
	}
	b.WriteString("$ rancher " + strings.Join(words, " ") + "\n")
	for _, e := range r.env {
		b.WriteString("env: " + e + "\n")
	}
	if r.config != "" {
		b.WriteString("config.yaml: " + strconv.Quote(r.config) + "\n")
	}
	fmt.Fprintf(&b, "exit: %d\n", code)
	if stderr == "" {
		b.WriteString("stderr: (empty)\n")
		return b.String()
	}
	b.WriteString("stderr:\n")
	trailing := strings.HasSuffix(stderr, "\n")
	for l := range strings.SplitSeq(strings.TrimSuffix(stderr, "\n"), "\n") {
		b.WriteString("> " + l + "\n")
	}
	if !trailing {
		b.WriteString("(no trailing newline)\n")
	}
	return b.String()
}

// TestExitTable asserts that main, run as a process for every line of the
// table, exits with the recorded code and writes the recorded stderr, so a
// change that moves where the process ends keeps every line's exit status
// and error text. Regenerate the golden on purpose with
// "go test ./cmd/rancher/ -run TestExitTable -update". The golden is the
// same under "go test" and "go test -cover". The table runs on Linux and
// macOS: no Windows job runs this package.
func TestExitTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the golden holds paths with forward slashes and the dial error text of Linux and macOS; no Windows job runs this package")
	}
	t.Parallel()
	rows := exitTableRows()
	require.GreaterOrEqual(t, len(rows), 63)
	stdoutDir := os.Getenv(exitTableStdoutEnv)
	if stdoutDir != "" {
		require.NoError(t, os.MkdirAll(stdoutDir, 0o755))
	}
	entries := make([]string, len(rows))
	codes := make([]int, len(rows))
	t.Run("lines", func(t *testing.T) {
		for i, r := range rows {
			t.Run(strconv.Itoa(i)+" "+strings.Join(r.words, " "), func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				cfgDir := filepath.Join(root, "cfg")
				require.NoError(t, os.MkdirAll(cfgDir, 0o755))
				work := filepath.Join(root, "work")
				require.NoError(t, os.MkdirAll(work, 0o755))
				if r.config != "" {
					require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(r.config), 0o600))
				}
				stdout, stderr, code := runMainProcess(t, exitTableEnv(root, cfgDir, r.env), work, r.words...)
				if stdoutDir != "" {
					name := filepath.Join(stdoutDir, fmt.Sprintf("%03d.txt", i))
					require.NoError(t, os.WriteFile(name, []byte(exitTableText(stdout, root)), 0o644))
				}
				entries[i] = exitTableEntry(r, code, exitTableText(stderr, root))
				codes[i] = code
			})
		}
	})
	if t.Failed() {
		return
	}
	assert.Contains(t, codes, 0)
	assert.Contains(t, codes, 1)
	assert.Contains(t, codes, 2)
	assert.Contains(t, codes, 7)

	got := strings.Join(entries, "\n")
	golden := filepath.Join("testdata", "exit-table.golden")
	if *updateExitTable {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}
