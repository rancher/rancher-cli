package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// emptyBoolMessage is the refusal of the bool flags typed with "=" and an
// empty value, flags as typed without the "=", written independently of
// the pipeline's own rendering.
func emptyBoolMessage(flags ...string) (message, hint string) {
	var named, fixes []string
	for _, f := range flags {
		named = append(named, f+"=")
		fixes = append(fixes, f+" or "+f+"=true|false")
	}
	verb := "has"
	if len(flags) > 1 {
		verb = "have"
	}
	return strings.Join(named, ", ") + " " + verb + " an empty value; a bool flag typed with = takes true or false",
		"write " + strings.Join(fixes, "; ")
}

// assertNoFiles asserts that every directory in dirs is still empty.
func assertNoFiles(t *testing.T, label string, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries, "%s: no file written under %s", label, dir)
	}
}

// emptyDirs returns the directories extraArgsEnv gives a run: the home,
// config and working directories.
func emptyDirs(t *testing.T) []string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return []string{os.Getenv("HOME"), os.Getenv(config.EnvConfigDir), wd}
}

// TestEveryLeafRefusesAnEmptyBoolValue asserts that every leaf of the built
// tree, given every bool flag it accepts - its own, under each of their
// names, and the global ones, as the schema manifest lists them - typed
// with "=" and an empty value, is refused with VALIDATION at exit 2, a
// message naming each of those flags and a hint saying to write each alone
// or with true or false, with no request to the server it is pointed at
// and no file written. The commands are built once, so each leaf runs
// once, with all its bool flags on the one line, under a new root, in a
// subtest of its own. Not parallel: it sets the process environment and
// the captured command line.
func TestEveryLeafRefusesAnEmptyBoolValue(t *testing.T) {
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
	var globals []string
	for _, g := range m.GlobalFlags {
		if g.Type == "bool" {
			globals = append(globals, g.Name)
		}
	}
	require.Contains(t, globals, "--apply")
	require.Contains(t, globals, "--yes")

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
		var flags []string
		for _, f := range c.Flags {
			if f.Type != "bool" {
				continue
			}
			flags = append(flags, "--"+f.Name)
			for _, a := range f.Aliases {
				flags = append(flags, "-"+a)
			}
		}
		flags = append(flags, globals...)
		argv := append(slices.Clone(leaf), argValues(c.Args)...)
		for _, f := range flags {
			argv = append(argv, f+"=")
		}
		if format != "" {
			argv = append(argv, "-o", format)
		}
		message, hint := emptyBoolMessage(flags...)
		t.Run(name, func(t *testing.T) {
			defer rec.take()
			stderr, code := runOnce(t, wr.root("v-test"), argv)
			label := "rancher " + strings.Join(argv, " ")
			assert.Equal(t, 2, code, label)
			assert.Empty(t, rec.take(), "%s: no request", label)
			assertNoFiles(t, label, dirs...)
			ce := renderedError(t, format, stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code, label)
			assert.Equal(t, message, ce.Message, label)
			assert.Equal(t, hint, ce.Hint, label)
		})
	}
}

// methodRecorder is a server that records the method, path and query of
// every request and answers each with a namespace object.
type methodRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *methodRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.Path+"?"+req.URL.RawQuery)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns1","resourceVersion":"1"}}`))
}

// take returns the recorded requests and clears them.
func (r *methodRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.requests
	r.requests = nil
	return out
}

// methodRecorderEnv is extraArgsEnv with every command pointed at a
// methodRecorder instead. Not for a parallel test: it sets the process
// environment.
func methodRecorderEnv(t *testing.T) *methodRecorder {
	t.Helper()
	extraArgsEnv(t)
	rec := &methodRecorder{}
	srv := httptest.NewTLSServer(rec)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return rec
}

// runAsMain runs argv through root as the process entry point does: the
// command line captured, and urfave given SanitizedArgs of it. It returns
// stderr and the exit code. root must be one no earlier run has used.
func runAsMain(t *testing.T, root *ucli.Command, argv []string) (stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader("")
	code = runMain(t, root, argv...)
	return errOut.String(), code
}

// TestEmptyBoolValueIsRefused asserts the refusal of a bool flag typed with
// "=" and an empty value wherever it sits: on a mutating command's --apply,
// on a destructive command's --yes, as a global flag ahead of the command
// name, as a short alias, as a long name with one dash, twice, and followed
// by the same flag with a value. A value that is only whitespace (a space,
// a tab, a newline, a no-break space) and whitespace ahead of the flag are
// read with the whitespace cut off, a word that is only whitespace is
// kept as typed, and so is a line holding the completion protocol marker,
// which urfave never sees. Each is VALIDATION at exit 2 with no request,
// naming each flag once. The lines run as the process entry point runs
// them. Not parallel: it sets the process environment and the captured
// command line.
func TestEmptyBoolValueIsRefused(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	dirs := emptyDirs(t)
	for _, tc := range []struct {
		name  string
		argv  []string
		flags []string
	}{
		{"--apply= on a mutating command", []string{"create", "namespace", "ns1", "--cluster", "local", "--apply=", "-o", "json"}, []string{"--apply"}},
		{"--yes= on a destructive command", []string{"delete", "namespace", "ns1", "--cluster", "local", "--apply", "--yes=", "-o", "json"}, []string{"--yes"}},
		{"a global flag ahead of the command name", []string{"--apply=", "create", "namespace", "ns1", "--cluster", "local", "-o", "json"}, []string{"--apply"}},
		{"a global flag between the command names", []string{"-o", "json", "delete", "--yes=", "namespace", "ns1", "--cluster", "local", "--apply"}, []string{"--yes"}},
		{"a short alias", []string{"access", "list", "--cluster", "local", "-A=", "-o", "json"}, []string{"-A"}},
		{"a long name with one dash", []string{"access", "list", "--cluster", "local", "-all-namespaces=", "-o", "json"}, []string{"-all-namespaces"}},
		{"two flags, one typed twice", []string{"delete", "namespace", "ns1", "--cluster", "local", "--apply=", "--yes=", "--apply=", "-o", "json"}, []string{"--apply", "--yes"}},
		{"an empty value, then the flag with a value", []string{"create", "namespace", "ns1", "--cluster", "local", "--apply=", "--apply=true", "-o", "json"}, []string{"--apply"}},
		{"a space as the value", []string{"delete", "namespace", "ns1", "--cluster", "local", "--apply", "--yes= ", "-o", "json"}, []string{"--yes"}},
		{"a tab as the value", []string{"create", "namespace", "ns1", "--cluster", "local", "--apply=\t", "-o", "json"}, []string{"--apply"}},
		{"a newline as the value", []string{"delete", "namespace", "ns1", "--cluster", "local", "--apply", "--yes=\n", "-o", "json"}, []string{"--yes"}},
		{"a no-break space as the value", []string{"create", "namespace", "ns1", "--cluster", "local", "--apply=\u00a0", "-o", "json"}, []string{"--apply"}},
		{"whitespace ahead of the flag", []string{"create", "namespace", "ns1", "--cluster", "local", " --apply=", "-o", "json"}, []string{"--apply"}},
		{"whitespace ahead of a flag ahead of the command name", []string{" --apply=", "create", "namespace", "ns1", "--cluster", "local", "-o", "json"}, []string{"--apply"}},
		{"a tab ahead of a short alias", []string{"access", "list", "--cluster", "local", "\t-A=", "-o", "json"}, []string{"-A"}},
		{"a word that is only whitespace on the line", []string{"create", "namespace", "  ", "--cluster", "local", "--apply=", "-o", "json"}, []string{"--apply"}},
		{"the completion protocol marker on the line", []string{"create", "namespace", "ns1", "--cluster", protoSentinel, "local", "--apply=", "-o", "json"}, []string{"--apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runAsMain(t, root, tc.argv)
			assert.Equal(t, 2, code)
			assert.Empty(t, rec.take(), "no request")
			assertNoFiles(t, tc.name, dirs...)
			message, hint := emptyBoolMessage(tc.flags...)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, message, ce.Message)
			assert.Equal(t, hint, ce.Hint)
		})
	}
}

// TestPaddedWordsAreReadAsUrfaveReadsThem asserts that the bool value and
// extra argument rules read a word with whitespace around it the way urfave
// does: a positional word is used as typed, and is classified with the
// whitespace cut off. A padded bool value typed after a bool flag is
// refused as a bool value, in a free declared slot or beyond the declared
// argument, and named as typed, with the joined line's value trimmed so the
// hint runs, and a padded extra word is named as typed. A flag's separate
// value and a word after "--" stay as typed in the hint. No row makes a
// request. Not parallel: it sets the process environment and the captured
// command line.
func TestPaddedWordsAreReadAsUrfaveReadsThem(t *testing.T) {
	restoreRawArgv(t)
	rec := extraArgsEnv(t)
	for _, tc := range []struct {
		name    string
		argv    []string
		message string
		hint    string
	}{
		{
			name:    "a padded bool value in a free slot",
			argv:    []string{"create", "user", "--dry-run", " false", "--password-stdin", "-o", "json"},
			message: "create user: ' false' after the bool flag --dry-run" + boolRule,
			hint:    "rancher create user --help",
		},
		{
			name:    "a padded bool value beyond the declared argument",
			argv:    []string{"create", "user", "u04", "--dry-run", "false\t", "--password-stdin", "-o", "json"},
			message: "create user: 'false\t' after the bool flag --dry-run" + boolRule,
			hint:    "rancher create user 'u04' --dry-run='false' --password-stdin -o 'json'",
		},
		{
			name:    "a padded separate value of a value flag stays as typed",
			argv:    []string{"get", "pod", "p1", "--wait", "true", "--cluster", " c1", "-o", "json"},
			message: "get pod: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher get pod 'p1' --wait='true' --cluster ' c1' -o 'json'",
		},
		{
			name:    "a padded word after -- stays as typed",
			argv:    []string{"get", "pod", "--wait", "true", "--cluster", "c1", "-o", "json", "--", " p1"},
			message: "get pod: 'true' after the bool flag --wait" + boolRule,
			hint:    "rancher get pod --wait='true' --cluster 'c1' -o 'json' -- ' p1'",
		},
		{
			name:    "a padded extra word",
			argv:    []string{"create", "user", "u04", " extra", "--password-stdin", "-o", "json"},
			message: "create user takes 1 argument (<username>); unexpected argument ' extra'",
			hint:    "rancher create user --help",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			stderr, code := runAsMain(t, root, tc.argv)
			assert.Equal(t, 2, code)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Empty(t, rec.take(), "no request")
		})
	}
}

// TestBoolValuesTheEmptyRuleLeavesAlone asserts the lines the empty bool
// value rule does not refuse: --apply alone or joined with a true value
// applies the change (the dry-run request, then the request that makes
// it), --apply joined with a false value previews it the way a line
// without --apply does, and `--apply=` after "--" is an argument. Not
// parallel: it sets the process environment and the captured command line.
func TestBoolValuesTheEmptyRuleLeavesAlone(t *testing.T) {
	restoreRawArgv(t)
	rec := methodRecorderEnv(t)
	run := func(t *testing.T, argv ...string) (int, []string) {
		t.Helper()
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		_, code := runOnce(t, root, argv)
		return code, rec.take()
	}
	create := []string{"create", "namespace", "ns1", "--cluster", "local", "-o", "json"}
	const dryRun = "POST /k8s/clusters/local/api/v1/namespaces?dryRun=All&fieldManager=cli.rancher.io"
	const apply = "POST /k8s/clusters/local/api/v1/namespaces?fieldManager=cli.rancher.io"

	previewCode, preview := run(t, create...)
	require.Equal(t, []string{dryRun}, preview)
	require.NotZero(t, previewCode)

	for _, spelling := range []string{"--apply", "--apply=true", "--apply=1"} {
		code, requests := run(t, append(slices.Clone(create), spelling)...)
		assert.Equal(t, 0, code, spelling)
		assert.Equal(t, []string{dryRun, apply}, requests, spelling)
	}
	for _, spelling := range []string{"--apply=false", "--apply=0"} {
		code, requests := run(t, append(slices.Clone(create), spelling)...)
		assert.Equal(t, previewCode, code, spelling)
		assert.Equal(t, preview, requests, spelling)
	}

	code, _ := run(t, "auth", "delete-token", "-o", "json", "--", "--apply=")
	assert.Equal(t, 0, code, "rancher auth delete-token -o json -- --apply=")
}

// TestIsEmptyBool asserts which words the empty bool value rule reads as a
// bool flag with "=" and an empty value: a bool flag the command accepts,
// joined with "=" and nothing or only whitespace after it, with or without
// whitespace around the word, and not a flag the command does not accept,
// a flag that takes a value, a bool flag with a value or without "=", or a
// word after "--".
func TestIsEmptyBool(t *testing.T) {
	t.Parallel()
	flags := map[string]bool{"dry-run": false, "A": false, "cluster": true}
	for word, want := range map[string]bool{
		"--dry-run=":   true,
		"-A=":          true,
		"--dry-run= ":  true,
		" --dry-run=":  true,
		"--dry-run=\t": true,
		"--bogus=":     false,
		"--cluster=":   false,
		"--dry-run=0":  false,
		"--dry-run":    false,
	} {
		words := scanTail([]string{word}, flags)
		require.Len(t, words, 1, "%q", word)
		assert.Equal(t, want, isEmptyBool(words[0]), "%q", word)
	}
	afterEnd := scanTail([]string{"--", "--dry-run="}, flags)
	assert.False(t, isEmptyBool(afterEnd[1]), "--dry-run= after --")
}

// TestEmptyBoolValueCompletesNothing asserts that completion offers nothing,
// and makes no request with network completion on, on a line holding a
// bool flag typed with "=" and an empty value - after the leaf's name,
// ahead of it, after a group's name, or at the root, also with whitespace
// around the word - while the same line
// with the flag joined to a value still offers candidates, and asks the
// server for the leaf's live candidates. Not parallel: it sets the
// process environment and the captured command line.
func TestEmptyBoolValueCompletesNothing(t *testing.T) {
	restoreRawArgv(t)
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
		{[]string{"get", "pod", "--dry-run="}, "--"},
		{[]string{"--dry-run=", "get", "pod"}, "--"},
		{[]string{"get", "--dry-run="}, ""},
		{[]string{"--dry-run="}, ""},
	} {
		label := "rancher " + strings.Join(tc.words, " ") + " " + tc.cur + "<TAB>"
		out, requests := complete(t, tc.words, tc.cur)
		assert.Empty(t, out, label)
		assert.Empty(t, requests, "%s: no request", label)

		joined := slices.Clone(tc.words)
		joined[slices.Index(joined, "--dry-run=")] = "--dry-run=false"
		out, _ = complete(t, joined, tc.cur)
		assert.NotEmpty(t, out, "rancher %s %s<TAB> offers candidates", strings.Join(joined, " "), tc.cur)
	}

	// Words with whitespace around them: a
	// blank joined bool value, whitespace ahead of a flag, a padded bool
	// value after a bool flag, and a padded flag the leaf does not accept.
	for _, words := range [][]string{
		{"get", "pod", "--dry-run= "},
		{"get", "pod", " --dry-run="},
		{"get", "pod", "--wait", " true"},
		{"get", "pod", " --bogus"},
	} {
		label := "rancher " + strings.Join(words, " ") + " --<TAB>"
		out, requests := complete(t, words, "--")
		assert.Empty(t, out, label)
		assert.Empty(t, requests, "%s: no request", label)
	}

	// A live source: the pod names, which the line with the empty value
	// never asks the server for.
	out, requests := complete(t, []string{"get", "pod", "--cluster", "c1", "--dry-run="}, "")
	assert.Empty(t, out, "rancher get pod --cluster c1 --dry-run= <TAB>")
	assert.Empty(t, requests, "rancher get pod --cluster c1 --dry-run= <TAB>: no request")
	_, requests = complete(t, []string{"get", "pod", "--cluster", "c1", "--dry-run=false"}, "")
	assert.NotEmpty(t, requests, "rancher get pod --cluster c1 --dry-run=false <TAB> asks the server for pod names")
}
