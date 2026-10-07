package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// requestRecorder is a server that records the path of every request it
// gets and answers each with an empty list.
type requestRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *requestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"items":[]}`))
}

// take returns the recorded requests and clears them.
func (r *requestRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.paths
	r.paths = nil
	return out
}

// extraArgsEnv points every command at a request-recording server through
// RANCHER_CLI_URL and RANCHER_CLI_TOKEN, with an empty config directory, the
// file credential store and a temporary working directory, and returns the
// server. Not for a parallel test: it sets the process environment.
func extraArgsEnv(t *testing.T) *requestRecorder {
	t.Helper()
	rec := &requestRecorder{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvToken, "token-x:y")
	t.Chdir(t.TempDir())
	return rec
}

// cliError is the error a command rendered: decoded from the JSON envelope,
// or read from the two lines of the text form.
type cliError struct{ Code, Message, Hint string }

// renderedError reads the error a run wrote to stderr in format ("json" or
// the text form for "").
func renderedError(t *testing.T, format, stderr string) cliError {
	t.Helper()
	if format == "json" {
		var env struct {
			Kind string `json:"kind"`
			Data struct {
				Error cliError `json:"error"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(stderr), &env), "stderr: %s", stderr)
		require.Equal(t, "Error", env.Kind)
		return env.Data.Error
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	var ce cliError
	head, ok := strings.CutPrefix(lines[0], "error (")
	require.True(t, ok, "stderr: %s", stderr)
	ce.Code, ce.Message, ok = strings.Cut(head, "): ")
	require.True(t, ok, "stderr: %s", stderr)
	for _, l := range lines[1:] {
		if h, ok := strings.CutPrefix(l, "  hint: "); ok {
			ce.Hint = h
		}
	}
	return ce
}

// runOnce runs argv through root, with the command line captured as the
// process entry point captures it, and returns stderr and the exit code.
// root must be one no earlier run has used.
func runOnce(t *testing.T, root *ucli.Command, argv []string) (stderr string, code int) {
	t.Helper()
	_, stderr, code = runCaptured(t, root, argv)
	return stderr, code
}

// runCaptured is runOnce that also returns stdout.
func runCaptured(t *testing.T, root *ucli.Command, argv []string) (stdout, stderr string, code int) {
	t.Helper()
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader("")
	CaptureArgv(argv)
	err := root.Run(t.Context(), append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), exitCodeOf(err)
}

// argValues returns a value for each positional argument args declares:
// the first enum value when the argument has one, a plain word otherwise.
func argValues(args []runtime.ArgSpec) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = fmt.Sprintf("a%d", i+1)
		if len(a.Enum) > 0 {
			out[i] = a.Enum[0]
		}
	}
	return out
}

// TestEveryLeafRefusesAnExtraPositionalArgument asserts that every leaf of
// the built tree, given one positional argument more than it declares, is
// refused with VALIDATION at exit 2, a message naming the extra word, the
// command's own --help as the hint, and no request to the server it is
// pointed at. The commands are built once; each leaf runs once, under a new
// root, in a subtest of its own, so one failing leaf does not hide another.
// Not parallel: it sets the process environment and the captured command
// line.
func TestEveryLeafRefusesAnExtraPositionalArgument(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	m := wr.reg.Schema("v-test", config.MinRancherVersion)
	byName := map[string]runtime.SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}

	leaves, _ := commandPaths(wr.root("v-test"), nil)
	require.Len(t, leaves, len(m.Commands), "every command in the manifest is a visible leaf")

	const extra = "zz extra"
	for _, leaf := range leaves {
		name := strings.Join(leaf, " ")
		c, ok := byName[name]
		require.True(t, ok, "leaf %q is in the manifest", name)
		format := "json"
		if !(runtime.OutputPolicy{AllowedFormats: c.OutputPolicy}).Allows("json") {
			format = ""
		}
		argv := append(append(slices.Clone(leaf), argValues(c.Args)...), extra)
		if format != "" {
			argv = append(argv, "-o", format)
		}
		t.Run(name, func(t *testing.T) {
			defer rec.take()
			stderr, code := runOnce(t, wr.root("v-test"), argv)
			label := "rancher " + strings.Join(argv, " ")
			assert.Equal(t, 2, code, label)
			assert.Empty(t, rec.take(), "%s: no request", label)
			ce := renderedError(t, format, stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code, label)
			assert.Contains(t, ce.Message, name+" "+declaredArgsText(c.Args), label)
			assert.Contains(t, ce.Message, "unexpected argument "+clierr.ShellQuote(extra), label)
			assert.Equal(t, "rancher "+name+" --help", ce.Hint, label)
		})
	}
}

// declaredArgsText is the part of the refusal that states what a command
// takes, written from the manifest independently of the pipeline's own
// rendering: "takes no arguments", "takes 1 argument (<name>)", "takes at
// most 2 arguments (<verb> [resource])".
func declaredArgsText(args []runtime.ArgSpec) string {
	if len(args) == 0 {
		return "takes no arguments"
	}
	var names []string
	optional := false
	for _, a := range args {
		if a.Required {
			names = append(names, "<"+a.Name+">")
		} else {
			names = append(names, "["+a.Name+"]")
			optional = true
		}
	}
	text := fmt.Sprintf("%d argument", len(args))
	if len(args) > 1 {
		text += "s"
	}
	if optional {
		text = "at most " + text
	}
	return "takes " + text + " (" + strings.Join(names, " ") + ")"
}

// boolRule is the part of the refusal of a bool value typed as its own
// word that says what to do instead.
const boolRule = "; a bool flag takes its value joined with =, and an argument that reads as a bool goes before the flags or after --"

// TestBoolValueAfterBoolFlagIsRefused asserts the refusal of a positional
// word typed as its own word directly after a bool flag that accepts it as
// its value, in any position - a free declared slot, before the declared
// argument, beyond the declared arguments: VALIDATION at exit 2, a message
// naming each such word and its flag and saying what to do instead, and
// the command as typed with each value joined to its flag as the hint
// (the command's --help when that line would still hold an extra word or
// leave a declared argument, required or optional, empty).
// A word the flag does not accept, a word after a joined value and a word
// after "--" keep the count rule. No row makes a request. Not parallel: it
// sets the process environment and the captured command line.
func TestBoolValueAfterBoolFlagIsRefused(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)

	for _, tc := range []struct {
		name    string
		argv    []string
		message string
		hint    string
	}{
		{
			name:    "a free required slot: the joined line would lack the argument",
			argv:    []string{"create", "user", "--password-stdin", "--dry-run", "false", "-o", "json"},
			message: "create user: 'false' after the bool flag --dry-run" + boolRule,
			hint:    "rancher create user --help",
		},
		{
			name:    "a free required slot, a destructive command",
			argv:    []string{"delete", "project", "--yes", "true", "--cluster", "c1", "-o", "json"},
			message: "delete project: 'true' after the bool flag --yes" + boolRule,
			hint:    "rancher delete project --help",
		},
		{
			name:    "a free optional slot: the joined line would leave it empty",
			argv:    []string{"get", "pod", "--wait", "true", "--cluster", "c1", "-o", "json"},
			message: "get pod: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher get pod --help",
		},
		{
			name:    "before the declared argument",
			argv:    []string{"get", "pod", "--wait", "true", "mypod", "--cluster", "c1", "-o", "json"},
			message: "get pod: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher get pod --wait='true' 'mypod' --cluster 'c1' -o 'json'",
		},
		{
			name:    "no free slot, a global bool flag",
			argv:    []string{"create", "user", "u04", "--password-stdin", "--dry-run", "false", "-o", "json"},
			message: "create user: 'false' after the bool flag --dry-run" + boolRule,
			hint:    "rancher create user 'u04' --password-stdin --dry-run='false' -o 'json'",
		},
		{
			name:    "no free slot, a command bool flag, any value strconv.ParseBool accepts",
			argv:    []string{"get", "pod", "p1", "--wait", "TRUE", "--cluster", "c 1", "-o", "json"},
			message: "get pod: 'TRUE' after the bool flag --wait" + boolRule,
			hint:    "rancher get pod 'p1' --wait='TRUE' --cluster 'c 1' -o 'json'",
		},
		{
			name:    "two bool flags, global flags ahead of the command",
			argv:    []string{"-o", "json", "--color=never", "create", "user", "it's", "--yes", "0", "--dry-run", "t", "--password-stdin"},
			message: "create user: '0' after the bool flag --yes, 't' after the bool flag --dry-run" + boolRule,
			hint:    `rancher -o 'json' --color='never' create user 'it'\''s' --yes='0' --dry-run='t' --password-stdin`,
		},
		{
			name:    "a bool value and an extra word that is not one",
			argv:    []string{"create", "user", "u04", "--dry-run", "false", "extra", "-o", "json"},
			message: "create user: 'false' after the bool flag --dry-run" + boolRule + "; create user takes 1 argument (<username>); unexpected argument 'extra'",
			hint:    "rancher create user --help",
		},
		{
			name:    "a value the flag does not accept",
			argv:    []string{"create", "user", "u04", "--dry-run", "maybe", "-o", "json"},
			message: "create user takes 1 argument (<username>); unexpected argument 'maybe'",
			hint:    "rancher create user --help",
		},
		{
			name:    "a bool flag with a joined value",
			argv:    []string{"create", "user", "u04", "--dry-run=true", "false", "-o", "json"},
			message: "create user takes 1 argument (<username>); unexpected argument 'false'",
			hint:    "rancher create user --help",
		},
		{
			name:    "after the end of the flags",
			argv:    []string{"-o", "json", "create", "user", "u04", "--dry-run", "--", "false"},
			message: "create user takes 1 argument (<username>); unexpected argument 'false'",
			hint:    "rancher create user --help",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runOnce(t, root, tc.argv)
			assert.Equal(t, 2, code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Empty(t, rec.take(), "no request")
		})
	}
}

// TestBoolLookingArgumentsTheRuleLeavesAlone asserts the lines the bool
// value rule does not refuse: an argument that reads as a bool typed
// before the flags or after "--", an argument after a joined bool value,
// and a word the bool flag does not accept in a free slot all reach the
// command (auth delete-token, which succeeds whatever name it is given).
// Not parallel: it sets the process environment and the captured command
// line.
func TestBoolLookingArgumentsTheRuleLeavesAlone(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for name, argv := range map[string][]string{
		"before the flags":                 {"auth", "delete-token", "true", "--read-only", "-o", "json"},
		"after --":                         {"auth", "delete-token", "--read-only", "-o", "json", "--", "true"},
		"after a joined bool value":        {"auth", "delete-token", "--yes=true", "true", "-o", "json"},
		"a value the flag does not accept": {"auth", "delete-token", "--read-only", "maybe", "-o", "json"},
	} {
		t.Run(name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runOnce(t, root, argv)
			assert.Equal(t, 0, code, stderr)
			assert.Empty(t, stderr)
		})
	}
}

// shellWords splits a hint into its words the way a POSIX shell does for
// the quoting clierr.ShellQuote writes: single-quoted runs, a backslash
// escaping one character, and unquoted spaces between words.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\'':
			quoted = false
		case quoted:
			cur.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == '\\':
			require.Less(t, i+1, len(line), "a trailing backslash in %q", line)
			i++
			cur.WriteByte(line[i])
			inWord = true
		case c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	require.False(t, quoted, "an unclosed quote in %q", line)
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// TestJoinedFormHintRunsVerbatim asserts that the hint of a refused bool
// value runs as written. A joined-form hint gets past flag parsing and the
// argument checks: the run succeeds, sends a request to the server, or
// fails with an error other than VALIDATION, the one VALIDATION allowed
// being the empty stdin a test run gives --password-stdin. A hint that is
// the command's help (the joined line would leave a declared argument
// empty) prints that help. Not parallel: it sets the process environment
// and the captured command line.
func TestJoinedFormHintRunsVerbatim(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	for _, tc := range []struct {
		argv []string
		help bool
	}{
		{argv: []string{"config", "path", "--read-only", "true", "-o", "json"}},
		{argv: []string{"auth", "delete-token", "--read-only", "true", "prod", "-o", "json"}},
		{argv: []string{"get", "pod", "--wait", "false", "mypod", "--cluster", "c 1", "--namespace", "n", "-o", "json"}},
		{argv: []string{"get", "pod", "--wait", "true", "mypod", "--cluster", "c1", "--namespace", "n", "-o", "json"}},
		{argv: []string{"get", "pod", "--wait", "true", "--cluster", "c1", "--namespace", "n", "-o", "json"}, help: true},
		{argv: []string{"-o", "json", "create", "user", "it's", "--yes", "0", "--dry-run", "t", "--password-stdin"}},
		{argv: []string{"delete", "project", "p1", "--cluster", "c1", "--yes", "true", "--dry-run", "false", "-o", "json"}},
		{argv: []string{"delete", "project", "--yes", "true", "--cluster", "c1", "-o", "json"}, help: true},
		{argv: []string{"create", "user", "--password-stdin", "--dry-run", "false", "-o", "json"}, help: true},
	} {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runOnce(t, root, tc.argv)
			require.Equal(t, 2, code, stderr)
			hint := renderedError(t, "json", stderr).Hint
			words := shellWords(t, hint)
			require.NotEmpty(t, words, "the refusal carries a hint: %s", stderr)
			require.Equal(t, "rancher", words[0], hint)
			assert.Equal(t, tc.help, words[len(words)-1] == "--help", hint)

			root, err = BuildRoot("v-test")
			require.NoError(t, err)
			stdout, stderr, code := runCaptured(t, root, words[1:])
			requests := rec.take()
			if tc.help {
				assert.Equal(t, 0, code, stderr)
				assert.Contains(t, stdout, "USAGE:", hint)
				return
			}
			if code == 0 || len(requests) > 0 {
				return
			}
			ce := renderedError(t, "json", stderr)
			if ce.Code == clierr.CodeValidation {
				assert.True(t, strings.HasPrefix(ce.Message, "--password-stdin produced an empty password"),
					"%s: refused before running: %s", hint, ce.Message)
			}
		})
	}
}

// TestBoolValueArgsOffersNothingWhenTheLineDisagrees asserts that the
// bool value reader reports the positional arguments typed after a bool
// flag, with the joined line, for the command line urfave parsed for the
// leaf, and offers nothing for a line with no such argument or a line that
// is not the one parsed, so a refusal and its hint never come from another
// line.
func TestBoolValueArgsOffersNothingWhenTheLineDisagrees(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var leaf *ucli.Command
	for _, c := range root.Commands {
		if c.Name == "create" {
			leaf = c.Command("user")
		}
	}
	require.NotNil(t, leaf)
	argv := []string{"create", "user", "u04", "--dry-run", "false", "--password-stdin"}
	_, _ = runOnce(t, root, argv)
	require.Equal(t, []string{"u04", "false"}, leaf.Args().Slice())

	vals, line, ok := boolValueArgs(argv, leaf)
	require.True(t, ok)
	assert.Equal(t, []runtime.BoolValue{{Index: 1, Flag: "--dry-run"}}, vals)
	assert.Equal(t, "rancher create user 'u04' --dry-run='false' --password-stdin", line)

	for name, other := range map[string][]string{
		"no argument after a bool flag": {"create", "user", "u04", "false", "--dry-run"},
		"another positional":            {"create", "user", "u05", "--dry-run", "false"},
		"no positional typed":           {"create", "user", "--dry-run"},
		"words in another order":        {"create", "user", "--dry-run", "false", "u04"},
	} {
		_, _, ok := boolValueArgs(other, leaf)
		assert.False(t, ok, name)
	}
}

// TestEveryLeafCompletesNothingPastItsArguments asserts that for every leaf
// of the built tree, once the line holds every positional argument the leaf
// declares, completing a further word offers no candidate and makes no
// request, and that a line holding a bool value typed after its flag
// (`--dry-run true`) offers no flag either, with network completion on.
// Two sets of commands are built, one per line, and each leaf runs once
// per set, under a new root, in a subtest of its own. Not parallel: it
// sets the process environment and the captured command line.
func TestEveryLeafCompletesNothingPastItsArguments(t *testing.T) {
	restoreRawArgv(t)
	stubExit(t)
	rec := extraArgsEnv(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: os.Getenv(config.EnvURL)}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(os.Getenv(config.EnvConfigDir)))
	require.NoError(t, cfg.Write())

	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	byName := map[string]runtime.SchemaCommand{}
	for _, c := range wr.reg.Schema("v-test", config.MinRancherVersion).Commands {
		byName[c.Name] = c
	}
	// A second set of commands for the bool value lines: each leaf runs
	// once per set.
	wrBool, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	leaves, _ := commandPaths(wr.root("v-test"), nil)
	require.Len(t, leaves, len(byName))

	complete := func(t *testing.T, root *ucli.Command, words []string, cur string) {
		t.Helper()
		scriptArgs := append(append([]string{protoSentinel}, words...), curPrefix+cur, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &out
		root.Reader = strings.NewReader("")
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		label := "rancher " + strings.Join(words, " ") + " " + cur + "<TAB>"
		assert.Empty(t, out.String(), label)
		assert.Empty(t, rec.take(), "%s: no request", label)
	}
	for _, leaf := range leaves {
		name := strings.Join(leaf, " ")
		c := byName[name]
		t.Run(name, func(t *testing.T) {
			defer rec.take()
			complete(t, wr.root("v-test"), append(slices.Clone(leaf), argValues(c.Args)...), "")
			complete(t, wrBool.root("v-test"), append(slices.Clone(leaf), "--dry-run", "true"), "--")
		})
	}
}

// TestSchemaSecondWordCompletesThroughTheBuiltRoot asserts, through the
// built root and the lines the shipped scripts send, that `schema
// auth <TAB>` offers exactly the second words of the selectors that begin
// with auth: the words typed before the one completed reach the schema
// command's completer. Not parallel: it sets the process environment and
// the captured command line.
func TestSchemaSecondWordCompletesThroughTheBuiltRoot(t *testing.T) {
	restoreRawArgv(t)
	stubExit(t)
	extraArgsEnv(t)
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var want []string
	for _, c := range m.Commands {
		verb, rest, ok := strings.Cut(c.Name, " ")
		if !ok || verb != "auth" {
			continue
		}
		want = append(want, rest)
		want = append(want, c.Aliases...)
	}
	slices.Sort(want)
	want = slices.Compact(want)
	require.NotEmpty(t, want)

	scriptArgs := []string{protoSentinel, "schema", "auth", curPrefix, "--generate-shell-completion"}
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
	assert.Equal(t, want, strings.Fields(out.String()))
}

// TestSchemaKindCompletesKindsOnly asserts, through the built root and
// a marked line, that with --kind on the line (alone or joined with a
// true value) schema's first word offers exactly the names, keys and
// aliases of the manifest's kinds, each of which resolves through the
// schema command as a kind, and a second word nothing; without --kind, or
// with --kind=false, the first word offers command and group selectors as
// well. Not parallel: it sets the process environment and the captured
// command line.
func TestSchemaKindCompletesKindsOnly(t *testing.T) {
	restoreRawArgv(t)
	stubExit(t)
	extraArgsEnv(t)
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	m := wr.reg.Schema("v-test", config.MinRancherVersion)
	var want []string
	for _, k := range m.Kinds {
		want = append(want, k.Name, k.Key)
		want = append(want, k.Aliases...)
	}
	slices.Sort(want)
	want = slices.Compact(want)
	require.NotEmpty(t, want)

	complete := func(words ...string) []string {
		scriptArgs := append(append([]string{protoSentinel}, words...), curPrefix, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return strings.Fields(out.String())
	}

	schema := runtime.NewSchemaCommand(wr.reg, "v-test", config.MinRancherVersion, nil).(runtime.ReadCommand)
	for _, spelling := range []string{"--kind", "--kind=true"} {
		got := complete("schema", spelling)
		assert.Equal(t, want, got, "rancher schema %s <TAB>", spelling)
		for _, cand := range got {
			_, err := schema.Execute(t.Context(), cmdtest.Input{Pos: []string{cand}, Flag: map[string]bool{"kind": true}}, nil)
			require.NoError(t, err, "rancher schema %s %s", cand, spelling)
		}
		assert.Empty(t, complete("schema", spelling, "auth"), "rancher schema %s auth <TAB>", spelling)
		assert.Empty(t, complete("schema", spelling, "pod"), "rancher schema %s pod <TAB>", spelling)
	}
	for _, words := range [][]string{{"schema"}, {"schema", "--kind=false"}} {
		assert.Contains(t, complete(words...), "auth", "rancher %s <TAB> offers command selectors", strings.Join(words, " "))
	}
}
