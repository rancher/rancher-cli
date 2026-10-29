package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// leakingStreams names the streams of a run that hold a character of the
// remedy marker (U+2060 to U+2064), in the order stdout, stderr.
func leakingStreams(stdout, stderr string) []string {
	var out []string
	if holdsMarkerRuneDecoded(stdout) {
		out = append(out, "stdout")
	}
	if holdsMarkerRuneDecoded(stderr) {
		out = append(out, "stderr")
	}
	return out
}

// filesHoldingMarkerRunes returns the path, relative to root, of every file
// under root that holds a character of the remedy marker.
func filesHoldingMarkerRunes(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if holdsMarkerRuneDecoded(string(b)) {
			rel, _ := filepath.Rel(root, path)
			found = append(found, rel)
		}
		return nil
	})
	require.NoError(t, err)
	return found
}

// TestTheLeakCheckSeesEachMarkerRune asserts the check on a run's output
// reports each of the five characters, in stdout and in stderr, and nothing
// for text without them.
func TestTheLeakCheckSeesEachMarkerRune(t *testing.T) {
	t.Parallel()
	for r := '\u2060'; r <= '\u2064'; r++ {
		planted := "a" + string(r) + "b"
		assert.Equal(t, []string{"stdout"}, leakingStreams(planted, "clean"), "U+%04X in stdout", r)
		assert.Equal(t, []string{"stderr"}, leakingStreams("clean", planted), "U+%04X in stderr", r)
		assert.Equal(t, []string{"stdout", "stderr"}, leakingStreams(planted, planted), "U+%04X in both", r)
	}
	assert.Empty(t, leakingStreams("clean", "also \u2065 and \u205f, which are not part of it"))
	// A json encoder that wrote the character as an escape would hide it from
	// the bytes; the decoded strings show it.
	assert.Equal(t, []string{"stdout"}, leakingStreams(`{"message": "a\u2063b"}`, "clean"))
	assert.Equal(t, []string{"stderr"}, leakingStreams("clean", "message: \"a\\u2062b\"\n"))
}

// TestTheDirectoryScanReportsAFileWithAMarkerRune asserts the scan of a
// config directory reports, with its path, a file that holds any one of the
// five characters, in a nested directory too, and no file that holds none.
func TestTheDirectoryScanReportsAFileWithAMarkerRune(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested", "deeper"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "config.yaml"), []byte("servers: {}\n"), 0o600))
	var planted []string
	for r := '\u2060'; r <= '\u2064'; r++ {
		name := filepath.Join("nested", "deeper", "u"+strconv.FormatInt(int64(r), 16)+".yaml")
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("error: a"+string(r)+"b\n"), 0o600))
		planted = append(planted, name)
	}
	got := filesHoldingMarkerRunes(t, root)
	slices.Sort(got)
	assert.Equal(t, planted, got)
}

// TestEveryExitTableRowLeaksNoMarkerIntoStructuredOutput runs every line of
// the exit table once with --output json and once with --output yaml
// appended, whatever its exit code, and asserts that no character of the
// remedy marker (U+2060 to U+2064) reaches stdout, stderr or a file under the
// line's config directory. A line that fails before it renders, or that holds
// -o already, checks nothing about its output; the lines below that must
// succeed, and the completion script run as it is, keep the check from
// passing for nothing.
func TestEveryExitTableRowLeaksNoMarkerIntoStructuredOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the exit table runs on Linux and macOS")
	}
	t.Parallel()
	rows := exitTableRows()
	var ran, succeeded atomic.Int32
	t.Run("lines", func(t *testing.T) {
		for i, r := range rows {
			for _, format := range []string{"json", "yaml"} {
				t.Run(strconv.Itoa(i)+" "+strings.Join(r.words, " ")+" "+format, func(t *testing.T) {
					t.Parallel()
					root := t.TempDir()
					cfgDir := filepath.Join(root, "cfg")
					require.NoError(t, os.MkdirAll(cfgDir, 0o755))
					work := filepath.Join(root, "work")
					require.NoError(t, os.MkdirAll(work, 0o755))
					if r.config != "" {
						require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(r.config), 0o600))
					}
					words := append(slices.Clone(r.words), "--output", format)
					stdout, stderr, code := runMainProcess(t, exitTableEnv(root, cfgDir, r.env), work, words...)
					ran.Add(1)
					if code == 0 && stdout != "" {
						succeeded.Add(1)
					}
					assert.Empty(t, leakingStreams(stdout, stderr), "the output holds a character of the remedy marker")
					assert.Empty(t, filesHoldingMarkerRunes(t, cfgDir), "a file of the config directory holds a character of the remedy marker")
				})
			}
		}
	})
	t.Logf("%d runs of %d exit table lines, %d of them exited 0 with output", ran.Load(), len(rows), succeeded.Load())
}

// TestStructuredOutputOfTheCommandsThatMustSucceedIsChecked asserts the
// lines that keep the leak check honest: config path, config status and
// schema, each with --output json, exit 0 and write to stdout, and the
// completion script, run as it is, exits 0 and prints its script. Each is
// held to the same check.
func TestStructuredOutputOfTheCommandsThatMustSucceedIsChecked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the exit table runs on Linux and macOS")
	}
	t.Parallel()
	for _, tc := range []struct {
		name  string
		words []string
	}{
		{"config path", []string{"config", "path", "--output", "json"}},
		{"config status", []string{"config", "status", "--output", "json"}},
		{"schema", []string{"schema", "--output", "json"}},
		{"completion bash as it is", []string{"completion", "bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			cfgDir := filepath.Join(root, "cfg")
			require.NoError(t, os.MkdirAll(cfgDir, 0o755))
			work := filepath.Join(root, "work")
			require.NoError(t, os.MkdirAll(work, 0o755))
			stdout, stderr, code := runMainProcess(t, exitTableEnv(root, cfgDir, nil), work, tc.words...)
			require.Zero(t, code, stderr)
			require.NotEmpty(t, stdout)
			assert.Empty(t, leakingStreams(stdout, stderr))
			assert.Empty(t, filesHoldingMarkerRunes(t, cfgDir))
		})
	}
}

// plainInput is the Input of a command that takes no flags and no arguments.
type plainInput struct{}

func (plainInput) String(string) string    { return "" }
func (plainInput) Bool(string) bool        { return false }
func (plainInput) Strings(string) []string { return nil }
func (plainInput) IsSet(string) bool       { return false }
func (plainInput) Args() []string          { return nil }

// textCommand prints a text that holds a marked command.
type textCommand struct{}

func (textCommand) Describe() rt.Descriptor {
	return rt.Descriptor{Name: "config status", CommandType: rt.Local, Shape: rt.ShapeText}
}

func (textCommand) Execute(context.Context, rt.Input, rt.Prompter) (rt.Result, error) {
	return rt.Result{Text: "next: " + clierr.Rancher + " auth login\n"}, nil
}

// TestTextAndErrorsWrittenWithoutAnInvocationLeakNoMarker asserts the two
// writes that expand no remedy for a directory - a result's text, and an
// error rendered by a caller that has no invocation, as the CLI does before
// it knows which directory a run uses - hold the marked command as the plain
// command, with no character of the remedy marker (U+2060 to U+2064), in
// every format. The exit table cannot reach either with a marked command, so
// this row stands for them: dropping the plain rewrite in plainRemedies fails
// it.
func TestTextAndErrorsWrittenWithoutAnInvocationLeakNoMarker(t *testing.T) {
	t.Parallel()
	ce := clierr.New(clierr.CodeNotLoggedIn, "not logged in; run "+clierr.Rancher+" auth login 'prod'").
		WithHint(clierr.Rancher + " auth login 'prod'").WithWarnings("see " + clierr.Rancher + " config status")
	for _, format := range []string{"", "json", "yaml"} {
		var b bytes.Buffer
		require.NoError(t, rt.RenderErrorStyled(&b, format, ce, style.Style{}))
		if format == "" {
			assert.Contains(t, b.String(), "rancher auth login 'prod'", format)
		} else {
			strs := decodedStrings(b.String())
			require.NotEmpty(t, strs, "the error in format %q is a document", format)
			assert.True(t, slices.ContainsFunc(strs, func(s string) bool { return strings.Contains(s, "rancher auth login 'prod'") }),
				"a string of the error in format %q holds the plain command: %q", format, strs)
		}
		assert.False(t, holdsMarkerRuneDecoded(b.String()), "the error in format %q holds a character of the remedy marker: %q", format, b.String())
	}
	var out, errBuf bytes.Buffer
	_, got := rt.Run(t.Context(), textCommand{}, plainInput{}, rt.IO{Out: &out, Err: &errBuf}, safety.Binding{})
	require.Nil(t, got)
	assert.Equal(t, "next: rancher auth login\n", out.String())
}
