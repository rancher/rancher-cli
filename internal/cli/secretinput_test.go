package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"
)

// secretCanary is the value every row of the secret input table feeds a
// command in a position that holds a secret: one line, no spaces, letters
// and digits, so no renderer escapes or splits it.
const secretCanary = "s3cretcanary9"

// shortSecretCanary is a canary shorter than the 12 characters a password
// needs, for the rows where the server refuses a short password.
const shortSecretCanary = "s3cretcnry9"

// canaryForms returns canary as every renderer could show it: as typed, and
// in each of the three base64 alignments (the text after zero, one or two
// bytes of other text), cut to the characters the neighbor bytes do not
// change. The renderers do not HTML-escape, so a JSON-escaped form adds
// nothing for a canary of letters and digits.
func canaryForms(canary string) []string {
	forms := []string{canary}
	for pad := range 3 {
		enc := strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", pad)+canary)), "=")
		start := (8*pad + 5) / 6
		end := 8 * (pad + len(canary)) / 6
		forms = append(forms, enc[start:end])
	}
	return forms
}

// TestCanaryFormsFindTheCanaryInAnyAlignment asserts that the base64 forms
// of the canary are found inside the base64 text of a larger value at every
// alignment, so an absence check on them can fail.
func TestCanaryFormsFindTheCanaryInAnyAlignment(t *testing.T) {
	t.Parallel()
	forms := canaryForms(secretCanary)
	require.Len(t, forms, 4)
	for pad := range 6 {
		enc := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("-", pad) + secretCanary + "tail"))
		found := false
		for _, f := range forms[1:] {
			found = found || strings.Contains(enc, f)
		}
		assert.True(t, found, "pad %d: %s", pad, enc)
	}
}

// secretRow is one row of the secret input table: a command line with one
// malformed secret-bearing input holding the canary. A row names the flag or
// the environment variable it feeds; TestEveryRowNamesAFlagOrAnEnvironmentVariable
// asserts none leaves both empty.
type secretRow struct {
	// flag is the name of the flag whose input the row feeds, or env the
	// name of the environment variable it sets; the scans require a row
	// for every secret-bearing flag and variable. A row with neither feeds
	// another input that may hold a secret.
	flag string
	env  string
	name string
	// setEnv is set in the environment before the run.
	setEnv map[string]string
	// before are command lines run first, each of which must succeed.
	before [][]string
	// argv is the command line. {url} and {pem} stand for the stub
	// server's address and its certificate.
	argv []string
	// stdin is what the command reads from standard input.
	stdin string
	// files are written to the working directory before the run.
	files map[string]string
	// stub answers the requests it takes; the stub Rancher answers the rest.
	stub secretStub
	// canary is the secret the row asserts absent; empty means secretCanary.
	canary string
	// code and message are the error the row must produce: its code and a
	// text its message holds, so a row cannot pass by failing elsewhere.
	code    string
	message string
	// hint, when set, is the text the error's hint must equal.
	hint string
}

// secretBearer is the token the stub server knows; the row runs use it as
// RANCHER_CLI_TOKEN. Any other token is refused with 401.
const secretBearer = "u-member:secret"

// secretStub is a server's answer in front of the stub Rancher: it writes
// the answer and returns true, or returns false to let the stub Rancher
// answer.
type secretStub func(w http.ResponseWriter, r *http.Request) bool

// writeKubeJSON writes body as a JSON answer with status.
func writeKubeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const secretConfigMap = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default","resourceVersion":"7"},"data":{"a":"1"}}`

// configMapStub answers the get of the config map "app" with the object.
func configMapStub(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/configmaps/app") {
		writeKubeJSON(w, http.StatusOK, secretConfigMap)
		return true
	}
	return false
}

// unauthorizedStub answers every request with 401, as a server does for a
// token it does not know.
func unauthorizedStub(w http.ResponseWriter, _ *http.Request) bool {
	writeKubeJSON(w, http.StatusUnauthorized, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
	return true
}

// shortPasswordStub serves the ext discovery of a server that has the
// password-change-request resource, and answers every create the way a
// server answers a password it refuses as too short: 422 with the length
// rule, which names no value.
func shortPasswordStub(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost:
		writeKubeJSON(w, http.StatusUnprocessableEntity, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"password must be at least 12 characters","reason":"Invalid","code":422}`)
	case r.URL.Path == "/apis/ext.cattle.io":
		writeKubeJSON(w, http.StatusOK, `{"kind":"APIGroup","apiVersion":"v1","name":"ext.cattle.io","versions":[{"groupVersion":"ext.cattle.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ext.cattle.io/v1","version":"v1"}}`)
	case r.URL.Path == "/k8s/clusters/local/apis":
		writeKubeJSON(w, http.StatusOK, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"ext.cattle.io","versions":[{"groupVersion":"ext.cattle.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ext.cattle.io/v1","version":"v1"}}]}`)
	case r.URL.Path == "/apis/ext.cattle.io/v1", r.URL.Path == "/k8s/clusters/local/apis/ext.cattle.io/v1":
		writeKubeJSON(w, http.StatusOK, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"ext.cattle.io/v1","resources":[{"name":"passwordchangerequests","singularName":"passwordchangerequest","namespaced":false,"kind":"PasswordChangeRequest","verbs":["create"]}]}`)
	default:
		return false
	}
	return true
}

// secretManifest is a Secret manifest whose value field is value.
func secretManifest(value string) string {
	return "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nstringData:\n  password: " + value + "\n"
}

const (
	createSecretLine = "create secret s --cluster c-x --namespace d --dry-run"
	createUserLine   = "create user bob --dry-run"
	passwordLine     = "create password-change-request u-abc --dry-run"
	machineConfig    = "create machine-config amazonec2config w --dry-run"
	patchLine        = "patch configmap app --cluster c-x --namespace default --dry-run"
	createFileLine   = "create resource --cluster c-x --namespace n2 --dry-run"
	loginLine        = "auth login x --url {url} --cacert {pem}"
)

// secretRows is the table: one or more rows for every secret-bearing flag
// of every command, each feeding the flag a malformed value that holds the
// canary where a secret goes.
func secretRows() []secretRow {
	c := secretCanary
	var rows []secretRow
	add := func(r ...secretRow) { rows = append(rows, r...) }
	line := func(l string, more ...string) []string { return append(strings.Fields(l), more...) }
	const validation = string(clierr.CodeValidation)

	add(
		secretRow{flag: "from-literal", name: "no equals", argv: line(createSecretLine, "--from-literal", c), code: validation, message: "malformed --from-literal value"},
		secretRow{flag: "from-literal", name: "empty key", argv: line(createSecretLine, "--from-literal", "="+c), code: validation, message: "malformed --from-literal value"},
		secretRow{flag: "from-literal", name: "the second half of a value with a comma", argv: line(createSecretLine, "--from-literal", "pw=ab,"+c), code: validation, message: "malformed --from-literal value"},
		secretRow{flag: "from-literal", name: "a flag given twice", argv: line(createSecretLine, "--from-literal", "a=b", "--from-literal", "k="+c), code: validation, message: "invalid value for flag -from-literal: can't duplicate this flag"},
		secretRow{flag: "from-literal", name: "an extra argument", argv: line(createSecretLine, "--from-literal", "a=b", c), code: validation, message: "unexpected argument 2"},
	)

	bad := map[string]string{
		"a scalar that does not fit its tag": secretManifest("!!int " + c),
		"an unknown alias":                   secretManifest("*" + c),
		"a map key that is a map":            "apiVersion: v1\nkind: Secret\nstringData:\n  ? {a: " + c + "}\n  : c\n",
		"a kind the CLI does not know":       "apiVersion: v1\nkind: Widget\nmetadata:\n  name: x\nstringData:\n  password: " + c + "\n",
		"a namespace the flag contradicts":   "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  namespace: n1\nstringData:\n  password: " + c + "\n",
	}
	wantMessage := map[string]string{
		"a scalar that does not fit its tag": "invalid manifest: the YAML could not be decoded",
		"an unknown alias":                   "invalid manifest: the YAML could not be decoded",
		"a map key that is a map":            "invalid manifest: the YAML could not be decoded",
		"a kind the CLI does not know":       "unsupported manifest kind v1/Widget",
		"a namespace the flag contradicts":   "conflicts with the manifest namespace",
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range bad {
			if !yield(k) {
				return
			}
		}
	}) {
		add(
			secretRow{flag: "filename", name: name + " in a file", argv: line(createFileLine, "-f", "m.yaml"), files: map[string]string{"m.yaml": bad[name]}, code: validation, message: wantMessage[name]},
			secretRow{flag: "filename", name: name + " on stdin", argv: line(createFileLine, "-f", "-"), stdin: bad[name], code: validation, message: wantMessage[name]},
		)
	}
	add(
		secretRow{flag: "filename", name: "an extra argument", argv: line(createFileLine, "-f", "m.yaml", c), files: map[string]string{"m.yaml": secretManifest("x")}, code: validation, message: "unexpected argument 1"},
	)

	malformedPatch := `[{"op":"add","path":"/data/a"},{"op":"add","path":"/data/b","value":"` + c + `"}]`
	failingTest := `[{"op":"test","path":"/data/a","value":"` + c + `"}]`
	notJSON := `[{"op":"add","path":"/data/a","value":"` + c + `"`
	add(
		secretRow{flag: "patch", name: "a malformed op ahead of an op holding the canary", argv: line(patchLine, "-p", malformedPatch), code: validation, message: `patch operation 0 (add) has no "value"`},
		secretRow{flag: "patch", name: "a test op that fails", argv: line(patchLine, "-p", failingTest), stub: configMapStub, code: validation, message: "testing value /data/a failed"},
		secretRow{flag: "patch", name: "a patch that is not JSON", argv: line(patchLine, "-p", notJSON), code: validation, message: "not valid JSON"},
		secretRow{flag: "patch", name: "an extra argument", argv: line(patchLine, "-p", "[]", c), code: validation, message: "unexpected argument 2"},
		secretRow{flag: "patch-file", name: "a malformed op ahead of an op holding the canary", argv: line(patchLine, "--patch-file", "-"), stdin: malformedPatch, code: validation, message: `patch operation 0 (add) has no "value"`},
		secretRow{flag: "patch-file", name: "a test op that fails", argv: line(patchLine, "--patch-file", "-"), stdin: failingTest, stub: configMapStub, code: validation, message: "testing value /data/a failed"},
		secretRow{flag: "patch-file", name: "a patch that is not JSON", argv: line(patchLine, "--patch-file", "-"), stdin: notJSON, code: validation, message: "not valid JSON"},
	)

	add(
		secretRow{flag: "set", name: "no equals", argv: line(machineConfig, "--set", c), code: validation, message: "--set argument #1 must be <field>=<value>"},
		secretRow{flag: "set", name: "empty field name", argv: line(machineConfig, "--set", "="+c), code: validation, message: "--set argument #1 has an empty field name"},
		secretRow{flag: "set", name: "a credential field", argv: line(machineConfig, "--set", "secretKey="+c), code: validation, message: "holds a credential"},
		secretRow{flag: "set", name: "a top-level object field", argv: line(machineConfig, "--set", "kind="+c), code: validation, message: "names a top-level Kubernetes object field"},
		secretRow{flag: "set", name: "a field given twice", argv: line(machineConfig, "--set", "a=b", "--set", "a="+c), code: validation, message: "is given more than once"},
		secretRow{flag: "set", name: "an extra argument", argv: line(machineConfig, "--set", "a=b", c), code: validation, message: "unexpected argument 3"},
	)

	for _, cmd := range []struct{ line, extra string }{{createUserLine, "unexpected argument 2"}, {passwordLine, "unexpected argument 2"}} {
		add(
			secretRow{flag: "password-stdin", name: cmd.line + ": an empty password", argv: line(cmd.line, "--password-stdin"), code: validation, message: "produced an empty password"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the flag without the pipe", argv: line(cmd.line), stdin: c, code: validation, message: "--password-stdin is required"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed after the flag with =", argv: line(cmd.line, "--password-stdin="+c), code: validation, message: "invalid value for flag -password-stdin: parse error"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed as an argument", argv: line(cmd.line, "--password-stdin", c), code: validation, message: cmd.extra},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed after a flag that does not exist", argv: line(cmd.line, "--password", c), code: validation, message: "unknown flag at word 7 after rancher"},
		)
	}
	short := shortSecretCanary
	add(
		secretRow{flag: "password-stdin", name: createUserLine + ": a password the server refuses as too short", argv: line(createUserLine, "--password-stdin"), stdin: short, stub: shortPasswordStub, canary: short, code: validation, message: "password must be at least 12 characters"},
		secretRow{flag: "password-stdin", name: passwordLine + ": a password the server refuses as too short", argv: line(passwordLine, "--password-stdin"), stdin: short, stub: shortPasswordStub, canary: short, code: validation, message: "password must be at least 12 characters"},
	)

	add(
		secretRow{flag: "token-stdin", name: "a token with no colon", argv: line(loginLine, "--token-stdin"), stdin: c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "a token with no id", argv: line(loginLine, "--token-stdin"), stdin: ":" + c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "a token the server refuses", argv: line(loginLine, "--token-stdin"), stdin: "id:" + c, code: string(clierr.CodeAuth), message: "the server rejected the credential"},
		secretRow{flag: "token-stdin", name: "a token with no colon, not verified", argv: line(loginLine, "--token-stdin", "--no-verify"), stdin: c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "the token typed after the flag with =", argv: line(loginLine, "--token-stdin="+c), code: validation, message: "invalid value for flag -token-stdin: parse error"},
		secretRow{flag: "token-stdin", name: "the token typed as an argument", argv: line(loginLine, "--token-stdin", c), code: validation, message: "unexpected argument 2"},
	)

	for _, token := range []string{c, "id:" + c, "id:" + c + " x"} {
		add(secretRow{env: "RANCHER_CLI_TOKEN", name: "a token the server refuses: " + strings.ReplaceAll(token, c, "canary"), argv: line("get cluster"), setEnv: map[string]string{config.EnvToken: token}, stub: unauthorizedStub, code: string(clierr.CodeAuth), message: "the server rejected the credential"})
	}

	const badPort = "@rancher.invalid:abc"
	redactedBadPort := "invalid server URL: https://<redacted>" + badPort
	for _, cmd := range []struct{ name, line string }{
		{"config add-server", "config add-server x --url"},
		{"auth login", "auth login x --token-stdin --url"},
	} {
		stdin := ""
		if cmd.name == "auth login" {
			stdin = "id:y"
		}
		add(
			secretRow{flag: "url", name: cmd.name + ": a URL with a password and a bad port", argv: line(cmd.line, "https://u:"+c+badPort), stdin: stdin, code: validation, message: redactedBadPort},
			secretRow{flag: "url", name: cmd.name + ": a URL with a password and an unsupported scheme", argv: line(cmd.line, "ftp://u:"+c+"@rancher.invalid"), stdin: stdin, code: validation, message: "unsupported server URL scheme: ftp://<redacted>@rancher.invalid"},
		)
	}
	// The password holds a character that ends an authority, so the part of
	// it after that character is the canary.
	for _, sep := range []string{"/", "?", "#", "@"} {
		add(secretRow{flag: "url", name: "config add-server: a password with " + sep, argv: line("config add-server x --url", "https://u:s3cret"+sep+"canary9"+badPort), canary: "canary9", code: validation, message: redactedBadPort})
	}
	updateBefore := [][]string{line("config add-server x --url {url} --cacert {pem}")}
	add(
		secretRow{flag: "url", name: "config update-server: a URL with a password and a bad port", argv: line("config update-server x --url", "https://u:"+c+badPort), before: updateBefore, code: validation, message: redactedBadPort},
		secretRow{flag: "url", name: "config update-server: a URL with a password and an unsupported scheme", argv: line("config update-server x --url", "ftp://u:"+c+"@rancher.invalid"), before: updateBefore, code: validation, message: "unsupported server URL scheme: ftp://<redacted>@rancher.invalid"},
		secretRow{flag: "url", name: "config update-server: a URL with a password typed as an argument", argv: line("config update-server x", "https://u:"+c+"@rancher.invalid"), before: updateBefore, code: validation, message: "unexpected argument 2"},
		secretRow{flag: "url", name: "config add-server: a URL with a password typed as an argument", argv: line("config add-server x --url {url}", "https://u:"+c+"@rancher.invalid"), code: validation, message: "unexpected argument 2"},
		secretRow{flag: "url", name: "auth login: a URL with a password typed as an argument", argv: line("auth login x --token-stdin", "https://u:"+c+"@rancher.invalid"), stdin: "id:y", code: validation, message: "unexpected argument 2"},
	)

	// A bool flag followed by a word that reads as a bool is refused; the
	// hint of a command with a secret input is its help, never the line
	// typed, which holds the other flags' values.
	add(
		secretRow{flag: "from-literal", name: "a bool value refusal does not repeat the flag values", argv: line("create secret s --cluster c-x --namespace d --from-literal", "pw="+c, "--dry-run", "false"), code: validation, message: "create secret: argument 2 after the bool flag --dry-run", hint: "rancher create secret --help"},
		secretRow{flag: "patch", name: "a bool value refusal does not repeat the flag values", argv: line("patch configmap app --cluster c-x --namespace default -p", `[{"op":"add","path":"/data/b","value":"`+c+`"}]`, "--dry-run", "false"), code: validation, message: "patch configmap: argument 2 after the bool flag --dry-run", hint: "rancher patch configmap --help"},
		secretRow{flag: "set", name: "a bool value refusal does not repeat the flag values", argv: line("create machine-config amazonec2config w --set", "a="+c, "--dry-run", "false"), code: validation, message: "create machine-config: argument 3 after the bool flag --dry-run", hint: "rancher create machine-config --help"},
		secretRow{flag: "url", name: "a bool value refusal does not repeat the flag values", argv: line("auth login x --token-stdin --url", "https://u:"+c+"@rancher.invalid", "--no-verify", "false"), stdin: "id:y", code: validation, message: "auth login: argument 2 after the bool flag --no-verify", hint: "rancher auth login --help"},
		secretRow{flag: "password-stdin", name: "a word that reads as a bool is named by position", argv: line("create user bob --password-stdin --dry-run TRUE"), canary: "TRUE", code: validation, message: "create user: argument 2 after the bool flag --dry-run", hint: "rancher create user --help"},
		secretRow{flag: "password-stdin", name: "a word after a bare dash is counted, not named", argv: line("create user bob --password-stdin -", c), code: validation, message: "1 word after a - on its own is not used"},
		secretRow{flag: "from-literal", name: "words after a bare dash are counted, not named", argv: line("create secret s --cluster c-x --namespace d -", "--from-literal", "pw="+c), code: validation, message: "2 words after a - on its own are not used"},
		secretRow{flag: "password-stdin", name: "a password typed with a leading dash is not named", argv: line("create user bob --password-stdin", "-"+c), code: validation, message: "unknown flag at word 7 after rancher"},
		secretRow{flag: "token-stdin", name: "a token typed with a leading dash is not named", argv: line("auth login x --token-stdin", "-"+c), code: validation, message: "unknown flag at word 7 after rancher"},
		secretRow{flag: "password-stdin", name: "words after a bare dash at a group level are counted, not named", argv: line("create -", c, "--password-stdin"), code: validation, message: "2 words after a - on its own are not used"},
		secretRow{flag: "password-stdin", name: "words after a bare dash at the root are counted, not named", argv: line("-", c), code: validation, message: "1 word after a - on its own is not used"},
		secretRow{flag: "from-literal", name: "a flag typed without its value is named, since the command defines it", argv: line(createSecretLine, "--from-literal"), code: validation, message: "flag needs an argument: --from-literal"},
		secretRow{flag: "password-stdin", name: "a password typed after a padded flag word gives no position", argv: line("create user bob --password-stdin", " -"+c), code: validation, message: "unknown flag"},
		secretRow{flag: "password-stdin", name: "a password typed after three dashes gives no word", argv: line("create user bob --password-stdin", "---"+c), code: validation, message: "unknown flag"},
		secretRow{flag: "password-stdin", name: "an unknown flag typed at a group before the command name is named by position", argv: line("create -" + c + " user bob --password-stdin"), code: validation, message: "unknown flag at word 4 after rancher"},
		secretRow{flag: "password-stdin", name: "an unknown flag typed before the group name is named by position", argv: line("-" + c + " create user bob --password-stdin"), code: validation, message: "unknown flag at word 3 after rancher"},
		secretRow{flag: "url", name: "an unknown flag typed before a command with a secret input is named by position", argv: line("-" + c + " config add-server"), code: validation, message: "unknown flag at word 3 after rancher"},
		secretRow{flag: "password-stdin", name: "a value for a flag typed before the group name is not named", argv: line("--debug=" + c + " create user bob --password-stdin"), code: validation, message: "invalid value for flag -debug: parse error"},
		secretRow{flag: "password-stdin", name: "a value for a flag typed at a group is not named", argv: line("create --debug=" + c + " user bob --password-stdin"), code: validation, message: "invalid value for flag -debug: parse error"},
		secretRow{flag: "from-literal", name: "a --color value that is the canary is not named", argv: line(createSecretLine, "--from-literal", "a=b", "--color", c), code: validation, message: "--color value is not one of auto|always|never"},
		secretRow{flag: "password-stdin", name: "an enum flag given the canary is not named", argv: line(createUserLine, "--password-stdin", "--enabled", c), code: validation, message: "--enabled value is not one of true|false"},
		secretRow{flag: "patch", name: "the patch type given the canary is not named", argv: line(patchLine, "-p", "[]", "--type", c), code: validation, message: "--type value is not one of json|merge"},
		secretRow{flag: "token-stdin", name: "an enum flag of auth login given the canary is not named", argv: line(loginLine, "--token-stdin", "--auth-flow", c), stdin: "id:y", code: validation, message: "--auth-flow value is not one of devicecode|authcode"},
		secretRow{flag: "set", name: "a machine config kind that is the canary is not named", argv: line("create machine-config", c, "x", "--dry-run"), code: validation, message: "argument <kind> value is not one of"},
		secretRow{flag: "from-literal", name: "a flag typed with no value takes --from-literal=<canary> as its value: --cluster", argv: line("create secret s1 --cluster", "--from-literal=k="+c), code: validation, message: "flag --cluster got flag --from-literal as its value"},
		secretRow{flag: "from-literal", name: "a flag typed with no value takes --from-literal=<canary> as its value: --namespace", argv: line("create secret s1 --namespace", "--from-literal=k="+c), code: validation, message: "flag --namespace got flag --from-literal as its value"},
		secretRow{flag: "from-literal", name: "a flag typed with no value takes --from-literal=<canary> as its value: --project", argv: line("create secret s1 --project", "--from-literal=k="+c), code: validation, message: "flag --project got flag --from-literal as its value"},
		secretRow{flag: "patch", name: "an unknown op names the op field only: the op holds a verb, never a value", argv: line(patchLine, "-p", `[{"op":"`+"bogus"+`","path":"/data/a","value":"`+c+`"}]`), code: validation, message: `patch operation 0 has the unknown operation "bogus"`},
	)
	return rows
}

// secretFlagPattern matches the names of the flags that may take a secret
// value, or a file or stdin that holds one.
var secretFlagPattern = regexp.MustCompile(`secret|password|token|credential|literal|set|patch|stdin|key|file|filename|url|server`)

// secretExempt names the flags the pattern matches whose value is never a
// secret, with the reason.
var secretExempt = map[string]string{
	"cloud-credential":     "the name of a cloud credential, never its content",
	"credential-store":     "the name of a credential store backend",
	"must-change-password": "a bool",
	"output-file":          "a path the command writes to",
	"token-ttl":            "a duration",
	"non-resource-url":     "a request path of the Kubernetes API on the server, never a URL with userinfo",
	"server":               serverURLNotRedacted,
}

// serverURLNotRedacted is the reason of the exempt entries for the paths
// that show a server URL with its userinfo.
const serverURLNotRedacted = "the server URL's userinfo is not yet redacted on this path (internal/token, internal/client)"

// secretNameGaps returns one problem per name in uses that pattern matches
// and neither rows nor exempt covers, one per row or exempt entry that
// matches no name in uses, one per exempt entry with no reason, and one per
// name with both a row and an exempt entry. kind and dashes spell a name in
// a problem: "flag" with "--", "environment variable" with "".
func secretNameGaps(kind, dashes string, uses []nameUse, pattern *regexp.Regexp, rows map[string]bool, exempt map[string]string) []string {
	seen := map[string]bool{}
	var problems []string
	for _, u := range uses {
		seen[u.name] = true
		if _, exempted := exempt[u.name]; pattern.MatchString(u.name) && !rows[u.name] && !exempted {
			problems = append(problems, kind+" "+dashes+u.name+" of "+u.owner+" has no row and no exempt entry")
		}
	}
	for name := range rows {
		if !seen[name] {
			problems = append(problems, "the table has a row for "+kind+" "+dashes+name+", which nothing has")
		}
	}
	for name, reason := range exempt {
		switch {
		case !seen[name]:
			problems = append(problems, "the exempt list has "+kind+" "+dashes+name+", which nothing has")
		case reason == "":
			problems = append(problems, "the exempt entry for "+kind+" "+dashes+name+" has no reason")
		case rows[name]:
			problems = append(problems, kind+" "+dashes+name+" has a row and an exempt entry")
		}
	}
	slices.Sort(problems)
	return problems
}

// nameUse is a flag or environment variable and the command or list that
// has it.
type nameUse struct{ name, owner string }

// secretFlagGaps is secretNameGaps over the flags of every command of m.
func secretFlagGaps(m runtime.Manifest, rows map[string]bool, exempt map[string]string) []string {
	var uses []nameUse
	for _, c := range m.Commands {
		for _, f := range c.Flags {
			uses = append(uses, nameUse{f.Name, c.Name})
		}
	}
	return secretNameGaps("flag", "--", uses, secretFlagPattern, rows, exempt)
}

// secretEnvPattern matches the names of the environment variables that may
// hold a secret.
var secretEnvPattern = regexp.MustCompile(`(?i)secret|password|token|credential|key|url`)

// secretEnvExempt names the environment variables the pattern matches whose
// value is never a secret, with the reason.
var secretEnvExempt = map[string]string{
	"RANCHER_CLI_CREDENTIAL_STORE": "the name of a credential store backend",
	"RANCHER_CLI_URL":              serverURLNotRedacted,
}

// secretEnvGaps is secretNameGaps over the environment variables vars.
func secretEnvGaps(vars []runtime.EnvVar, rows map[string]bool, exempt map[string]string) []string {
	uses := make([]nameUse, len(vars))
	for i, v := range vars {
		uses[i] = nameUse{v.Name, "the environment list"}
	}
	return secretNameGaps("environment variable", "", uses, secretEnvPattern, rows, exempt)
}

// secretInputGaps returns one problem per command in cmds that has a flag
// with a row and does not set SecretInput, and one per command that sets it
// with no such flag.
func secretInputGaps(cmds []runtime.Descriptor, rows map[string]bool) []string {
	var problems []string
	for _, d := range cmds {
		has := false
		for _, f := range d.Flags {
			has = has || rows[f.Name]
		}
		switch {
		case has && !d.SecretInput:
			problems = append(problems, d.Name+" takes a secret input and does not set SecretInput")
		case !has && d.SecretInput:
			problems = append(problems, d.Name+" sets SecretInput and has no flag with a row")
		}
	}
	slices.Sort(problems)
	return problems
}

func rowFlags(rows []secretRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.flag != "" {
			out[r.flag] = true
		}
	}
	return out
}

func rowEnvs(rows []secretRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.env != "" {
			out[r.env] = true
		}
	}
	return out
}

// TestEverySecretFlagHasARowOrAnExemptEntry asserts that every flag of the
// built command tree whose name may hold a secret has a row in the table or
// an exempt entry with a reason, and that no row or entry is stale.
func TestEverySecretFlagHasARowOrAnExemptEntry(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Empty(t, secretFlagGaps(m, rowFlags(secretRows()), secretExempt))
}

// TestSecretFlagScanFailsOnAFlagWithNoRow asserts that the scan names a flag
// that matches the pattern and has no row or exempt entry, a row for a flag
// no command has, an exempt entry for a flag no command has, an exempt entry
// with no reason, and a flag with both.
func TestSecretFlagScanFailsOnAFlagWithNoRow(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	rows := rowFlags(secretRows())

	withFlag := m
	withFlag.Commands = slices.Clone(m.Commands)
	withFlag.Commands[0].Flags = append(slices.Clone(withFlag.Commands[0].Flags), runtime.ArgSpec{Name: "secret-x", Type: "string"})
	problems := secretFlagGaps(withFlag, rows, secretExempt)
	require.Len(t, problems, 1)
	assert.Equal(t, "flag --secret-x of "+m.Commands[0].Name+" has no row and no exempt entry", problems[0])

	staleRows := maps.Clone(rows)
	staleRows["gone-flag"] = true
	assert.Equal(t, []string{"the table has a row for flag --gone-flag, which nothing has"}, secretFlagGaps(m, staleRows, secretExempt))

	staleExempt := maps.Clone(secretExempt)
	staleExempt["gone-flag"] = "a flag that no longer exists"
	assert.Equal(t, []string{"the exempt list has flag --gone-flag, which nothing has"}, secretFlagGaps(m, rows, staleExempt))

	noReason := maps.Clone(secretExempt)
	noReason["token-ttl"] = ""
	assert.Equal(t, []string{"the exempt entry for flag --token-ttl has no reason"}, secretFlagGaps(m, rows, noReason))

	both := maps.Clone(secretExempt)
	both["patch"] = "a flag that also has a row"
	assert.Equal(t, []string{"flag --patch has a row and an exempt entry"}, secretFlagGaps(m, rows, both))

	missing := maps.Clone(rows)
	delete(missing, "set")
	problems = secretFlagGaps(m, missing, secretExempt)
	require.Len(t, problems, 1)
	assert.Equal(t, "flag --set of create machine-config has no row and no exempt entry", problems[0])
}

// TestEverySecretEnvironmentVariableHasARowOrAnExemptEntry asserts that
// every environment variable of the schema whose name may hold a secret has
// a row in the table or an exempt entry with a reason, and that no row or
// entry is stale.
func TestEverySecretEnvironmentVariableHasARowOrAnExemptEntry(t *testing.T) {
	t.Parallel()
	rows := rowEnvs(secretRows())
	assert.Empty(t, secretEnvGaps(runtime.EnvVars(), rows, secretEnvExempt))

	vars := append(slices.Clone(runtime.EnvVars()), runtime.EnvVar{Name: "RANCHER_CLI_SECRET_X"})
	assert.Equal(t, []string{"environment variable RANCHER_CLI_SECRET_X of the environment list has no row and no exempt entry"},
		secretEnvGaps(vars, rows, secretEnvExempt))
	stale := maps.Clone(rows)
	stale["GONE_TOKEN"] = true
	assert.Equal(t, []string{"the table has a row for environment variable GONE_TOKEN, which nothing has"},
		secretEnvGaps(runtime.EnvVars(), stale, secretEnvExempt))
}

// TestEveryRowNamesAFlagOrAnEnvironmentVariable asserts that no row of the
// table leaves both its flag and its environment variable empty, so the
// marker check, which reads the flags the rows name, sees every command a
// row feeds.
func TestEveryRowNamesAFlagOrAnEnvironmentVariable(t *testing.T) {
	t.Parallel()
	for _, row := range secretRows() {
		assert.True(t, row.flag != "" || row.env != "", "the row %q names neither", row.name)
	}
}

// TestSecretInputIsSetOnEveryCommandWithARowAndOnlyThere asserts both ways
// that SecretInput, which makes a command name its arguments by position,
// is set on every command that has a flag with a row in the table, and on no
// other command.
func TestSecretInputIsSetOnEveryCommandWithARowAndOnlyThere(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	var descriptors []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		descriptors = append(descriptors, c.Describe())
	}
	rows := rowFlags(secretRows())
	assert.Empty(t, secretInputGaps(descriptors, rows))

	unmarked := slices.Clone(descriptors)
	for i := range unmarked {
		if unmarked[i].Name == "create user" {
			unmarked[i].SecretInput = false
		}
		if unmarked[i].Name == "config path" {
			unmarked[i].SecretInput = true
		}
	}
	assert.Equal(t, []string{
		"config path sets SecretInput and has no flag with a row",
		"create user takes a secret input and does not set SecretInput",
	}, secretInputGaps(unmarked, rows))
}

// secretServer starts a TLS server answering with h, points the process at
// it with an empty config directory, the file credential store and a
// temporary working directory, and returns it. Nothing reaches a real
// server. Not for a parallel test: it sets the process environment.
func secretServer(t *testing.T, stub secretStub, token string) *httptest.Server {
	t.Helper()
	keyring.MockInit()
	rancher := testrancher.New(testrancher.Options{
		SeedToken:          secretBearer,
		SelfSubjectReviews: true,
		ClusterVisibility: func(b string) ([]testrancher.ClusterFixture, bool) {
			return []testrancher.ClusterFixture{{ID: "c-x", DisplayName: "c-x"}}, b == secretBearer
		},
		ProjectVisibility: func(context.Context, string, string) ([]testrancher.ProjectFixture, bool, error) {
			return nil, true, nil
		},
		NamespaceVisibility: func(string, string) ([]testrancher.NamespaceFixture, bool) { return nil, true },
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeKubeJSON(w, http.StatusOK, `{"items":[]}`)
		}),
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("request: %s %s", r.Method, r.URL.RequestURI())
		if stub != nil && stub(w, r) {
			return
		}
		rancher.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(config.EnvConfigDir, t.TempDir())
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvURL, srv.URL)
	t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
	t.Setenv(config.EnvToken, token)
	t.Setenv("EDITOR", "rancher-test-no-such-editor")
	t.Setenv("VISUAL", "rancher-test-no-such-editor")
	t.Chdir(t.TempDir())
	return srv
}

// runWithStdin runs argv through a new root as the process entry point does
// (Run, which refuses a line with words after a bare "-" or a bool value
// typed as its own word before urfave parses it), with stdin as the
// command's standard input and a time bound, and returns stdout, stderr and the exit
// code.
func runWithStdin(t *testing.T, argv []string, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	var out, errOut strings.Builder
	root.Writer = &out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader(stdin)
	code = Run(ctx, root, append([]string{"rancher"}, argv...))
	return out.String(), errOut.String(), code
}

// assertNoCanary asserts that none of the forms of canary is in any of
// texts. The check reads rendered text because the canary is a value no
// field of the envelope may hold.
func assertNoCanary(t *testing.T, canary string, texts map[string]string) {
	t.Helper()
	for where, text := range texts {
		for _, form := range canaryForms(canary) {
			assert.NotContains(t, text, form, "%s", where)
		}
	}
}

// TestSecretInputErrorsNeverQuoteTheValue asserts that every row of the
// table, a command line with a malformed secret-bearing input holding the
// canary, is refused with the row's code and message, and that the canary,
// as typed and in each base64 alignment, is in none of stdout, stderr and
// the error the envelope carries. The check on the text of the output is a
// string search of rendered JSON on purpose: the canary must be absent
// wherever it could be written, not only in the fields a decoder reads.
// Every run uses a temporary config directory, the file credential store and
// a local stub server. Not parallel: it sets the process environment and the
// captured command line.
func TestSecretInputErrorsNeverQuoteTheValue(t *testing.T) {
	restoreRawArgv(t)
	for _, row := range secretRows() {
		t.Run(cmp.Or(row.flag, row.env, "other")+"/"+row.name, func(t *testing.T) {
			srv := secretServer(t, row.stub, secretBearer)
			for k, v := range row.setEnv {
				t.Setenv(k, v)
			}
			for name, content := range row.files {
				require.NoError(t, os.WriteFile(filepath.Join(".", name), []byte(content), 0o600))
			}
			fill := func(words []string) []string {
				out := make([]string, 0, len(words)+2)
				for _, w := range words {
					w = strings.ReplaceAll(w, "{url}", srv.URL)
					w = strings.ReplaceAll(w, "{pem}", string(pemFromServer(t, srv)))
					out = append(out, w)
				}
				return out
			}
			for _, line := range row.before {
				stdout, stderr, code := runWithStdin(t, append([]string{"-o", "json"}, fill(line)...), "")
				require.Zero(t, code, "before %v: stdout: %s stderr: %s", line, stdout, stderr)
			}
			argv := append([]string{"-o", "json"}, fill(row.argv)...)
			stdout, stderr, code := runWithStdin(t, argv, row.stdin)

			canary := cmp.Or(row.canary, secretCanary)
			assertNoCanary(t, canary, map[string]string{"stdout": stdout, "stderr": stderr})
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, row.code, string(ce.Code), "stderr: %s", stderr)
			assert.Contains(t, ce.Message, row.message)
			if row.hint != "" {
				assert.Equal(t, row.hint, ce.Hint)
			}
			assertNoCanary(t, canary, map[string]string{"code": ce.Code, "message": ce.Message, "hint": ce.Hint})
		})
	}
}

// TestUsageErrorTextLeavesTheValueOutForASecretInput asserts that the text
// of a flag-parse error quotes the flag's value for an ordinary command, and
// names only the flag, and keeps only a reason that quotes nothing, for a
// command that takes a secret input, whatever the value holds.
func TestUsageErrorTextLeavesTheValueOutForASecretInput(t *testing.T) {
	t.Parallel()
	ordinary := &ucli.Command{Name: "a", Metadata: map[string]any{descriptorMetadataKey: runtime.Descriptor{Name: "a"}}}
	secret := &ucli.Command{Name: "b", Metadata: map[string]any{descriptorMetadataKey: runtime.Descriptor{Name: "b", SecretInput: true}}}
	undescribed := &ucli.Command{Name: "c"}

	for _, tc := range []struct {
		name, err, ordinary, secret string
	}{
		{"a flag given twice", `invalid value "k=v" for flag -from-literal: can't duplicate this flag`, `invalid value "k=v" for flag -from-literal: can't duplicate this flag`, "invalid value for flag -from-literal: can't duplicate this flag"},
		{"a bool flag with a value", `invalid value "pw" for flag -password-stdin: parse error`, `invalid value "pw" for flag -password-stdin: parse error`, "invalid value for flag -password-stdin: parse error"},
		{"a value that holds the text around it", `invalid value "x for flag -y: parse error" for flag -z: parse error`, `invalid value "x for flag -y: parse error" for flag -z: parse error`, "invalid value for flag -z: parse error"},
		{"a reason that quotes the value", `invalid value "abc" for flag -n: strconv.ParseInt: parsing "abc": invalid syntax`, `invalid value "abc" for flag -n: strconv.ParseInt: parsing "abc": invalid syntax`, "invalid value for flag -n"},
		{"no flag named", `invalid value "abc"`, `invalid value "abc"`, "invalid value for a flag"},
		{"a flag typed without its value, which the command defines", "flag needs an argument: -p", "flag needs an argument: -p", "flag needs an argument: -p"},
		{"a text no list has", "bad flag syntax: ---s3cretcanary9", "bad flag syntax: ---s3cretcanary9", "the command line could not be parsed"},
		{"an empty text", "", "", "the command line could not be parsed"},
		{"a text that only starts like a known one", "flag needs an argumentative word: -s3cretcanary9", "flag needs an argumentative word: -s3cretcanary9", "the command line could not be parsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := errors.New(tc.err)
			assert.Equal(t, tc.ordinary, usageErrorText(ordinary, err))
			wantUndescribed := tc.ordinary
			if strings.HasPrefix(tc.err, "invalid value ") {
				wantUndescribed = tc.secret
			}
			assert.Equal(t, wantUndescribed, usageErrorText(undescribed, err), "the root or a group names the flag and not its value, and passes every other text")
			assert.Equal(t, tc.secret, usageErrorText(secret, err))
		})
	}
}

// TestUnknownFlagTextGivesThePositionOfTheWord asserts that an undefined flag
// of a command that takes a secret input is named by the position of its
// word on the command line, counted from 1, whatever the word holds: a long
// or short spelling, with or without "=", and a word that only looks like a
// flag. A command with no secret input keeps the text with the flag name.
func TestUnknownFlagTextGivesThePositionOfTheWord(t *testing.T) {
	t.Parallel()
	argv := []string{"-o", "json", "create", "user", "bob", "--password=s3cretcanary9", "-s3cretcanary8", "--dry-run"}
	assert.Equal(t, "unknown flag at word 6 after rancher", unknownFlagText("password", argv))
	assert.Equal(t, "unknown flag at word 7 after rancher", unknownFlagText("s3cretcanary8", argv))
	assert.Equal(t, "unknown flag", unknownFlagText("absent", argv))
	assert.Equal(t, "unknown flag", unknownFlagText("bob", argv), "a word with no dash is not a flag")
	assert.Equal(t, "unknown flag", unknownFlagText("x", []string{" -x"}), "a word padded with a space gives no position")
	assert.Equal(t, "unknown flag", unknownFlagText("-x", []string{"---x"}), "a word with three dashes gives no position")

	ordinary := &ucli.Command{Name: "a", Metadata: map[string]any{descriptorMetadataKey: runtime.Descriptor{Name: "a"}}}
	assert.Equal(t, "flag provided but not defined: -x", usageErrorText(ordinary, errors.New("flag provided but not defined: -x")))
}

// leafNamed returns the name of the descriptor of the leaf, or "" for nil.
func leafNamed(leaf *ucli.Command) string {
	if leaf == nil {
		return ""
	}
	d, _ := commandDescriptor(leaf)
	return d.Name
}

// TestLeafOfLineFollowsTheLineThroughUndefinedFlags asserts that the leaf a
// line names is found with every undefined flag read as a flag with no value
// and taken out, at the root, at a group, several in a row, through an alias,
// and with a flag that takes a value and a word after "--"; that a line that
// ends at the root or a group, or at a group's flag, names no leaf; and that
// the line is not changed.
func TestLeafOfLineFollowsTheLineThroughUndefinedFlags(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a flag at the group", []string{"create", "-x", "user", "bob"}, "create user"},
		{"a flag at the root", []string{"-x", "create", "user", "bob"}, "create user"},
		{"a flag at the root and one at the group", []string{"-x", "create", "-y", "user"}, "create user"},
		{"an alias of a leaf", []string{"get", "-typo", "pods"}, "get pod"},
		{"two flags before a command with no secret input", []string{"-x", "get", "-y", "pods"}, "get pod"},
		{"the flag takes no value, so the next word is the command", []string{"-x", "create", "user"}, "create user"},
		{"a defined flag with a value before the command", []string{"-o", "json", "-x", "create", "user"}, "create user"},
		{"an undefined flag after the end of the flags", []string{"create", "user", "--", "-x"}, "create user"},
		{"a flag joined with =", []string{"--x=v", "create", "user"}, "create user"},
		{"nothing", nil, ""},
		{"an undefined flag alone", []string{"-x"}, ""},
		{"a group alone", []string{"get"}, ""},
		{"a group and an undefined flag", []string{"create", "-x"}, ""},
		{"a flag with no value at the root", []string{"-o"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			argv := slices.Clone(tc.argv)
			assert.Equal(t, tc.want, leafNamed(leafOfLine(root, argv)))
			assert.Equal(t, tc.argv, argv, "the line is read, not changed")
		})
	}
}

// TestUnknownFlagTextAtResolvesTheLeafOfTheLine asserts, on a tree of its own,
// that an undefined flag reported by the root or a group gets the position
// text when the line's leaf takes a secret input, has no descriptor or is not
// found, and urfave's text when the leaf has a descriptor that takes none.
func TestUnknownFlagTextAtResolvesTheLeafOfTheLine(t *testing.T) {
	t.Parallel()
	leaf := func(name string, md map[string]any) *ucli.Command { return &ucli.Command{Name: name, Metadata: md} }
	described := func(secret bool) map[string]any {
		return map[string]any{descriptorMetadataKey: runtime.Descriptor{Name: "x", SecretInput: secret}}
	}
	root := &ucli.Command{Name: "rancher", Commands: []*ucli.Command{
		leaf("secret", described(true)),
		leaf("plain", described(false)),
		leaf("bare", nil),
		{Name: "group", Commands: []*ucli.Command{leaf("secret", described(true)), leaf("bare", nil)}},
	}}
	const msg = "flag provided but not defined: -x"
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a leaf that takes a secret input", []string{"-x", "secret"}, "unknown flag at word 1 after rancher"},
		{"a leaf that takes no secret input", []string{"-x", "plain"}, msg},
		{"a leaf with no descriptor", []string{"-x", "bare"}, "unknown flag at word 1 after rancher"},
		{"a leaf of a group, with a secret input", []string{"group", "-x", "secret"}, "unknown flag at word 2 after rancher"},
		{"a leaf of a group, with no descriptor", []string{"group", "-x", "bare"}, "unknown flag at word 2 after rancher"},
		{"no leaf", []string{"-x"}, "unknown flag at word 1 after rancher"},
		{"no leaf, at a group", []string{"group", "-x"}, "unknown flag at word 2 after rancher"},
		{"a word not found in the line", []string{"secret"}, "unknown flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, unknownFlagTextAt(root, "x", tc.argv, msg))
		})
	}
}

// TestAnUnknownFlagBeforeTheCommandNameIsNamedByTheCommand asserts, through
// the process entry point, that an undefined flag typed before the command
// name is named by its position when the line's command takes a secret input
// or no command is named, and keeps urfave's text, with the flag as typed,
// for a command that takes none, at the root and at a group, so a typo of
// such a flag still reads. The canary is the flag word of the first row.
// Not parallel: it sets the process environment and the captured command
// line.
func TestAnUnknownFlagBeforeTheCommandNameIsNamedByTheCommand(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	for _, tc := range []struct {
		name   string
		argv   []string
		want   string
		noWord bool
	}{
		{"no command is named", []string{"-" + secretCanary}, "unknown flag at word 3 after rancher", true},
		{"a command with no secret input, an alias, at a group", []string{"get", "-typo", "pods"}, "flag provided but not defined: -typo", false},
		{"two unknown flags before a command with no secret input", []string{"-x", "get", "-y", "pods"}, "flag provided but not defined: -x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runWithStdin(t, append([]string{"-o", "json"}, tc.argv...), "")
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, string(clierr.CodeValidation), ce.Code)
			assert.Contains(t, ce.Message, tc.want)
			if tc.noWord {
				assertNoCanary(t, secretCanary, map[string]string{"stdout": stdout, "stderr": stderr})
			}
		})
	}
}

// secretCommands returns the descriptors of the commands that set
// SecretInput, in registry order.
func secretCommands(t *testing.T) []runtime.Descriptor {
	t.Helper()
	wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	var out []runtime.Descriptor
	for _, c := range wr.reg.Commands() {
		if d := c.Describe(); d.SecretInput {
			out = append(out, d)
		}
	}
	require.NotEmpty(t, out, "a command that takes a secret input was found")
	return out
}

// TestNoCommandThatTakesASecretShowsAFlagWordTypedBeforeItsName asserts, for
// every command that sets SecretInput, that a flag typed before each word of
// the command's name, once as an undefined flag holding the canary and once
// as a defined bool flag given the canary as its value, shows the canary in
// none of stdout, stderr and the error. Not parallel: it sets the process
// environment and the captured command line.
func TestNoCommandThatTakesASecretShowsAFlagWordTypedBeforeItsName(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	commands := secretCommands(t)
	ran := 0
	for _, d := range commands {
		words := strings.Fields(d.Name)
		for i := range words {
			for _, flag := range []string{"-" + secretCanary, "--debug=" + secretCanary} {
				argv := []string{"-o", "json"}
				argv = append(argv, words[:i]...)
				argv = append(argv, flag)
				argv = append(argv, words[i:]...)
				stdout, stderr, code := runWithStdin(t, argv, "")
				label := strings.Join(argv, " ")
				assertNoCanary(t, secretCanary, map[string]string{"stdout " + label: stdout, "stderr " + label: stderr})
				assert.NotZero(t, code, label)
				ran++
			}
		}
	}
	assert.NotZero(t, ran, "a command that takes a secret input ran")
}

// TestOutputAndColorValuesOfACommandThatTakesASecretAreNotNamed asserts, in
// the text form of the error, that a -o or --color given no value of its own
// takes the next word, which may be a secret, and that the refusal names
// the flag and not the value for a command that takes a secret input: an
// unknown format, a template that does not parse, an unknown color, and a
// registered flag written with "=", which the flag value check refuses
// before the output check runs. An ordinary command keeps the value in its
// refusal, and the hidden completion command, which has no descriptor, names an unknown flag by its
// position. Each row asserts the exit code, the message and that the canary
// is in neither stdout nor stderr. Not parallel: it sets the process
// environment and the captured command line.
func TestOutputAndColorValuesOfACommandThatTakesASecretAreNotNamed(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	const oneOf = "table|wide|json|yaml|name|template=<go-template>"
	c := secretCanary
	for _, tc := range []struct {
		name    string
		argv    []string
		message string
		canary  bool
	}{
		{"-o given a flag written with =, which the flag value check refuses", []string{"create", "secret", "s1", "-o", "--from-literal=k=" + c},
			"flag -o got flag --from-literal as its value - empty shell variable? check that the value for -o is set, or use -o=<value> to pass a literal flag-like string", true},
		{"-o given the canary", []string{"create", "secret", "s1", "-o", c}, "--output value is not one of " + oneOf, true},
		{"-o given a template that does not parse", []string{"create", "secret", "s1", "-o", "template={{" + c}, "--output template does not parse; must be valid Go text/template syntax", true},
		{"--color given the canary", []string{"create", "secret", "s1", "--color", c}, "--color value is not one of auto|always|never", true},
		{"an ordinary command keeps the value of -o", []string{"get", "pods", "-o", "typo"}, `--output="typo" is not one of ` + oneOf, false},
		{"an ordinary command keeps the value of --color", []string{"get", "pods", "--color", "typo"}, `--color="typo" is not one of auto|always|never`, false},
		{"the hidden completion command names the unknown flag by position", []string{"__completion", "--" + c}, "unknown flag at word 2 after rancher; pass --help for usage", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runWithStdin(t, tc.argv, "")
			assert.Equal(t, clierr.ExitCode(clierr.CodeValidation), code, "stderr: %s", stderr)
			ce := renderedError(t, "", stderr)
			assert.Equal(t, string(clierr.CodeValidation), ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			if tc.canary {
				assertNoCanary(t, c, map[string]string{"stdout": stdout, "stderr": stderr})
			}
		})
	}
}

// outputWordRun is one run of the guard over -o and --color.
type outputWordRun struct {
	argv []string
	want string
}

// TestNoCommandThatTakesASecretNamesAnOutputOrColorValue asserts, for every
// command that sets SecretInput, that -o and --color given the canary, typed
// after the command's words, refuse with the fixed text and show the canary
// in none of stdout and stderr. Not parallel: it sets the process
// environment and the captured command line.
func TestNoCommandThatTakesASecretNamesAnOutputOrColorValue(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	commands := secretCommands(t)
	ran := 0
	for _, d := range commands {
		words := strings.Fields(d.Name)
		for _, run := range []outputWordRun{
			{append(slices.Clone(words), "-o", secretCanary), "--output value is not one of"},
			{append(slices.Clone(words), "--color", secretCanary), "--color value is not one of"},
		} {
			stdout, stderr, code := runWithStdin(t, run.argv, "")
			label := strings.Join(run.argv, " ")
			assertNoCanary(t, secretCanary, map[string]string{"stdout " + label: stdout, "stderr " + label: stderr})
			assert.Equal(t, clierr.ExitCode(clierr.CodeValidation), code, label)
			assert.Contains(t, renderedError(t, "", stderr).Message, run.want, label)
			ran++
		}
	}
	assert.NotZero(t, ran, "a command that takes a secret input ran with -o and with --color")
}

// requiredWords fills the required arguments and required non-bool flags of
// d the way the validation sweep fills them (fillRequired: the first allowed
// value of an enum, "x" otherwise), as command line words, leaving out the
// flag skipFlag. upto is how many positional arguments to give (the
// canary's position included), or -1 for every required one.
func requiredWords(d runtime.Descriptor, skipFlag string, upto int) (args, flags []string) {
	for i, a := range d.Args {
		if upto >= 0 && i >= upto {
			break
		}
		if upto < 0 && !a.Required {
			continue
		}
		v := "x"
		if len(a.Enum) > 0 {
			v = a.Enum[0]
		}
		args = append(args, v)
	}
	for _, f := range d.Flags {
		if !f.Required || f.Type == "bool" || f.Name == skipFlag {
			continue
		}
		v := "x"
		if len(f.Enum) > 0 {
			v = f.Enum[0]
		}
		flags = append(flags, "--"+f.Name, v)
	}
	return args, flags
}

// TestNoCommandThatTakesASecretNamesAnEnumValue asserts, for every flag and
// every argument with an enum of every command that sets SecretInput, that
// the canary given where the enum is checked is refused with the text that
// names the flag or the argument and the allowed values, never the value.
// Each run fills the command's required arguments and flags, so a run that
// stops at an earlier check fails the message assertion. At least one flag
// and one argument with an enum are found. Not parallel: it sets the process
// environment and the captured command line.
func TestNoCommandThatTakesASecretNamesAnEnumValue(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	commands := secretCommands(t)
	flagRuns, argRuns := 0, 0
	for _, d := range commands {
		for _, f := range d.Flags {
			if len(f.Enum) > 0 {
				flagRuns++
			}
		}
		for _, a := range d.Args {
			if len(a.Enum) > 0 {
				argRuns++
			}
		}
	}
	require.NotZero(t, flagRuns, "a flag with an enum was found")
	require.NotZero(t, argRuns, "an argument with an enum was found")

	run := func(argv []string, want string) {
		t.Helper()
		label := strings.Join(argv, " ")
		stdout, stderr, code := runWithStdin(t, argv, "")
		assertNoCanary(t, secretCanary, map[string]string{"stdout " + label: stdout, "stderr " + label: stderr})
		assert.Equal(t, clierr.ExitCode(clierr.CodeValidation), code, label)
		assert.Contains(t, renderedError(t, "", stderr).Message, want, label)
	}
	for _, d := range commands {
		words := strings.Fields(d.Name)
		for _, f := range d.Flags {
			if len(f.Enum) == 0 {
				continue
			}
			args, flags := requiredWords(d, f.Name, -1)
			argv := slices.Concat(words, args, flags, []string{"--" + f.Name, secretCanary})
			run(argv, "--"+f.Name+" value is not one of")
		}
		for i, a := range d.Args {
			if len(a.Enum) == 0 {
				continue
			}
			args, flags := requiredWords(d, "", i)
			argv := slices.Concat(words, args, []string{secretCanary}, flags)
			run(argv, "argument <"+a.Name+"> value is not one of")
		}
	}
}

// TestNoCommandThatTakesASecretShowsAFlagWithEqualsTakenAsAValue asserts, for
// every value-taking flag of every command that sets SecretInput (its own
// flags and the global ones), that the flag typed with no value before a
// registered flag written with "=" and holding the canary is refused with
// the two flag spellings and shows the canary in none of stdout and stderr.
// Each run uses one fixed second flag, --color, or --output for --color. At
// least one command and one flag are found. Not parallel: it sets the
// process environment and the captured command line.
func TestNoCommandThatTakesASecretShowsAFlagWithEqualsTakenAsAValue(t *testing.T) {
	restoreRawArgv(t)
	secretServer(t, nil, secretBearer)
	ownFlags := 0
	for _, d := range secretCommands(t) {
		words := strings.Fields(d.Name)
		seen := map[string]bool{}
		var valueFlags []string
		for _, f := range slices.Concat(d.Flags, globalArgSpecs) {
			if f.Type == "bool" || seen[f.Name] {
				continue
			}
			seen[f.Name] = true
			valueFlags = append(valueFlags, f.Name)
		}
		for _, f := range d.Flags {
			if f.Type != "bool" {
				ownFlags++
			}
		}
		for _, flag := range valueFlags {
			second := "color"
			if flag == "color" {
				second = "output"
			}
			argv := slices.Concat(words, []string{"--" + flag, "--" + second + "=" + secretCanary})
			stdout, stderr, code := runWithStdin(t, argv, "")
			label := strings.Join(argv, " ")
			assertNoCanary(t, secretCanary, map[string]string{"stdout " + label: stdout, "stderr " + label: stderr})
			assert.Equal(t, clierr.ExitCode(clierr.CodeValidation), code, label)
			assert.Contains(t, renderedError(t, "", stderr).Message, "flag --"+flag+" got flag --"+second+" as its value", label)
		}
	}
	assert.NotZero(t, ownFlags, "a value-taking flag of a command that takes a secret input, besides the global ones, was found")
}
