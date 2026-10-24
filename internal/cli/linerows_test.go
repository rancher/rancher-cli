package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyRecorder is a server that records the method, escaped path, query and
// body of every request and answers each with a namespace object, except a
// read of the management API, which it answers with an empty collection.
type bodyRecorder struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (r *bodyRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.EscapedPath()+"?"+req.URL.RawQuery)
	r.bodies = append(r.bodies, string(body))
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/v1/") {
		_, _ = w.Write([]byte(`{"type":"collection","revision":"1","count":0,"data":[]}`))
		return
	}
	_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns1","resourceVersion":"1"}}`))
}

// take returns the recorded requests and their bodies and clears both.
func (r *bodyRecorder) take() (requests, bodies []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	requests, bodies = r.requests, r.bodies
	r.requests, r.bodies = nil, nil
	return requests, bodies
}

// bodyRecorderEnv is extraArgsEnv with every command pointed at a
// bodyRecorder instead. Not for a parallel test: it sets the process
// environment.
func bodyRecorderEnv(t *testing.T) *bodyRecorder {
	t.Helper()
	extraArgsEnv(t)
	rec := &bodyRecorder{}
	srv := httptest.NewTLSServer(rec)
	t.Cleanup(srv.Close)
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	return rec
}

// lineWant is what a command line comes to.
type lineWant struct {
	// exit is the exit code.
	exit int
	// message and hint are the error a line that fails with one rendered.
	message, hint string
	// usage is the usage line of the help a line shows, and stdout the
	// pattern the output of a line that shows the version matches.
	usage, stdout string
	// requests are the requests the line makes: none unless stated.
	requests []string
}

// refused is a line refused with a VALIDATION error before any request.
func refused(message, hint string) lineWant {
	return lineWant{exit: 2, message: message, hint: hint}
}

// urfaveError is a line urfave fails on, before any request.
func urfaveError(message string) lineWant {
	return refused(message+"; pass --help for usage", "rancher schema")
}

// unknownCommand is a line that names a command that does not exist.
func unknownCommand(name string) lineWant {
	return refused(`unknown command "`+name+`"`, "rancher schema")
}

// helpCard is a line that shows the help of a command: its usage line.
func helpCard(usage string) lineWant { return lineWant{usage: usage} }

// versionOut is a line that prints the version.
var versionOut = lineWant{stdout: `^rancher version v-test\n$`}

// droppedWordsRule is the end of the message that refuses words after a "-"
// on its own, after the words it names.
const droppedWordsRule = " after a - on its own; a - on its own ends the command line"

// TestCommandLineRowsGiveTheirResult asserts the result of command lines
// that hold a "-" on its own, a bool flag typed with "=" and an empty value
// or one of only whitespace, a bool value typed as its own word, a word with
// whitespace around it, the help flag or the version flag among them, each
// run as the process runs it: the error and its hint, or the help card or
// version printed, and the requests made. Not parallel: it sets the process
// environment and the captured command line.
func TestCommandLineRowsGiveTheirResult(t *testing.T) {
	restoreRawArgv(t)
	rec := bodyRecorderEnv(t)

	const (
		dashHint  = bareDashHint
		getPod    = "rancher get pod [options] [name]"
		getPodErr = "rancher get pod [name]"
		root      = "rancher [global options] [command [command options]]"
	)
	local := []string{"--cluster", "local"}
	get := append([]string{"get", "pods"}, local...)
	line := func(words ...string) []string { return words }
	with := func(prefix []string, words ...string) []string {
		return append(append([]string{}, prefix...), words...)
	}
	emptyBoolRefused := func(flag string) lineWant {
		message, hint := emptyBoolMessage(flag)
		return refused(message, hint)
	}
	droppedRefused := func(words ...string) lineWant {
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = clierr.ShellQuote(w)
		}
		return refused(strings.Join(quoted, " ")+droppedWordsRule, dashHint)
	}

	rows := []struct {
		name string
		argv []string
		want lineWant
	}{
		{"a padded dash then a flag", line("create", "namespace", "--cluster", "local", "--apply", "-o", "json", " - ", "--dry-run"), droppedRefused("--dry-run")},
		{"a dash at a group", line("get", "-", "pods", "-o", "json"), droppedRefused("pods", "-o", "json")},
		{"a dash at the root", line("-", "get", "pods", "-o", "json"), droppedRefused("get", "pods", "-o", "json")},
		{"a flag given once, typed again after the dash", line("create", "project", "p1", "--cluster", "local", "--quota-limit", "limitsCpu=2", "-o", "json", "-", "--quota-limit", "limitsCpu=4"), droppedRefused("--quota-limit", "limitsCpu=4")},
		{"a flag waiting for its value after the dash", line("get", "pods", "--cluster", "c1", "-o", "json", "-", "--cluster", "--output", "json"), droppedRefused("--cluster", "--output", "json")},
		{"help typed before the dash at the root", line("-o", "json", "--help", "-", "get"), droppedRefused("get")},
		{"help typed before the dash at a leaf", line("get", "pods", "--help", "-", "x"), helpCard(getPod)},
		{"the version typed before the dash", line("--version", "-", "get"), versionOut},
		{"an unknown command before the dash", line("-o", "json", "get", "nosuch", "-", "x"), unknownCommand("nosuch")},
		{"an undefined flag before the dash", line("get", "pods", "--bogus", "-", "x"), urfaveError("flag provided but not defined: -bogus")},
		{"an undefined flag after the dash", line("get", "pods", "-", "--bogus"), droppedRefused("--bogus")},
		{"a bool flag with a space as the value", line("delete", "namespace", "ns1", "--cluster", "local", "--apply", "--yes= ", "-o", "json"), emptyBoolRefused("--yes")},
		{"a bool flag with a tab as the value", line("create", "namespace", "ns1", "--cluster", "local", "--apply=\t", "-o", "json"), emptyBoolRefused("--apply")},
		{"a blank bool value then help", with(get, "--yes= ", "--help"), emptyBoolRefused("--yes")},
		{"help then a blank bool value", with(get, "--help", "--yes= "), helpCard(getPodErr)},
		{"a blank help value", with(get, "--help= "), emptyBoolRefused("--help")},
		{"a blank help value then a dash and a word", with(get, "--help= ", "-", "x"), droppedRefused("x")},
		{"help, a blank bool value, then a dash and a word", with(get, "--help", "--yes= ", "-", "x"), helpCard(getPodErr)},
		{"help then a blank bool value at the root", line("--help", "--yes= "), helpCard(root)},
		{"help turned off then a blank bool value at the root", line("--help", "--help=false", "--yes= "), emptyBoolRefused("--yes")},
		{"an empty help value", with(get, "--help="), helpCard(getPod)},
		{"a blank bool value ahead of the command, then help", with(line("--dry-run= "), append(get, "--help")...), emptyBoolRefused("--dry-run")},
		{"an unknown command then a blank bool value", line("nosuch", "--yes= "), unknownCommand("nosuch")},
		{"an undefined flag then a blank bool value", with(get, "--bogus", "--yes= "), urfaveError("flag provided but not defined: -bogus")},
		{"a blank bool value then an unknown command", line("--yes= ", "nosuch"), emptyBoolRefused("--yes")},
		{"a blank bool value then the version flag", line("--yes= ", "--version"), emptyBoolRefused("--yes")},
		{"a blank bool value then an undefined flag", with(get, "--yes= ", "--bogus"), emptyBoolRefused("--yes")},
		{"help turned off before the dash", with(get, "--help=false", "-", "x"), droppedRefused("x")},
		{"the version flag then a dash and help", line("--version", "-", "--help"), versionOut},
		{"the version flag turned off then a dash", line("--version=false", "-", "get"), versionOut},
		{"an empty bool value then help", with(get, "--apply=", "--help"), helpCard(getPod)},
		{"a bool value as its own word then help", with(get, "--dry-run", "false", "--help"), helpCard(getPod)},
		{"a padded bool value", line("create", "user", "--dry-run", " false", "--password-stdin", "-o", "json"),
			refused("create user: ' false' after the bool flag --dry-run"+boolRule, "rancher create user --help")},
		{"a bool value with a tab", line("create", "user", "u04", "--dry-run", "false\t", "--password-stdin", "-o", "json"),
			refused("create user: 'false\t' after the bool flag --dry-run"+boolRule, "rancher create user 'u04' --dry-run='false' --password-stdin -o 'json'")},
		{"a padded extra word", line("create", "user", "u04", " extra", "--password-stdin", "-o", "json"),
			refused("create user takes 1 argument (<username>); unexpected argument ' extra'", "rancher create user --help")},
		{"a padded command name", line(" get", "pods"), unknownCommand(" get")},
		{"a dash and a symbol is an argument", with(get, "-€"), refused("--namespace is required to address a pod by name", "")},
		{"a dash and a symbol, a dash and a word", with(get, "-€", "-", "x"), refused("get pod takes at most 1 argument ([name]); unexpected arguments '-' 'x'", "rancher get pod --help")},
		{"an undefined flag after a bool value as its own word", with(get, "--dry-run", "false", "--bogus"), urfaveError("flag provided but not defined: -bogus")},
		{"an undefined flag then help after a bool value", with(get, "--dry-run", "false", "--bogus", "--help"), urfaveError("flag provided but not defined: -bogus")},
		{"help then an undefined flag", with(get, "--help", "--dry-run", "false", "--bogus"), helpCard(getPodErr)},
		{"a dash then help", with(get, "-", "--help"), droppedRefused("--help")},
		{"a dash, a word, then help", with(get, "-", "x", "--help"), droppedRefused("x", "--help")},
		{"the version flag then a dash after the command", with(append([]string{"--version"}, get...), "-", "x"), versionOut},
		{"help then a dash after the command", with(append([]string{"--help"}, get...), "-", "x"), helpCard(getPod)},
		{"a blank bool value then a dash and a word", with(get, "--yes= ", "-", "x"), droppedRefused("x")},
		{"a bool value as its own word then a dash and a word", with(get, "--dry-run", "false", "-", "x"), droppedRefused("x")},
		{"a bool value as its own word on a command that is not a mutation", line("schema", "--dry-run", "false"),
			refused("schema: 'false' after the bool flag --dry-run"+boolRule, "rancher schema --help")},
		{"a bool value as its own word and a format", with(get, "--dry-run", "false", "-o", "json"),
			refused("get pod: 'false' after the bool flag --dry-run"+boolRule, "rancher get pod --help")},
		{"a bool value as its own word and a format that does not exist", with(get, "--dry-run", "false", "-o", "bogus"),
			refused("get pod: 'false' after the bool flag --dry-run"+boolRule, "rancher get pod --help")},
		{"a dash after the hidden completion command", line("__completion", "-", "x"), unknownCommand("-")},
		{"a dash last is the name to create", line("create", "namespace", "--cluster", "local", "--apply", "-o", "json", " - "),
			lineWant{requests: []string{
				"POST /k8s/clusters/local/api/v1/namespaces?dryRun=All&fieldManager=cli.rancher.io",
				"POST /k8s/clusters/local/api/v1/namespaces?fieldManager=cli.rancher.io",
			}}},
		{"a padded name is looked up as typed", line("get", "pod", " web", "--cluster", "local", "-n", "ns", "-o", "json"),
			lineWant{requests: []string{"GET /k8s/clusters/local/api/v1/namespaces/ns/pods/%20web?"}}},
		{"two blank words then a dash and a word", line("", "", "-", "x"), unknownCommand("")},
		{"two blank words then a dash and a word at a group", line("get", "", "", "-", "x"), unknownCommand("")},
		{"a blank word then a dash and a word", line("", "-", "x"), unknownCommand("")},
		{"a padded command name after help", line("help", " get", "pods"), refused(`unknown command "help"`, "rancher --help")},
		{"two dashes ahead of the command, then an empty bool value and a format", line("create", "--", "namespace", "ns1", "--cluster", "local", "--apply=", "-o", "json"), emptyBoolRefused("--apply")},
		{"a flag value of two dashes, then an empty bool value and a format", line("create", "namespace", "ns1", "--cluster", "--", "-o", "json", "--apply="), emptyBoolRefused("--apply")},
		{"a blank bool value before an undefined flag and a format", line("create", "namespace", "ns1", "--cluster", "local", "--apply= ", "--bogus", "-o", "json"), emptyBoolRefused("--apply")},
		{"a blank bool value then help and an undefined flag, then a format", line("create", "namespace", "ns1", "--cluster", "local", "--apply= ", "--help", "--bogus", "-o", "json"), emptyBoolRefused("--apply")},
		{"a blank bool value then an unknown command and a format", line("--yes= ", "bogus", "x", "-o", "json"), emptyBoolRefused("--yes")},
		{"a bool value as its own word after the command name typed past two dashes", line("create", "--", "namespace", "ns1", "--cluster", "local", "--dry-run", "false", "-o", "json"),
			refused("create namespace: 'false' after the bool flag --dry-run"+boolRule, "rancher create -- namespace 'ns1' --cluster 'local' --dry-run='false' -o 'json'")},
		{"a dash on its own at the root", line("-"), unknownCommand("-")},
		{"a dash on its own at a group", line("get", "-"), unknownCommand("-")},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			rec.take()
			cmd, err := BuildRoot("v-test")
			require.NoError(t, err)
			stdout, stderr, code := runCaptured(t, cmd, row.argv)
			requests, _ := rec.take()
			assert.Equal(t, row.want.exit, code, "rancher %q: stdout %q, stderr %q", row.argv, stdout, stderr)
			assert.Equal(t, row.want.requests, requests, "rancher %q", row.argv)
			switch {
			case row.want.message != "":
				format := ""
				if slices.Contains(row.argv, "json") {
					format = "json"
				}
				ce := renderedError(t, format, stderr)
				assert.Equal(t, row.want.message, ce.Message)
				assert.Equal(t, row.want.hint, ce.Hint)
				assert.Empty(t, stdout)
			case row.want.usage != "":
				assert.Empty(t, stderr)
				m := regexp.MustCompile(`(?m)^USAGE:\n\s+(.*)$`).FindStringSubmatch(stdout)
				require.NotNil(t, m, "stdout: %s", stdout)
				assert.Equal(t, row.want.usage, m[1])
			case row.want.stdout != "":
				assert.Empty(t, stderr)
				assert.Regexp(t, row.want.stdout, stdout)
			}
		})
	}
}

// TestADashLastIsTheNameToCreateAsTyped asserts that a "-" with whitespace
// around it, typed last, is a positional argument used as typed: the create
// request carries the name with the whitespace. Not parallel: it sets the
// process environment and the captured command line.
func TestADashLastIsTheNameToCreateAsTyped(t *testing.T) {
	restoreRawArgv(t)
	rec := bodyRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runCaptured(t, root, []string{"create", "namespace", "--cluster", "local", "--apply", "-o", "json", " - "})
	require.Equal(t, 0, code, stderr)
	requests, bodies := rec.take()
	require.Len(t, requests, 2)
	for _, body := range bodies {
		assert.Contains(t, body, `"name":" - "`)
	}
}

// TestAValueFlagTakesABlankBoolWordAsItsValue asserts that a bool flag word
// with whitespace as its value, typed as the separate value of a value flag,
// is that flag's value, not a bool flag with a blank value: the command
// runs and looks the cluster up by that name, which the server does not
// have. Not parallel: it sets the process environment and the
// captured command line.
func TestAValueFlagTakesABlankBoolWordAsItsValue(t *testing.T) {
	restoreRawArgv(t)
	rec := bodyRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	_, stderr, code := runCaptured(t, root, []string{"get", "pods", "--cluster", "local", "--cluster", "--yes= ", "-o", "json"})
	assert.Equal(t, 4, code)
	ce := renderedError(t, "json", stderr)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster not found: '--yes= '; you can read no cluster on this server; a Rancher administrator grants cluster and project roles", ce.Message)
	assert.Empty(t, ce.Hint)
	requests, _ := rec.take()
	assert.Equal(t, []string{"GET /v1/management.cattle.io.clusters?"}, requests)
}

// TestAPaddedBoolValueIsRefusedBeforeThePasswordIsRead asserts that a bool
// value with whitespace around it, typed as its own word after a bool flag,
// is refused before the command reads the password from standard input, so
// no user is created. Not parallel: it sets the process environment and the
// captured command line.
func TestAPaddedBoolValueIsRefusedBeforeThePasswordIsRead(t *testing.T) {
	restoreRawArgv(t)
	rec := bodyRecorderEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	input := strings.NewReader("a-password\n")
	root.Reader = input
	assert.Equal(t, 2, runMain(t, root, "create", "user", "--yes", "--apply", " false", "--password-stdin", "-o", "json"))
	ce := renderedError(t, "json", errOut.String())
	assert.Equal(t, "create user: ' false' after the bool flag --apply"+boolRule, ce.Message)
	assert.Equal(t, "rancher create user --help", ce.Hint)
	assert.Equal(t, len("a-password\n"), input.Len(), "standard input is not read")
	requests, _ := rec.take()
	assert.Empty(t, requests)
}

// TestBoolRefusalsRenderInTheFormatAndColorTheLineSelects asserts that the
// refusals of an empty bool value and of a bool value typed as its own word
// render in the output format and color the line selects, read as urfave
// reads them: a "--" typed before the command name does not hide them, and
// neither does an error urfave reports before they are typed, nor a root or
// group line that ends at an unknown command, help or the version; for
// those the line scan finds them. Not parallel: it sets the process
// environment and the captured command line.
func TestBoolRefusalsRenderInTheFormatAndColorTheLineSelects(t *testing.T) {
	restoreRawArgv(t)
	bodyRecorderEnv(t)
	ansi := regexp.MustCompile("\x1b\\[[0-9;]*m")
	const create = "create"
	base := []string{create, "--", "namespace", "ns1", "--cluster", "local"}
	for _, tc := range []struct {
		name    string
		argv    []string
		format  string
		styled  bool
		message string
	}{
		{"a bool value in JSON", append(slices.Clone(base), "--dry-run", "false", "-o", "json"), "json", false, "create namespace: 'false' after the bool flag --dry-run" + boolRule},
		{"a bool value in YAML", append(slices.Clone(base), "--dry-run", "false", "-o", "yaml"), "yaml", false, "create namespace: 'false' after the bool flag --dry-run" + boolRule},
		{"a bool value in color", append(slices.Clone(base), "--dry-run", "false", "--color", "always"), "", true, "create namespace: 'false' after the bool flag --dry-run" + boolRule},
		{"a bool value without color", append(slices.Clone(base), "--dry-run", "false", "--color", "always", "--color", "never"), "", false, "create namespace: 'false' after the bool flag --dry-run" + boolRule},
		{"an empty bool value in YAML", append(slices.Clone(base), "--apply=", "-o", "yaml"), "yaml", false, "--apply= has an empty value; a bool flag typed with = takes true or false"},
		{"an empty bool value in color", append(slices.Clone(base), "--apply=", "--color", "always"), "", true, "--apply= has an empty value; a bool flag typed with = takes true or false"},
		{"an empty bool value without color", append(slices.Clone(base), "--apply=", "--color=always", "--color=never"), "", false, "--apply= has an empty value; a bool flag typed with = takes true or false"},
		{"a blank bool value before an unknown command, in JSON", []string{"--yes= ", "bogus", "x", "-o", "json"}, "json", false, "--yes= has an empty value; a bool flag typed with = takes true or false"},
		{"a blank bool value before an unknown command at a group, in YAML", []string{"get", "--yes= ", "bogus", "-o", "yaml"}, "yaml", false, "--yes= has an empty value; a bool flag typed with = takes true or false"},
		{"a blank bool value before an unknown command, in color", []string{"--yes= ", "bogus", "x", "--color", "always"}, "", true, "--yes= has an empty value; a bool flag typed with = takes true or false"},
		{"a blank bool value before an error, in JSON", []string{create, "namespace", "ns1", "--cluster", "local", "--apply= ", "--bogus", "-o", "json"}, "json", false, "--apply= has an empty value; a bool flag typed with = takes true or false"},
		{"a blank bool value before an error, in color", []string{create, "namespace", "ns1", "--cluster", "local", "--apply= ", "--bogus", "--color", "always"}, "", true, "--apply= has an empty value; a bool flag typed with = takes true or false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			_, stderr, code := runCaptured(t, root, tc.argv)
			assert.Equal(t, 2, code)
			assert.Equal(t, tc.styled, strings.Contains(stderr, "\x1b["), "styled: %q", stderr)
			ce := renderedBareDashError(t, tc.format, ansi.ReplaceAllString(stderr, ""))
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
		})
	}
}
