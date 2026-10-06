package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"
)

// bareDashHint is the hint of the refusal of words typed after a bare
// "-", written independently of the pipeline's own rendering.
const bareDashHint = "put - last, or after -- when it is meant as an argument"

// bareDashMessage is the message of that refusal for the words, as typed,
// written independently of the pipeline's own rendering.
func bareDashMessage(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = clierr.ShellQuote(w)
	}
	return "a - on its own ends the command line, so the words after it would be ignored: " + strings.Join(quoted, " ")
}

// TestEveryLeafRefusesWordsAfterABareDash asserts that every leaf of the
// built tree, given its declared arguments, then a bare "-" and --dry-run,
// is refused with VALIDATION at exit 2, a message naming --dry-run and a
// hint saying where a "-" goes, with no request to the server it is
// pointed at and no file written; and that the same line without
// --dry-run, the "-" last, is what it was before the rule: a positional
// argument beyond the declared ones, refused as one. The commands are
// built once; each leaf runs, under a new root per run, in a subtest of
// its own. Not parallel: it sets the process environment and the captured
// command line.
func TestEveryLeafRefusesWordsAfterABareDash(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	dirs := emptyDirs(t)
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	m := wr.reg.Schema("v-test", config.MinRancherVersion)
	byName := map[string]runtime.SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}

	leaves, _ := commandPaths(wr.root("v-test"), nil)
	require.Len(t, leaves, len(m.Commands), "every command in the manifest is a visible leaf")

	for _, leaf := range leaves {
		name := strings.Join(leaf, " ")
		c, ok := byName[name]
		require.True(t, ok, "leaf %q is in the manifest", name)
		format := "json"
		if !(runtime.OutputPolicy{AllowedFormats: c.OutputPolicy}).Allows("json") {
			format = ""
		}
		last := append(slices.Clone(leaf), argValues(c.Args)...)
		if format != "" {
			last = append(last, "-o", format)
		}
		last = append(last, "-")
		dropped := append(slices.Clone(last), "--dry-run")
		t.Run(name, func(t *testing.T) {
			defer rec.take()
			stderr, code := runOnce(t, wr.root("v-test"), dropped)
			label := "rancher " + strings.Join(dropped, " ")
			assert.Equal(t, 2, code, label)
			assert.Empty(t, rec.take(), "%s: no request", label)
			assertNoFiles(t, label, dirs...)
			ce := renderedError(t, format, stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code, label)
			assert.Equal(t, bareDashMessage("--dry-run"), ce.Message, label)
			assert.Equal(t, bareDashHint, ce.Hint, label)

			stderr, code = runOnce(t, wr.root("v-test"), last)
			label = "rancher " + strings.Join(last, " ")
			assert.Equal(t, 2, code, label)
			assert.Empty(t, rec.take(), "%s: no request", label)
			ce = renderedError(t, format, stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code, label)
			assert.Contains(t, ce.Message, name+" "+declaredArgsText(c.Args)+"; unexpected argument '-'", label)
			assert.Equal(t, "rancher "+name+" --help", ce.Hint, label)
		})
	}
}

// TestWordsAfterABareDashAreRefused asserts the refusal of a command line
// with words after a bare "-" wherever the "-" sits: after the command
// name, where urfave takes it as an argument and drops the flags after it
// (--cluster, -o, --dry-run, --read-only), so a mutation would apply
// though --dry-run or --read-only was typed; padded with whitespace, which
// urfave trims; ahead of the command name, at the root or a group, where
// the command name itself is dropped, also with --help typed; and with
// more words than the refusal names, where a flag named last is named
// with the word after it. A word after the "-" that looks like a flag
// waiting for its value (`--cluster --output`) is not read either, and a
// flag that may be given once, typed again after the "-", is a word after
// it, not a second instance. Each is VALIDATION at exit 2, naming the
// words after the "-" as typed, with no request and no file written,
// rendered in the format the whole line selects, -o typed after the "-"
// included, at a leaf, a group or the root. An unknown command name typed
// ahead of the "-" is reported as an unknown command instead. The lines
// run as the process entry point runs them. Not parallel: it sets the
// process environment and the captured command line.
func TestWordsAfterABareDashAreRefused(t *testing.T) {
	restoreRawArgv(t)
	exited := stubExit(t)
	rec := methodRecorderEnv(t)
	dirs := emptyDirs(t)
	create := []string{"create", "namespace", "--cluster", "local", "--apply", "-o", "json"}
	for _, tc := range []struct {
		name   string
		argv   []string
		format string
		words  []string
	}{
		{"the flags after a get", []string{"get", "pods", "-", "--cluster", "c1", "-o", "json"}, "json", []string{"--cluster", "c1", "-o", "json"}},
		{"-o yaml after the dash at a leaf", []string{"get", "pods", "--cluster", "c1", "-", "-o", "yaml"}, "yaml", []string{"-o", "yaml"}},
		{"-o yaml after the dash overrides -o json before it", []string{"get", "pods", "--cluster", "c1", "-o", "json", "-", "--output=yaml"}, "yaml", []string{"--output=yaml"}},
		{"-o json after the dash at a group", []string{"get", "-", "pods", "-o", "json"}, "json", []string{"pods", "-o", "json"}},
		{"-o yaml after the dash at a group", []string{"get", "-", "pods", "-o", "yaml"}, "yaml", []string{"pods", "-o", "yaml"}},
		{"-o json after the dash at the root", []string{"-", "get", "pods", "-o", "json"}, "json", []string{"get", "pods", "-o", "json"}},
		{"-o yaml after the dash at the root", []string{"-", "get", "pods", "-o", "yaml"}, "yaml", []string{"get", "pods", "-o", "yaml"}},
		{"a flag given once, typed again after the dash", []string{"create", "project", "p1", "--cluster", "local", "--quota-limit", "limitsCpu=2", "-o", "json", "-", "--quota-limit", "limitsCpu=4"}, "json", []string{"--quota-limit", "limitsCpu=4"}},
		{"--dry-run after a get", []string{"get", "pods", "--cluster", "c1", "-", "--dry-run"}, "", []string{"--dry-run"}},
		{"--dry-run after an applied create", append(slices.Clone(create), "-", "--dry-run"), "json", []string{"--dry-run"}},
		{"--read-only after an applied create", append(slices.Clone(create), "-", "--read-only"), "json", []string{"--read-only"}},
		{"a padded dash", append(slices.Clone(create), " - ", "--dry-run"), "json", []string{"--dry-run"}},
		{"a dash with a tab", append(slices.Clone(create), "\t-", "--dry-run"), "json", []string{"--dry-run"}},
		{"a padded word after the dash", append(slices.Clone(create), "-", " --dry-run"), "json", []string{" --dry-run"}},
		{"a flag waiting for its value after the dash", []string{"get", "pods", "--cluster", "c1", "-o", "json", "-", "--cluster", "--output", "json"}, "json", []string{"--cluster", "--output", "json"}},
		{"ahead of the command name", []string{"-o", "json", "-", "get", "pods", "--cluster", "c1"}, "json", []string{"get", "pods", "--cluster", "c1"}},
		{"ahead of the leaf name", []string{"-o", "json", "get", "-", "pods", "--cluster", "c1"}, "json", []string{"pods", "--cluster", "c1"}},
		{"ahead of the command name, with --help", []string{"-o", "json", "--help", "-", "get"}, "json", []string{"get"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			*exited = 0
			stderr, code := runAsMain(t, root, tc.argv)
			if code == 0 {
				// A line with no command name read exits through
				// urfave's exit function.
				code = *exited
			}
			assert.Equal(t, 2, code)
			assert.Empty(t, rec.take(), "no request")
			assertNoFiles(t, tc.name, dirs...)
			ce := renderedBareDashError(t, tc.format, stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, bareDashMessage(tc.words...), ce.Message)
			assert.Equal(t, bareDashHint, ce.Hint)
		})
	}

	for _, tc := range []struct {
		name    string
		argv    []string
		message string
	}{
		{"more words than the refusal names", []string{"get", "pods", "-o", "json", "-", "a", "b", "c", "d", "e", "f", "g"}, bareDashMessage("a", "b", "c", "d", "e") + " and 2 more"},
		{"a flag and its value across the cap", []string{"get", "pods", "-o", "json", "-", "a", "b", "c", "d", "-o", "json", "x"}, bareDashMessage("a", "b", "c", "d", "-o", "json") + " and 1 more"},
		{"a flag and its value last, across the cap", []string{"-o", "json", "-", "get", "pods", "--cluster", "c1", "-o", "json"}, bareDashMessage("get", "pods", "--cluster", "c1", "-o", "json")},
		{"two flags across the cap", []string{"get", "pods", "-o", "json", "-", "a", "b", "c", "d", "--dry-run", "--apply"}, bareDashMessage("a", "b", "c", "d", "--dry-run") + " and 1 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			*exited = 0
			stderr, code := runAsMain(t, root, tc.argv)
			if code == 0 {
				code = *exited
			}
			assert.Equal(t, 2, code)
			assert.Empty(t, rec.take(), "no request")
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, tc.message, ce.Message)
		})
	}

	for _, tc := range []struct {
		name string
		argv []string
		hint string
	}{
		{"nosuch", []string{"-o", "json", "get", "nosuch", "-", "x"}, "rancher schema"},
		{"help", []string{"-o", "json", "help", "-", "x"}, "rancher --help"},
	} {
		t.Run("an unknown command name "+tc.name+" ahead of the dash", func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			*exited = 0
			stderr, _ := runAsMain(t, root, tc.argv)
			assert.Equal(t, 2, *exited)
			assert.Empty(t, rec.take(), "no request")
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, `unknown command "`+tc.name+`"`, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
		})
	}
}

// renderedBareDashError is renderedError that also reads the YAML
// envelope, for format "yaml".
func renderedBareDashError(t *testing.T, format, stderr string) cliError {
	t.Helper()
	if format != "yaml" {
		return renderedError(t, format, stderr)
	}
	require.False(t, json.Valid([]byte(stderr)), "YAML, not JSON: %s", stderr)
	var env struct {
		Kind string `json:"kind"`
		Data struct {
			Error cliError `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(stderr), &env), "stderr: %s", stderr)
	require.Equal(t, "Error", env.Kind)
	return env.Data.Error
}

// TestBareDashLinesTheRuleLeavesAlone asserts the lines with a "-" that
// the rule does not refuse, each read as urfave reads it: a "-" as a
// flag's separate value reads the manifest from stdin and keeps the
// --dry-run after it (one dry-run request, nothing created); a "-" after
// "--" is an argument and the words after it are kept, so the extra one
// is refused as an extra argument; a "-" that is the last word is a
// positional argument, as before. Not parallel: it sets the process
// environment and the captured command line.
func TestBareDashLinesTheRuleLeavesAlone(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	run := func(t *testing.T, stdin string, argv ...string) (string, int, []string) {
		t.Helper()
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter, root.Reader = &out, &errOut, strings.NewReader(stdin)
		CaptureArgv(argv)
		err = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, argv...)))
		return errOut.String(), exitCodeOf(err), rec.take()
	}
	const manifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns1\n"
	const dryRun = "POST /k8s/clusters/local/api/v1/namespaces?dryRun=All&fieldManager=cli.rancher.io"

	for _, spelling := range [][]string{{"-f", "-"}, {"--filename", "-"}} {
		argv := slices.Concat([]string{"create", "resource", "--cluster", "local"}, spelling, []string{"--dry-run", "-o", "json"})
		stderr, code, requests := run(t, manifest, argv...)
		label := "rancher " + strings.Join(argv, " ")
		assert.Equal(t, 0, code, "%s: %s", label, stderr)
		assert.Contains(t, requests, dryRun, label)
		assert.NotContains(t, requests, "POST /k8s/clusters/local/api/v1/namespaces?fieldManager=cli.rancher.io", "%s: nothing is created", label)
	}

	argv := []string{"get", "pods", "--cluster", "c1", "-o", "json", "--", "-", "x"}
	stderr, code, requests := run(t, "", argv...)
	assert.Equal(t, 2, code)
	assert.Empty(t, requests)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, "get pod takes at most 1 argument ([name]); unexpected argument 'x'", ce.Message, "rancher %s", strings.Join(argv, " "))

	argv = []string{"get", "pods", "--cluster", "c1", "-o", "json", "-"}
	stderr, code, requests = run(t, "", argv...)
	assert.Equal(t, 2, code)
	assert.Empty(t, requests)
	ce = renderedError(t, "json", stderr)
	assert.Equal(t, "--namespace is required to address a pod by name", ce.Message, "rancher %s: - is the pod name", strings.Join(argv, " "))
}

// TestScanTailStopsAtABareDash asserts how scanTail reads a "-": a word
// that is only "-" once trimmed is a positional argument, kept as "-",
// and every word after it is dropped, kept as typed; a "-" as a flag's
// separate value is a value; a "-" after "--" or after a word urfave reads
// as a number is a plain argument, and the words after it are kept.
func TestScanTailStopsAtABareDash(t *testing.T) {
	t.Parallel()
	flags := map[string]bool{"f": true, "dry-run": false}
	type word struct {
		text string
		kind wordKind
		dash bool
	}
	for _, tc := range []struct {
		name string
		tail []string
		want []word
	}{
		{"a dash last", []string{"a", "-"}, []word{{"a", plainWord, false}, {"-", plainWord, true}}},
		{"words after a dash", []string{"-", "--dry-run", " b "}, []word{{"-", plainWord, true}, {"--dry-run", droppedWord, false}, {" b ", droppedWord, false}}},
		{"a padded dash", []string{" -\t", "x"}, []word{{"-", plainWord, true}, {"x", droppedWord, false}}},
		{"a dash as a flag's value", []string{"-f", "-", "--dry-run"}, []word{{"-f", flagWord, false}, {"-", valueWord, false}, {"--dry-run", flagWord, false}}},
		{"a dash after --", []string{"--", "-", "x"}, []word{{"--", endWord, false}, {"-", plainWord, false}, {"x", plainWord, false}}},
		{"a dash after a number", []string{"-1", "-", "x"}, []word{{"-1", plainWord, false}, {"-", plainWord, false}, {"x", plainWord, false}}},
		{"a double dash after a dash", []string{"-", "--", "x"}, []word{{"-", plainWord, true}, {"--", droppedWord, false}, {"x", droppedWord, false}}},
		{"two dashes", []string{"-", "-"}, []word{{"-", plainWord, true}, {"-", droppedWord, false}}},
		{"a dash, a space and a dash in one word", []string{"- -"}, []word{{"- -", plainWord, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []word
			for _, w := range scanTail(tc.tail, flags) {
				got = append(got, word{w.text, w.kind, w.dash})
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDroppedWordsOffersNothingWhenTheLineDisagrees asserts that the
// reader of the words after a bare "-" reports them, as typed, for the
// command line urfave parsed for the leaf, and offers nothing for a line
// with no word after a bare "-" or a line that is not the one parsed,
// also one with as many positional arguments as parsed but another one
// among them, so a refusal never comes from another line.
func TestDroppedWordsOffersNothingWhenTheLineDisagrees(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var leaf *ucli.Command
	for _, c := range root.Commands {
		if c.Name == "get" {
			leaf = c.Command("pod")
		}
	}
	require.NotNil(t, leaf)
	argv := []string{"-o", "json", "get", "pod", "--cluster", "c1", " - ", "--dry-run", " x"}
	_, _ = runOnce(t, root, argv)
	require.Equal(t, []string{"-"}, leaf.Args().Slice())

	words, ok := droppedWords(argv, leaf)
	require.True(t, ok)
	assert.Equal(t, []string{"--dry-run", " x"}, words)

	// The same number of arguments as parsed, another one among them.
	other, err := BuildRoot("v-test")
	require.NoError(t, err)
	var parsed *ucli.Command
	for _, c := range other.Commands {
		if c.Name == "get" {
			parsed = c.Command("pod")
		}
	}
	require.NotNil(t, parsed)
	_, _ = runOnce(t, other, []string{"get", "pod", "p1", "-", "x"})
	require.Equal(t, []string{"p1", "-"}, parsed.Args().Slice())
	words, ok = droppedWords([]string{"get", "pod", "p1", "-", "x"}, parsed)
	require.True(t, ok)
	assert.Equal(t, []string{"x"}, words)
	_, ok = droppedWords([]string{"get", "pod", "p2", "-", "x"}, parsed)
	assert.False(t, ok, "another argument than the one parsed")

	for name, other := range map[string][]string{
		"the dash last":              {"get", "pod", "--cluster", "c1", "-"},
		"another positional":         {"get", "pod", "p1", "-", "--dry-run"},
		"the dash after --":          {"get", "pod", "--", "-", "--dry-run"},
		"the dash as a flag's value": {"get", "pod", "--cluster", "-", "--dry-run"},
		"no dash":                    {"get", "pod", "--cluster", "c1", "--dry-run"},
	} {
		_, ok := droppedWords(other, leaf)
		assert.False(t, ok, name)
	}
}

// TestBareDashCompletesNothing asserts that completion offers nothing, and
// makes no request with network completion on, on a line with a bare "-"
// typed before the word being completed - after the leaf's name, padded,
// ahead of the leaf's name, after a group's name, or at the root - since
// that word would be one urfave never reads, while a "-" typed as a flag's
// value keeps the candidates, and the line without the "-" asks the
// server for the leaf's live candidates. Not parallel: it sets the process
// environment and the captured command line.
func TestBareDashCompletesNothing(t *testing.T) {
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

	complete := func(t *testing.T, words []string, cur string) (string, []string) {
		t.Helper()
		scriptArgs := append(append([]string{protoSentinel}, words...), curPrefix+cur, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = &out
		root.Reader = strings.NewReader("")
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String(), rec.take()
	}
	for _, tc := range []struct {
		words []string
		cur   string
	}{
		{[]string{"get", "pod", "-"}, "--"},
		{[]string{"get", "pod", " - "}, "--"},
		{[]string{"get", "pod", "--cluster", "c1", "-"}, ""},
		{[]string{"-", "get", "pod"}, "--"},
		{[]string{"get", "-"}, ""},
		{[]string{"-"}, ""},
	} {
		label := "rancher " + strings.Join(tc.words, " ") + " " + tc.cur + "<TAB>"
		out, requests := complete(t, tc.words, tc.cur)
		assert.Empty(t, out, label)
		assert.Empty(t, requests, "%s: no request", label)
	}

	out, _ := complete(t, []string{"create", "resource", "-f", "-"}, "--")
	assert.Contains(t, out, "--dry-run", "rancher create resource -f - --<TAB>: a - as a flag's value keeps the candidates")
	_, requests := complete(t, []string{"get", "pod", "--cluster", "c1"}, "")
	assert.NotEmpty(t, requests, "rancher get pod --cluster c1 <TAB> asks the server for pod names")
}

// TestWordsAfterABareDashRenderInTheColorTyped asserts that the refusal
// of words after a bare "-" takes its color from the --color typed
// anywhere on the line, the words after the "-" included, the last one
// winning, at a leaf, a group and the root: `--color always` after the
// "-" styles the text form, and `--color never` after it keeps the text
// form plain though `--color always` was typed before it. Not parallel:
// it sets the process environment and the captured command line.
func TestWordsAfterABareDashRenderInTheColorTyped(t *testing.T) {
	restoreRawArgv(t)
	exited := stubExit(t)
	rec := methodRecorderEnv(t)
	for _, tc := range []struct {
		name   string
		argv   []string
		styled bool
	}{
		{"always after the dash at a leaf", []string{"get", "pods", "--cluster", "c1", "-", "--color", "always"}, true},
		{"never after the dash at a leaf", []string{"get", "pods", "--cluster", "c1", "--color", "always", "-", "--color=never"}, false},
		{"always after the dash at a group", []string{"get", "-", "pods", "--color", "always"}, true},
		{"never after the dash at a group", []string{"--color", "always", "get", "-", "pods", "--color", "never"}, false},
		{"always after the dash at the root", []string{"-", "get", "pods", "--color", "always"}, true},
		{"never after the dash at the root", []string{"--color", "always", "-", "get", "--color", "never"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			*exited = 0
			stderr, code := runAsMain(t, root, tc.argv)
			if code == 0 {
				// A line with no command name read exits through
				// urfave's exit function.
				code = *exited
			}
			assert.Equal(t, 2, code)
			assert.Empty(t, rec.take(), "no request")
			assert.Equal(t, tc.styled, strings.Contains(stderr, "\x1b["), "styled: %q", stderr)
			plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(stderr, "")
			ce := renderedError(t, "", plain)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, bareDashHint, ce.Hint)
		})
	}
}
