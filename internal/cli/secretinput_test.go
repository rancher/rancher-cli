package cli

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// secretCanary is the value every row of the secret input table feeds a
// command in a position that holds a secret: one line, no spaces, letters
// and digits, so no renderer escapes or splits it.
const secretCanary = "s3cretcanary9"

// shortSecretCanary is a canary shorter than the 12 characters a password
// needs, for the rows where the server refuses a short password.
const shortSecretCanary = "s3cretcnry9"

// canaryForms returns canary as every renderer could show it: as typed, in
// lower case (an identity is lowercased), and in each of the three base64 alignments (the text after zero, one or two
// bytes of other text), cut to the characters the neighbor bytes do not
// change. The renderers do not HTML-escape, so a JSON-escaped form adds
// nothing for a canary of letters and digits.
func canaryForms(canary string) []string {
	forms := []string{canary, strings.ToLower(canary)}
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
	require.Len(t, forms, 5)
	for pad := range 6 {
		enc := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("-", pad) + secretCanary + "tail"))
		found := false
		for _, f := range forms[2:] {
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
	// stub responds to the requests it takes; the stub Rancher responds to the rest.
	stub secretStub
	// canary is the secret the row asserts absent; empty means secretCanary.
	canary string
	// tolerate is a fixed text of the error that holds the canary's letters
	// (a refusal that says "true or false" for a canary of TRUE); it is cut
	// out of the output before the canary is looked for.
	tolerate string
	// code and message are the error the row must produce: its code and a
	// text its message holds, so a row cannot pass by failing elsewhere.
	code    string
	message string
	// exit, when set, is the exit code the command must end with.
	exit int
	// contains are further texts the error's message holds.
	contains []string
	// hint, when set, is the text the error's hint must equal.
	hint string
	// stored writes config.yaml, with this one entry, into the config
	// directory before the before lines and the seed run.
	stored *secretStored
	// seed runs after the config is written, with the config directory and
	// the stub's own usable URL; it may add the records the row needs.
	seed func(tb testing.TB, dir, serverURL string)
	// noRefusal marks a row whose command is not refused for its URL: the
	// row asserts the message (when there is an error) holds none of forbid,
	// and a non-zero exit is not required.
	noRefusal bool
	// forbid are texts the output holds none of: those of a noRefusal row,
	// and the message of a refused one.
	forbid []string
	// instead is the text a noRefusal row's output holds in place of the
	// refusal, so it cannot pass by failing elsewhere; reaches says the stub
	// server saw a request.
	instead string
	reaches bool
}

// secretBearer is the token the stub server knows; the row runs use it as
// RANCHER_CLI_TOKEN. Any other token is refused with 401.
const secretBearer = "u-member:secret"

// secretStub is a server response in front of the stub Rancher: it writes
// the response and returns true, or returns false to let the stub Rancher
// respond.
type secretStub func(w http.ResponseWriter, r *http.Request) bool

// writeKubeJSON writes body as a JSON response with status.
func writeKubeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const secretConfigMap = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"app","namespace":"default","resourceVersion":"7"},"data":{"a":"1"}}`

// configMapStub responds to the get of the ConfigMap "app" with the object.
func configMapStub(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/configmaps/app") {
		writeKubeJSON(w, http.StatusOK, secretConfigMap)
		return true
	}
	return false
}

// unauthorizedStub responds to every request with 401, as a server does for a
// token it does not know.
func unauthorizedStub(w http.ResponseWriter, _ *http.Request) bool {
	writeKubeJSON(w, http.StatusUnauthorized, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`)
	return true
}

// shortPasswordStub serves the ext discovery of a server that has the
// password-change-request resource and the global roles create user reads,
// and responds to every create the way a server responds to a password it
// refuses as too short: 422 with the length rule, which names no value.
func shortPasswordStub(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.Method == http.MethodPost:
		writeKubeJSON(w, http.StatusUnprocessableEntity, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"password must be at least 12 characters","reason":"Invalid","code":422}`)
	case r.URL.Path == "/k8s/clusters/local/apis/management.cattle.io/v3/globalroles":
		writeKubeJSON(w, http.StatusOK, `{"kind":"GlobalRoleList","apiVersion":"management.cattle.io/v3","items":[{"apiVersion":"management.cattle.io/v3","kind":"GlobalRole","metadata":{"name":"user"},"newUserDefault":true}]}`)
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

// passwordStatusBody returns a Status body the way Rancher writes its response
// when it refuses a PasswordChangeRequest create request: the text in the
// message and, when cause is set, in details.causes[0].message. tail is
// appended to both texts.
func passwordStatusBody(code int, reason, message, cause, tail string) string {
	details := ""
	if cause != "" {
		details = `,"details":{"causes":[{"message":"` + cause + tail + `"}]}`
	}
	return `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"` + message + tail + `","reason":"` + reason + `"` + details + `,"code":` + strconv.Itoa(code) + `}`
}

// invalidCurrentPasswordBody is the 500 Rancher responds with when it refuses
// the current password.
func invalidCurrentPasswordBody(tail string) string {
	return passwordStatusBody(500, "InternalError", "Internal error occurred: error updating password: invalid current password",
		"error updating password: invalid current password", tail)
}

// notAuthorizedBody is the 401 Rancher responds with when a user changes
// another user's password without permission to change every user's password.
func notAuthorizedBody(tail string) string {
	return passwordStatusBody(401, "Unauthorized", "not authorized to update password", "", tail)
}

// missingPasswordSecretBody is a 500 whose text the command does not map.
const missingPasswordSecretBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Internal error occurred: error updating password: failed to get password secret: secrets \"u-abc\" not found","reason":"InternalError","details":{"causes":[{"message":"error updating password: failed to get password secret: secrets \"u-abc\" not found"}]},"code":500}`

// permissionsCheckBody is a 401 whose text the command does not map.
const permissionsCheckBody = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"error checking permissions: the server could not check the caller","reason":"Unauthorized","code":401}`

// currentPasswordStub serves the discovery of a server that has the
// password-change-request resource and responds to its create requests as
// Rancher does: to a create request with dryRun=All it responds with 201, the
// request's spec echoed, both passwords included, and no condition; to the
// real create request it responds with the Status body realBody and the
// status realStatus, or, when realBody is empty, with 201, the spec echoed and
// a True PasswordUpdated condition.
func currentPasswordStub(realStatus int, realBody string) secretStub {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/passwordchangerequests") {
			return shortPasswordStub(w, r)
		}
		var sent struct {
			Spec map[string]any `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			writeKubeJSON(w, http.StatusBadRequest, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"bad request","reason":"BadRequest","code":400}`)
			return true
		}
		status := map[string]any{"conditions": nil}
		if r.URL.Query().Get("dryRun") != "All" {
			if realBody != "" {
				writeKubeJSON(w, realStatus, realBody)
				return true
			}
			status = map[string]any{"conditions": []any{map[string]any{"type": "PasswordUpdated", "status": "True"}}, "summary": "Completed"}
		}
		out, err := json.Marshal(map[string]any{"kind": "PasswordChangeRequest", "apiVersion": "ext.cattle.io/v1", "metadata": map[string]any{}, "spec": sent.Spec, "status": status})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		writeKubeJSON(w, http.StatusCreated, string(out))
		return true
	}
}

// secretManifest is a Secret manifest whose value field is value.
func secretManifest(value string) string {
	return "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\nstringData:\n  password: " + value + "\n"
}

const (
	createSecretLine = "create secret s --cluster c-x --namespace d --dry-run"
	createUserLine   = "create user bob --dry-run"
	passwordLine     = "create password-change-request u-abc --dry-run"
	// passwordApplyLine applies the change and sends the real create request.
	passwordApplyLine = "create password-change-request u-abc --apply --yes"
	machineConfig     = "create machine-config amazonec2config w --dry-run"
	// createCloudCredentialLine is a create that the checks of the command line
	// refuse before any request.
	createCloudCredentialLine = "create cloud-credential s3 x --dry-run"
	// patchCloudCredentialLine is a patch that the checks of the command line
	// refuse before any request.
	patchCloudCredentialLine = "patch cloud-credential cc-abcde --dry-run"
	patchLine                = "patch configmap app --cluster c-x --namespace default --dry-run"
	createFileLine           = "create resource --cluster c-x --namespace n2 --dry-run"
	loginLine                = "auth login x --url {url} --cacert {pem}"
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
		secretRow{flag: "from-literal", name: "an extra argument", argv: line(createSecretLine, "--from-literal", "a=b", c), code: validation, message: "unexpected argument at position 2"},
		secretRow{flag: "from-file", name: "no equals", argv: line(createCloudCredentialLine, "--from-file", c), code: validation, message: "the 1st --from-file must be <field>=<path>"},
		secretRow{flag: "from-file", name: "empty field", argv: line(createCloudCredentialLine, "--from-file", "="+c), code: validation, message: "the 1st --from-file has an empty field name"},
		secretRow{flag: "from-file", name: "an extra argument", argv: line(createCloudCredentialLine, "--from-file", "secretKey=f", c), code: validation, message: "unexpected argument at position 3"},
		secretRow{flag: "from-file", name: "no equals, on patch cloud-credential", argv: line(patchCloudCredentialLine, "--from-file", c), code: validation, message: "the 1st --from-file must be <field>=<path>"},
		secretRow{flag: "from-file", name: "empty field, on patch cloud-credential", argv: line(patchCloudCredentialLine, "--from-file", "="+c), code: validation, message: "the 1st --from-file has an empty field name"},
		secretRow{flag: "from-file", name: "an extra argument, on patch cloud-credential", argv: line(patchCloudCredentialLine, "--from-file", "secretKey=f", c), code: validation, message: "unexpected argument at position 2"},
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
		secretRow{flag: "filename", name: "an extra argument", argv: line(createFileLine, "-f", "m.yaml", c), files: map[string]string{"m.yaml": secretManifest("x")}, code: validation, message: "unexpected argument at position 1"},
	)

	malformedPatch := `[{"op":"add","path":"/data/a"},{"op":"add","path":"/data/b","value":"` + c + `"}]`
	failingTest := `[{"op":"test","path":"/data/a","value":"` + c + `"}]`
	notJSON := `[{"op":"add","path":"/data/a","value":"` + c + `"`
	add(
		secretRow{flag: "patch", name: "a malformed op ahead of an op holding the canary", argv: line(patchLine, "-p", malformedPatch), code: validation, message: `patch operation 0 (add) has no "value"`},
		secretRow{flag: "patch", name: "a test op that fails", argv: line(patchLine, "-p", failingTest), stub: configMapStub, code: validation, message: `the value at "/data/a" differs from the value in the "value" member, or there is no value there`},
		secretRow{flag: "patch", name: "a patch that is not JSON", argv: line(patchLine, "-p", notJSON), code: validation, message: "not valid JSON"},
		secretRow{flag: "patch", name: "an extra argument", argv: line(patchLine, "-p", "[]", c), code: validation, message: "unexpected argument at position 2"},
		secretRow{flag: "patch-file", name: "a malformed op ahead of an op holding the canary", argv: line(patchLine, "--patch-file", "-"), stdin: malformedPatch, code: validation, message: `patch operation 0 (add) has no "value"`},
		secretRow{flag: "patch-file", name: "a test op that fails", argv: line(patchLine, "--patch-file", "-"), stdin: failingTest, stub: configMapStub, code: validation, message: `the value at "/data/a" differs from the value in the "value" member, or there is no value there`},
		secretRow{flag: "patch-file", name: "a patch that is not JSON", argv: line(patchLine, "--patch-file", "-"), stdin: notJSON, code: validation, message: "not valid JSON"},
	)

	add(
		secretRow{flag: "set", name: "no equals", argv: line(machineConfig, "--set", c), code: validation, message: "--set argument #1 must be <field>=<value>"},
		secretRow{flag: "set", name: "empty field name", argv: line(machineConfig, "--set", "="+c), code: validation, message: "--set argument #1 has an empty field name"},
		secretRow{flag: "set", name: "a credential field", argv: line(machineConfig, "--set", "secretKey="+c), code: validation, message: "holds a credential"},
		secretRow{flag: "set", name: "a top-level object field", argv: line(machineConfig, "--set", "kind="+c), code: validation, message: "names a top-level Kubernetes object field"},
		secretRow{flag: "set", name: "a field given twice", argv: line(machineConfig, "--set", "a=b", "--set", "a="+c), code: validation, message: "is given more than once"},
		secretRow{flag: "set", name: "an extra argument", argv: line(machineConfig, "--set", "a=b", c), code: validation, message: "unexpected argument at position 3"},
	)

	for _, cmd := range []struct{ line, extra string }{{createUserLine, "unexpected argument at position 2"}, {passwordLine, "unexpected argument at position 2"}} {
		add(
			secretRow{flag: "password-stdin", name: cmd.line + ": an empty password", argv: line(cmd.line, "--password-stdin"), code: validation, message: "the password read from stdin is empty"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the flag without the pipe", argv: line(cmd.line), stdin: c, code: validation, message: "--password-stdin is required"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed after the flag with =", argv: line(cmd.line, "--password-stdin="+c), code: validation, message: "invalid value for flag -password-stdin: the value must be true or false"},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed as an argument", argv: line(cmd.line, "--password-stdin", c), code: validation, message: cmd.extra},
			secretRow{flag: "password-stdin", name: cmd.line + ": the password typed after a flag that does not exist", argv: line(cmd.line, "--password", c), code: validation, message: "unknown flag at argument 7 after rancher"},
		)
	}
	short := shortSecretCanary
	add(
		secretRow{flag: "password-stdin", name: createUserLine + ": a password the server refuses as too short", argv: line(createUserLine, "--password-stdin"), stdin: short, stub: shortPasswordStub, canary: short, code: validation, message: "password must be at least 12 characters"},
		secretRow{flag: "password-stdin", name: passwordLine + ": a password the server refuses as too short", argv: line(passwordLine, "--password-stdin"), stdin: short, stub: shortPasswordStub, canary: short, code: validation, message: "password must be at least 12 characters"},
	)

	add(currentPasswordRows()...)

	add(
		secretRow{flag: "token-stdin", name: "a token with no colon", argv: line(loginLine, "--token-stdin"), stdin: c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "a token with no id", argv: line(loginLine, "--token-stdin"), stdin: ":" + c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "a token the server refuses", argv: line(loginLine, "--token-stdin"), stdin: "id:" + c, code: string(clierr.CodeAuth), message: "the server rejected the credential"},
		secretRow{flag: "token-stdin", name: "a token with no colon, not verified", argv: line(loginLine, "--token-stdin", "--no-verify"), stdin: c, code: validation, message: "invalid token (expected non-empty id:secret)"},
		secretRow{flag: "token-stdin", name: "the token typed after the flag with =", argv: line(loginLine, "--token-stdin="+c), code: validation, message: "invalid value for flag -token-stdin: the value must be true or false"},
		secretRow{flag: "token-stdin", name: "the token typed as an argument", argv: line(loginLine, "--token-stdin", c), code: validation, message: "unexpected argument at position 2"},
	)

	for _, token := range []string{c, "id:" + c, "id:" + c + " x"} {
		add(secretRow{env: "RANCHER_CLI_TOKEN", name: "a token the server refuses: " + strings.ReplaceAll(token, c, "canary"), argv: line("get cluster"), setEnv: map[string]string{config.EnvToken: token}, stub: unauthorizedStub, code: string(clierr.CodeAuth), message: "the server rejected the credential"})
	}

	const badPort = "@rancher.invalid:abc"
	refusedBadPort := "the server URL in --url cannot be used: it is not a valid URL"
	refusedUserinfo := "the server URL in --url cannot be used: it holds a user name or password"
	for _, cmd := range []struct{ name, line string }{
		{"config add-server", "config add-server x --url"},
		{"auth login", "auth login x --token-stdin --url"},
	} {
		stdin := ""
		if cmd.name == "auth login" {
			stdin = "id:y"
		}
		add(
			secretRow{flag: "url", name: cmd.name + ": a URL with a password and a bad port", argv: line(cmd.line, "https://u:"+c+badPort), stdin: stdin, code: validation, message: refusedBadPort},
			secretRow{flag: "url", name: cmd.name + ": a URL with a password and an unsupported scheme", argv: line(cmd.line, "ftp://u:"+c+"@rancher.invalid"), stdin: stdin, code: validation, message: refusedUserinfo},
		)
	}
	// The password holds a character that ends an authority, so the part of
	// it after that character is the canary.
	for _, sep := range []string{"/", "?", "#", "@"} {
		add(secretRow{flag: "url", name: "config add-server: a password with " + sep, argv: line("config add-server x --url", "https://u:s3cret"+sep+"canary9"+badPort), canary: "canary9", code: validation, message: refusedBadPort})
	}
	updateBefore := [][]string{line("config add-server x --url {url} --cacert {pem}")}
	add(
		secretRow{flag: "url", name: "config update-server: a URL with a password and a bad port", argv: line("config update-server x --url", "https://u:"+c+badPort), before: updateBefore, code: validation, message: refusedBadPort},
		secretRow{flag: "url", name: "config update-server: a URL with a password and an unsupported scheme", argv: line("config update-server x --url", "ftp://u:"+c+"@rancher.invalid"), before: updateBefore, code: validation, message: refusedUserinfo},
		secretRow{flag: "url", name: "config update-server: a URL with a password typed as an argument", argv: line("config update-server x", "https://u:"+c+"@rancher.invalid"), before: updateBefore, code: validation, message: "unexpected argument at position 2"},
		secretRow{flag: "url", name: "config add-server: a URL with a password typed as an argument", argv: line("config add-server x --url {url}", "https://u:"+c+"@rancher.invalid"), code: validation, message: "unexpected argument at position 2"},
		secretRow{flag: "url", name: "auth login: a URL with a password typed as an argument", argv: line("auth login x --token-stdin", "https://u:"+c+"@rancher.invalid"), stdin: "id:y", code: validation, message: "unexpected argument at position 2"},
	)

	// A bool flag followed by a word that reads as a bool is refused; the
	// hint of a command with a secret input is its help, never the line
	// typed, which holds the other flags' values.
	add(
		secretRow{flag: "from-literal", name: "a bool value refusal does not repeat the flag values", argv: line("create secret s --cluster c-x --namespace d --from-literal", "pw="+c, "--dry-run", "false"), code: validation, message: "create secret: argument at position 2 was typed after the bool flag --dry-run", hint: "rancher create secret --help"},
		secretRow{flag: "patch", name: "a bool value refusal does not repeat the flag values", argv: line("patch configmap app --cluster c-x --namespace default -p", `[{"op":"add","path":"/data/b","value":"`+c+`"}]`, "--dry-run", "false"), code: validation, message: "patch configmap: argument at position 2 was typed after the bool flag --dry-run", hint: "rancher patch configmap --help"},
		secretRow{flag: "set", name: "a bool value refusal does not repeat the flag values", argv: line("create machine-config amazonec2config w --set", "a="+c, "--dry-run", "false"), code: validation, message: "create machine-config: argument at position 3 was typed after the bool flag --dry-run", hint: "rancher create machine-config --help"},
		secretRow{flag: "url", name: "a bool value refusal does not repeat the flag values", argv: line("auth login x --token-stdin --url", "https://u:"+c+"@rancher.invalid", "--no-verify", "false"), stdin: "id:y", code: validation, message: "auth login: argument at position 2 was typed after the bool flag --no-verify", hint: "rancher auth login --help"},
		secretRow{flag: "password-stdin", name: "a word that reads as a bool is named by position", argv: line("create user bob --password-stdin --dry-run TRUE"), canary: "TRUE", tolerate: boolRule("--dry-run"), code: validation, message: "create user: argument at position 2 was typed after the bool flag --dry-run", hint: "rancher create user --help"},
		secretRow{flag: "password-stdin", name: "a word after a bare dash is counted, not named", argv: line("create user bob --password-stdin -", c), code: validation, message: "1 word after a - on its own is not used"},
		secretRow{flag: "from-literal", name: "words after a bare dash are counted, not named", argv: line("create secret s --cluster c-x --namespace d -", "--from-literal", "pw="+c), code: validation, message: "2 words after a - on its own are not used"},
		secretRow{flag: "password-stdin", name: "a password typed with a leading dash is not named", argv: line("create user bob --password-stdin", "-"+c), code: validation, message: "unknown flag at argument 7 after rancher"},
		secretRow{flag: "token-stdin", name: "a token typed with a leading dash is not named", argv: line("auth login x --token-stdin", "-"+c), code: validation, message: "unknown flag at argument 7 after rancher"},
		secretRow{flag: "password-stdin", name: "words after a bare dash at a group level are counted, not named", argv: line("create -", c, "--password-stdin"), code: validation, message: "2 words after a - on its own are not used"},
		secretRow{flag: "password-stdin", name: "words after a bare dash at the root are counted, not named", argv: line("-", c), code: validation, message: "1 word after a - on its own is not used"},
		secretRow{flag: "from-literal", name: "a flag typed without its value is named, since the command defines it", argv: line(createSecretLine, "--from-literal"), code: validation, message: "flag needs an argument: --from-literal"},
		secretRow{flag: "password-stdin", name: "a password typed after a padded flag word gives no position", argv: line("create user bob --password-stdin", " -"+c), code: validation, message: "unknown flag"},
		secretRow{flag: "password-stdin", name: "a password typed after three dashes gives no word", argv: line("create user bob --password-stdin", "---"+c), code: validation, message: "unknown flag"},
		secretRow{flag: "password-stdin", name: "an unknown flag typed at a group before the command name is named by position", argv: line("create -" + c + " user bob --password-stdin"), code: validation, message: "unknown flag at argument 4 after rancher"},
		secretRow{flag: "password-stdin", name: "an unknown flag typed before the group name is named by position", argv: line("-" + c + " create user bob --password-stdin"), code: validation, message: "unknown flag at argument 3 after rancher"},
		secretRow{flag: "url", name: "an unknown flag typed before a command with a secret input is named by position", argv: line("-" + c + " config add-server"), code: validation, message: "unknown flag at argument 3 after rancher"},
		secretRow{flag: "password-stdin", name: "a value for a flag typed before the group name is not named", argv: line("--debug=" + c + " create user bob --password-stdin"), code: validation, message: "invalid value for flag -debug: the value must be true or false"},
		secretRow{flag: "password-stdin", name: "a value for a flag typed at a group is not named", argv: line("create --debug=" + c + " user bob --password-stdin"), code: validation, message: "invalid value for flag -debug: the value must be true or false"},
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
	rows = append(rows, secretURLRows()...)
	return rows
}

// currentPasswordRows are the rows of create password-change-request that
// send two lines on stdin or read Rancher's responses to a create request,
// whether the command maps them or not: each asserts the submitted values are
// in no output. A row whose input
// holds the current password on one line and the new password on the other
// runs twice, with the canary on each line.
func currentPasswordRows() []secretRow {
	c := secretCanary
	const (
		fixedCurrent = "currentValue1234"
		fixedNew     = "newPassword1234"
	)
	line := func(l string, more ...string) []string { return append(strings.Fields(l), more...) }
	validation := string(clierr.CodeValidation)
	both := []string{"--current-password-stdin", "--password-stdin"}
	var rows []secretRow
	add := func(r ...secretRow) { rows = append(rows, r...) }
	// onEachLine adds the row twice: stdin holds the canary as the current
	// password, then as the new password, the other line a fixed value.
	onEachLine := func(r secretRow) {
		for _, l := range []struct{ where, stdin string }{{"the canary on line 1", c + "\n" + fixedNew}, {"the canary on line 2", fixedCurrent + "\n" + c}} {
			row := r
			row.name, row.stdin = r.name+", "+l.where, l.stdin
			add(row)
		}
	}

	add(
		secretRow{flag: "password-stdin", name: passwordApplyLine + ": a 500 whose text is not mapped", argv: line(passwordApplyLine, "--password-stdin"), stdin: c,
			stub: currentPasswordStub(500, missingPasswordSecretBody), code: string(clierr.CodeGeneric), message: "failed to get password secret"},
		secretRow{flag: "password-stdin", name: passwordApplyLine + ": a 401 whose text is not mapped", argv: line(passwordApplyLine, "--password-stdin"), stdin: c,
			stub: currentPasswordStub(401, permissionsCheckBody), code: string(clierr.CodeAuth), message: "error checking permissions"},
		secretRow{flag: "password-stdin", name: passwordLine + ": one line, a dry-run", argv: line(passwordLine, "--password-stdin"), stdin: c, stub: currentPasswordStub(0, ""),
			noRefusal: true, instead: "/spec/newPassword"},
		secretRow{flag: "password-stdin", name: passwordApplyLine + ": one line, applied", argv: line(passwordApplyLine, "--password-stdin"), stdin: c, stub: currentPasswordStub(0, ""),
			noRefusal: true, instead: "PasswordChangeRequestCompleted"},
	)

	// Refusals before any request.
	const flagNeedsPipe = "needs --password-stdin"
	add(
		secretRow{flag: "current-password-stdin", name: passwordLine + ": the flag without --password-stdin", argv: line(passwordLine, "--current-password-stdin"), stdin: c + "\n" + fixedNew, code: validation, message: flagNeedsPipe},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": an empty stdin", argv: line(passwordLine, both...), code: validation, message: "stdin is empty"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": one line", argv: line(passwordLine, both...), stdin: c, code: validation, message: "lines found: 1"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": three lines, the canary on line 1", argv: line(passwordLine, both...), stdin: c + "\n" + fixedCurrent + "\n" + fixedNew, code: validation, message: "lines found: 3"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": three lines, the canary on line 3", argv: line(passwordLine, both...), stdin: fixedCurrent + "\n" + fixedNew + "\n" + c, code: validation, message: "lines found: 3"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": line 1 empty", argv: line(passwordLine, both...), stdin: "\n" + c, code: validation, message: "line 1 (the current password) is empty"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": line 2 empty", argv: line(passwordLine, both...), stdin: c + "\n\n", code: validation, message: "line 2 (the new password) is empty"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": the password typed after the flag with =", argv: line(passwordLine, "--current-password-stdin="+c, "--password-stdin"), code: validation, message: "invalid value for flag -current-password-stdin: the value must be true or false"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": the password typed as an argument", argv: line(passwordLine, "--current-password-stdin", "--password-stdin", c), stdin: fixedCurrent + "\n" + fixedNew, code: validation, message: "unexpected argument at position 2"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": the password typed after a flag that does not exist", argv: line(passwordLine, "--current-password", c), code: validation, message: "unknown flag at argument 7 after rancher"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": the password typed after a flag that does not exist, with =", argv: line(passwordLine, "--current-password="+c), code: validation, message: "unknown flag at argument 7 after rancher"},
	)

	// A refusal of the server before the apply: lines 1 and 2 of one input,
	// the canary standing for each in turn.
	short := shortSecretCanary
	add(
		secretRow{flag: "current-password-stdin", name: passwordLine + ": a new password the server refuses as too short, the canary on line 2", argv: line(passwordLine, both...), stdin: c + "\n" + short, stub: shortPasswordStub, canary: short, code: validation, message: "password must be at least 12 characters"},
		secretRow{flag: "current-password-stdin", name: passwordLine + ": a new password the server refuses as too short, the canary on line 1", argv: line(passwordLine, both...), stdin: c + "\n" + short, stub: shortPasswordStub, canary: c, code: validation, message: "password must be at least 12 characters"},
	)

	// Success: both passwords are echoed by the server and are in no output.
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordLine + ": both flags, a dry-run", argv: line(passwordLine, both...), stub: currentPasswordStub(0, ""),
		noRefusal: true, instead: "/spec/currentPassword"})
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordApplyLine + ": both flags, applied", argv: line(passwordApplyLine, both...), stub: currentPasswordStub(0, ""),
		noRefusal: true, instead: "PasswordChangeRequestCompleted"})

	// Rancher's two refusals: the canary is also in Rancher's text.
	planted := " " + c
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordApplyLine + ": a refused current password", argv: line(passwordApplyLine, both...), stub: currentPasswordStub(500, invalidCurrentPasswordBody(planted)),
		code: validation, message: "Rancher refused the current password", exit: clierr.ExitCode(clierr.CodeValidation)})
	add(secretRow{flag: "password-stdin", name: passwordApplyLine + ": a refused current password, one line", argv: line(passwordApplyLine, "--password-stdin"), stdin: c, stub: currentPasswordStub(500, invalidCurrentPasswordBody(planted)),
		code: validation, message: "Rancher requires the current password", exit: clierr.ExitCode(clierr.CodeValidation)})
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordApplyLine + ": another user's password", argv: line(passwordApplyLine, both...), stub: currentPasswordStub(401, notAuthorizedBody(planted)),
		code: string(clierr.CodeRBAC), message: "you can change only your own password", exit: clierr.ExitCode(clierr.CodeRBAC)})

	// A response the command does not map prints Rancher's text as before.
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordApplyLine + ": a 500 whose text is not mapped", argv: line(passwordApplyLine, both...), stub: currentPasswordStub(500, missingPasswordSecretBody),
		code: string(clierr.CodeGeneric), message: "failed to get password secret"})
	onEachLine(secretRow{flag: "current-password-stdin", name: passwordApplyLine + ": a 401 whose text is not mapped", argv: line(passwordApplyLine, both...), stub: currentPasswordStub(401, permissionsCheckBody),
		code: string(clierr.CodeAuth), message: "error checking permissions"})

	// A newline inside a password piped to --password-stdin is refused, with
	// the remedy only on the command that has it.
	add(
		secretRow{flag: "password-stdin", name: createUserLine + ": a newline inside the password", argv: line(createUserLine, "--password-stdin"), stdin: "abc\n" + c, code: validation,
			message: "has more than one line", forbid: []string{"--current-password-stdin"}},
		secretRow{flag: "password-stdin", name: passwordLine + ": a newline inside the password", argv: line(passwordLine, "--password-stdin"), stdin: "abc\n" + c, code: validation,
			message: "has more than one line", contains: []string{"pass --current-password-stdin"}},
	)
	return rows
}

// secretFlagPattern matches the names of the flags and positional arguments
// that may take a secret value, or a file or stdin that holds one. A name it
// matches by mistake (a future bypass-cache) gets an entry in secretExempt, or
// in secretArgExempt for an argument, with a reason.
var secretFlagPattern = regexp.MustCompile(`secret|password|token|credential|literal|set|patch|stdin|key|file|filename|url|server|cacert|pass|pw`)

// secretExempt names the flags the pattern matches whose value is never a
// secret, with the reason.
var secretExempt = map[string]string{
	"cloud-credential":     "the name of a cloud credential, never its content",
	"must-change-password": "a bool",
	"output-file":          "a path the command writes to",
	"clear-cacert":         "a bool",
	"reset-values":         "a bool",
	"token-ttl":            "a duration",
	"non-resource-url":     "a request path of the Kubernetes API on the server, never a URL with userinfo",
}

// secretArgExempt names the positional arguments the pattern matches whose
// value is never a secret, with the reason.
var secretArgExempt = map[string]string{
	"credential":  "the id or display name of a cloud credential, never its content",
	"secret-name": "the name of an oidc-client secret, never its value",
}

// secretValueFlags lists each flag that has a row in secretRows and is not a
// bool ending in -stdin, with the reason it may take a value. A reason does
// not say the value is never a secret. It says what the value is: the form
// kubectl uses, public data (a path, a name, an id), or a value the command
// refuses without printing it.
var secretValueFlags = map[string]string{
	"from-file":        "a field=path pair; the path is read as a file or as stdin and the secret never appears on the command line; a malformed argument is refused without echoing it",
	"from-literal":     "key=value pairs of Secret data in the form kubectl create secret --from-literal uses; a malformed value is refused without echoing it; to keep the data off the command line, use create resource -f - and send the Secret on stdin",
	"patch":            "a JSON patch given inline, as kubectl does; to keep it off the command line, use --patch-file - and send the patch on stdin",
	"set":              "a field=value pair; a credential field is refused",
	"url":              "a server URL; one that holds a user name or password is refused",
	"server":           "a server URL; one that holds a user name or password is refused",
	"cacert":           "the path or PEM text of a CA certificate, public data",
	"cacerts":          "the path or PEM text of CA certificates, public data",
	"filename":         "a path, or - for stdin",
	"patch-file":       "a path, or - for stdin",
	"credential-store": "the name of a credential store, or exec:<name>",
	"user-id":          "a Rancher user id",
	"auth-provider":    "an auth config name, or an auth provider type",
}

// secretNameGaps returns one problem per name in uses that pattern matches
// and neither rows nor exempt covers; one per row that matches no name in
// uses; one per exempt entry that matches no name; one per exempt entry that
// matches a name and has no reason; and one per name that has a row and an
// exempt entry with a reason. A name that several commands have is reported
// once, with the first command in uses that has it. kind and dashes spell a
// name in a problem: "flag" with "--", "environment variable" with "".
func secretNameGaps(kind, dashes string, uses []nameUse, pattern *regexp.Regexp, rows map[string]bool, exempt map[string]string) []string {
	seen := map[string]bool{}
	var problems []string
	for _, u := range uses {
		if seen[u.name] {
			continue
		}
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

// nameUse is a flag or environment variable, its type
// (empty for an environment variable), and the command or list that has it.
type nameUse struct{ name, typ, owner string }

// globalFlagsOwner is the text a problem uses where it names the command, for
// a global flag.
const globalFlagsOwner = "the global flags"

// secretFlagUses returns each flag of m with its type and the command that has
// it: first the flags of each command in manifest order, then the global
// flags, each under its long name (the text after the last "--": config for
// -c/--config). A global flag whose name has no text after "--" is returned
// as a problem.
func secretFlagUses(m runtime.Manifest) (uses []nameUse, problems []string) {
	for _, c := range m.Commands {
		for _, f := range c.Flags {
			uses = append(uses, nameUse{f.Name, f.Type, c.Name})
		}
	}
	for _, g := range m.GlobalFlags {
		_, name, found := strings.CutLast(g.Name, "--")
		if !found || name == "" {
			problems = append(problems, "the global flag "+g.Name+" has no name after --; the scan cannot read it")
			continue
		}
		uses = append(uses, nameUse{name, g.Type, globalFlagsOwner})
	}
	return uses, problems
}

// secretFlagGaps runs secretNameGaps on the flags of every command and the
// global flags of m, and adds the problems secretFlagUses found.
func secretFlagGaps(m runtime.Manifest, rows map[string]bool, exempt map[string]string) []string {
	uses, problems := secretFlagUses(m)
	problems = append(problems, secretNameGaps("flag", "--", uses, secretFlagPattern, rows, exempt)...)
	slices.Sort(problems)
	return problems
}

// secretValueFlagGaps reports: (1) a flag with a row that is not a -stdin bool
// and has no entry in valueFlags; (2) a flag whose name ends in -stdin and is
// not a bool, whether or not it has a row, once per name and type; (3) an
// entry in valueFlags that no flag with a row matches; (4) an entry with no
// reason; (5) an entry whose name is also in exempt; (6) an entry for a -stdin
// bool. (2) is reported once for each name and type, and replaces (1) for that
// name and type. Items (1) and (3) to (6) give a name one problem, the first
// that applies; (1) names the first command that has the flag (or the global
// flags), and (3) to (6) name no command.
func secretValueFlagGaps(m runtime.Manifest, rows map[string]bool, valueFlags, exempt map[string]string) []string {
	uses, problems := secretFlagUses(m)
	seen := map[string]bool{}
	stdinBool := map[string]bool{}
	nonBoolStdin := map[string]bool{}
	for _, u := range uses {
		seen[u.name] = true
		stdin := strings.HasSuffix(u.name, "-stdin")
		if stdin && u.typ == "bool" {
			stdinBool[u.name] = true
		}
		if stdin && u.typ != "bool" && !nonBoolStdin[u.name+" "+u.typ] {
			nonBoolStdin[u.name+" "+u.typ] = true
			problems = append(problems, "flag --"+u.name+" of "+u.owner+" is a "+u.typ+"; a flag whose name ends in -stdin must be a bool")
		}
	}
	reported := map[string]bool{}
	for _, u := range uses {
		if !rows[u.name] || reported[u.name] || nonBoolStdin[u.name+" "+u.typ] {
			continue
		}
		if _, listed := valueFlags[u.name]; listed {
			continue
		}
		if u.typ == "bool" && strings.HasSuffix(u.name, "-stdin") {
			continue
		}
		reported[u.name] = true
		problems = append(problems, "flag --"+u.name+" of "+u.owner+" (a "+u.typ+") has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin")
	}
	for name, reason := range valueFlags {
		_, exempted := exempt[name]
		switch {
		case !seen[name] || !rows[name]:
			problems = append(problems, "secretValueFlags has an entry for --"+name+", but no flag with a row has that name")
		case reason == "":
			problems = append(problems, "the entry for --"+name+" in secretValueFlags has no reason")
		case exempted:
			problems = append(problems, "flag --"+name+" has an entry in secretValueFlags and in secretExempt")
		case stdinBool[name]:
			problems = append(problems, "secretValueFlags has an entry for --"+name+", a bool ending in -stdin, which takes no value")
		}
	}
	slices.Sort(problems)
	return problems
}

// secretArgGaps returns one problem for each positional argument name in m
// that pattern matches and exempt does not list; one for each entry of exempt
// that names no argument; and one for each entry that names an argument and
// has no reason. A name is reported once, with the first command that has it.
func secretArgGaps(m runtime.Manifest, pattern *regexp.Regexp, exempt map[string]string) []string {
	seen := map[string]bool{}
	var problems []string
	for _, c := range m.Commands {
		for _, a := range c.Args {
			if seen[a.Name] {
				continue
			}
			seen[a.Name] = true
			if _, exempted := exempt[a.Name]; pattern.MatchString(a.Name) && !exempted {
				problems = append(problems, "argument "+a.Name+" of "+c.Name+" matches the secret pattern and has no exempt entry")
			}
		}
	}
	for name, reason := range exempt {
		switch {
		case !seen[name]:
			problems = append(problems, "the exempt list has an entry for argument "+name+", but no command has an argument with that name")
		case reason == "":
			problems = append(problems, "the exempt entry for argument "+name+" has no reason")
		}
	}
	slices.Sort(problems)
	return problems
}

// secretEnvPattern matches the names of the environment variables that may
// hold a secret.
var secretEnvPattern = regexp.MustCompile(`(?i)secret|password|token|credential|key|url`)

// secretEnvExempt names the environment variables the pattern matches whose
// value is never a secret, with the reason.
var secretEnvExempt = map[string]string{}

// secretEnvGaps is secretNameGaps over the environment variables vars.
func secretEnvGaps(vars []runtime.EnvVar, rows map[string]bool, exempt map[string]string) []string {
	uses := make([]nameUse, len(vars))
	for i, v := range vars {
		uses[i] = nameUse{name: v.Name, owner: "the environment list"}
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
	assert.Equal(t, "flag --set of create cloud-credential has no row and no exempt entry", problems[0])
}

// manifestWithFlag returns a copy of m whose first command has the flag f in
// addition to the flags it already has.
func manifestWithFlag(m runtime.Manifest, f runtime.ArgSpec) runtime.Manifest {
	return manifestWithFlagOn(m, f, 0)
}

// manifestWithFlagOn returns a copy of m whose commands at the indexes have
// the flag f in addition to the flags they already have.
func manifestWithFlagOn(m runtime.Manifest, f runtime.ArgSpec, indexes ...int) runtime.Manifest {
	out := m
	out.Commands = slices.Clone(m.Commands)
	for _, i := range indexes {
		out.Commands[i].Flags = append(slices.Clone(out.Commands[i].Flags), f)
	}
	return out
}

// manifestWithGlobalFlag returns a copy of m with the global flag g added.
func manifestWithGlobalFlag(m runtime.Manifest, g runtime.GlobalFlag) runtime.Manifest {
	out := m
	out.GlobalFlags = append(slices.Clone(m.GlobalFlags), g)
	return out
}

// TestSecretFlagScanCoversPassPwAndGlobalFlags asserts that the scan reports
// a flag whose name contains pass but none of the other pattern words, and one
// that contains pw but none of the others; accepts a match by mistake that has
// an exempt entry with a reason; reports a global flag with no row; reports
// the same flag with no row on two commands once; reads each global flag under
// its long name; and reports a global flag whose name has no --. It also
// asserts that the real command tree has no problem.
func TestSecretFlagScanCoversPassPwAndGlobalFlags(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	rows := rowFlags(secretRows())
	first := m.Commands[0].Name

	tests := []struct {
		name    string
		m       runtime.Manifest
		exempt  map[string]string
		want    []string
		wantNil bool
	}{
		{
			name:    "the real command tree has no problem",
			m:       m,
			exempt:  secretExempt,
			wantNil: true,
		},
		{
			name:   "a flag name that contains pass and no other pattern word",
			m:      manifestWithFlag(m, runtime.ArgSpec{Name: "passwd", Type: "string"}),
			exempt: secretExempt,
			want:   []string{"flag --passwd of " + first + " has no row and no exempt entry"},
		},
		{
			name:   "a flag name that contains pw and no other pattern word",
			m:      manifestWithFlag(m, runtime.ArgSpec{Name: "pwd", Type: "string"}),
			exempt: secretExempt,
			want:   []string{"flag --pwd of " + first + " has no row and no exempt entry"},
		},
		{
			name:    "a match by mistake with an exempt entry that has a reason",
			m:       manifestWithFlag(m, runtime.ArgSpec{Name: "bypass-cache", Type: "string"}),
			exempt:  withEntry(secretExempt, "bypass-cache", "a cache switch, never a value that holds a secret"),
			wantNil: true,
		},
		{
			name:   "a global flag with no row",
			m:      manifestWithGlobalFlag(m, runtime.GlobalFlag{Name: "--token-x", Type: "string", AppliesTo: "all"}),
			exempt: secretExempt,
			want:   []string{"flag --token-x of the global flags has no row and no exempt entry"},
		},
		{
			name:   "the same flag with no row on two commands is one problem",
			m:      manifestWithFlagOn(m, runtime.ArgSpec{Name: "passwd", Type: "string"}, 0, 1),
			exempt: secretExempt,
			want:   []string{"flag --passwd of " + first + " has no row and no exempt entry"},
		},
		{
			name:   "a global flag with no --",
			m:      manifestWithGlobalFlag(m, runtime.GlobalFlag{Name: "-x", Type: "string", AppliesTo: "all"}),
			exempt: secretExempt,
			want:   []string{"the global flag -x has no name after --; the scan cannot read it"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := secretFlagGaps(tt.m, rows, tt.exempt)
			if tt.wantNil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}

	uses, problems := secretFlagUses(m)
	assert.Empty(t, problems)
	for _, name := range []string{"project", "config", "output"} {
		assert.Contains(t, uses, nameUse{name: name, typ: globalTypeOf(t, m, name), owner: globalFlagsOwner}, "the global flag --%s is not read under its long name", name)
	}
	assert.Empty(t, secretFlagGaps(m, rows, secretExempt), "the scan reports a problem on the real command tree")
}

// globalTypeOf returns the type of the global flag whose long name is name.
func globalTypeOf(t *testing.T, m runtime.Manifest, name string) string {
	t.Helper()
	for _, g := range m.GlobalFlags {
		if _, after, found := strings.CutLast(g.Name, "--"); found && after == name {
			return g.Type
		}
	}
	require.FailNow(t, "no global flag named --"+name)
	return ""
}

// withEntry returns a copy of m with the entry key set to value.
func withEntry(m map[string]string, key, value string) map[string]string {
	out := maps.Clone(m)
	out[key] = value
	return out
}

// TestEverySecretFlagWithARowIsAStdinBoolOrListed asserts that every flag with
// a row is a bool ending in -stdin or has an entry in secretValueFlags that
// gives the reason it may take a value, and that no flag ending in -stdin has
// another type.
func TestEverySecretFlagWithARowIsAStdinBoolOrListed(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Empty(t, secretValueFlagGaps(m, rowFlags(secretRows()), secretValueFlags, secretExempt))
}

// TestSecretValueFlagScanFailsOnEachMistake asserts that the scan reports each
// of these: a flag with a row that is not a -stdin bool and has no entry; a
// -stdin flag that is not a bool; an entry that no flag with a row matches; an
// entry with no reason; an entry that is also in the exempt list; an entry for
// a -stdin bool. It also asserts that the real command tree has no problem.
func TestSecretValueFlagScanFailsOnEachMistake(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	rows := rowFlags(secretRows())
	first := m.Commands[0].Name
	withRow := func(name string) map[string]bool {
		out := maps.Clone(rows)
		out[name] = true
		return out
	}
	allStringStdin := m
	allStringStdin.Commands = slices.Clone(m.Commands)
	var firstPasswordStdin string
	for i, c := range allStringStdin.Commands {
		allStringStdin.Commands[i].Flags = slices.Clone(c.Flags)
		for j, f := range allStringStdin.Commands[i].Flags {
			if f.Name == "password-stdin" {
				allStringStdin.Commands[i].Flags[j].Type = "string"
				if firstPasswordStdin == "" {
					firstPasswordStdin = c.Name
				}
			}
		}
	}
	require.NotEmpty(t, firstPasswordStdin)
	// twoTypes has password-stdin as a string in the first command that has
	// it and as a stringSlice in the others.
	twoTypes := m
	twoTypes.Commands = slices.Clone(m.Commands)
	var stdinOwners []string
	for i, c := range twoTypes.Commands {
		twoTypes.Commands[i].Flags = slices.Clone(c.Flags)
		for j, f := range twoTypes.Commands[i].Flags {
			if f.Name == "password-stdin" {
				twoTypes.Commands[i].Flags[j].Type = []string{"string", "stringSlice"}[min(len(stdinOwners), 1)]
				stdinOwners = append(stdinOwners, c.Name)
			}
		}
	}
	require.GreaterOrEqual(t, len(stdinOwners), 2)
	noServer := maps.Clone(secretValueFlags)
	delete(noServer, "server")
	var firstServer string
	for _, c := range m.Commands {
		if firstServer == "" && slices.ContainsFunc(c.Flags, func(f runtime.ArgSpec) bool { return f.Name == "server" }) {
			firstServer = c.Name
		}
	}
	require.NotEmpty(t, firstServer)

	tests := []struct {
		name        string
		m           runtime.Manifest
		rows        map[string]bool
		valueFlags  map[string]string
		exempt      map[string]string
		wantProblem string
		// wantProblems, when set, lists every expected problem; wantProblem is
		// then not used.
		wantProblems []string
	}{
		{
			name: "the real command tree has no problem", m: m, rows: rows, valueFlags: secretValueFlags, exempt: secretExempt,
		},
		{
			name: "a string flag current-password with a row and no entry",
			m:    manifestWithFlag(m, runtime.ArgSpec{Name: "current-password", Type: "string"}), rows: withRow("current-password"), valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --current-password of " + first + " (a string) has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin",
		},
		{
			name: "a string flag passwd with a row and no entry",
			m:    manifestWithFlag(m, runtime.ArgSpec{Name: "passwd", Type: "string"}), rows: withRow("passwd"), valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --passwd of " + first + " (a string) has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin",
		},
		{
			name: "every password-stdin flag is a string",
			m:    allStringStdin, rows: rows, valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --password-stdin of " + firstPasswordStdin + " is a string; a flag whose name ends in -stdin must be a bool",
		},
		{
			name: "a string flag whose name ends in -stdin with no row",
			m:    manifestWithFlag(m, runtime.ArgSpec{Name: "x-stdin", Type: "string"}), rows: rows, valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --x-stdin of " + first + " is a string; a flag whose name ends in -stdin must be a bool",
		},
		{
			name: "one -stdin name with two types gives one problem for each type",
			m:    twoTypes, rows: rows, valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblems: []string{
				"flag --password-stdin of " + stdinOwners[0] + " is a string; a flag whose name ends in -stdin must be a bool",
				"flag --password-stdin of " + stdinOwners[1] + " is a stringSlice; a flag whose name ends in -stdin must be a bool",
			},
		},
		{
			name: "a global string flag token with a row and no entry",
			m:    manifestWithGlobalFlag(m, runtime.GlobalFlag{Name: "--token", Type: "string", AppliesTo: "all"}), rows: withRow("token"), valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --token of the global flags (a string) has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin",
		},
		{
			name: "the entry for --server removed",
			m:    m, rows: rows, valueFlags: noServer, exempt: secretExempt,
			wantProblem: "flag --server of " + firstServer + " (a string) has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin",
		},
		{
			name: "an entry that no flag with a row matches",
			m:    m, rows: rows, valueFlags: withEntry(secretValueFlags, "gone-flag", "a flag that no longer exists"), exempt: secretExempt,
			wantProblem: "secretValueFlags has an entry for --gone-flag, but no flag with a row has that name",
		},
		{
			name: "an entry with an empty reason",
			m:    m, rows: rows, valueFlags: withEntry(secretValueFlags, "server", ""), exempt: secretExempt,
			wantProblem: "the entry for --server in secretValueFlags has no reason",
		},
		{
			name: "an entry that is also exempt",
			m:    m, rows: rows, valueFlags: secretValueFlags, exempt: withEntry(secretExempt, "server", "an entry that also has a value reason"),
			wantProblem: "flag --server has an entry in secretValueFlags and in secretExempt",
		},
		{
			name: "a bool not ending in -stdin with a row and no entry",
			m:    manifestWithFlag(m, runtime.ArgSpec{Name: "keep-secret", Type: "bool"}), rows: withRow("keep-secret"), valueFlags: secretValueFlags, exempt: secretExempt,
			wantProblem: "flag --keep-secret of " + first + " (a bool) has a row in secretRows and no entry in secretValueFlags; add an entry with a reason, or make it a bool ending in -stdin",
		},
		{
			name: "an entry for a bool ending in -stdin",
			m:    m, rows: rows, valueFlags: withEntry(secretValueFlags, "password-stdin", "an unneeded entry"), exempt: secretExempt,
			wantProblem: "secretValueFlags has an entry for --password-stdin, a bool ending in -stdin, which takes no value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := secretValueFlagGaps(tt.m, tt.rows, tt.valueFlags, tt.exempt)
			switch {
			case tt.wantProblems != nil:
				assert.Equal(t, tt.wantProblems, got)
			case tt.wantProblem == "":
				assert.Empty(t, got)
			default:
				assert.Equal(t, []string{tt.wantProblem}, got)
			}
		})
	}
}

// TestEverySecretArgumentHasAnExemptEntry asserts that every positional
// argument of the built command tree whose name matches the secret pattern
// has an exempt entry with a reason, and that no entry names an argument no
// command has.
func TestEverySecretArgumentHasAnExemptEntry(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Empty(t, secretArgGaps(m, secretFlagPattern, secretArgExempt))
}

// TestSecretArgumentScanFailsOnEachMistake asserts that the scan reports a
// positional argument that matches the pattern and has no entry; the same
// argument on two commands once; an entry that names no argument; and an entry
// with no reason. It also asserts that the real command tree has no problem.
func TestSecretArgumentScanFailsOnEachMistake(t *testing.T) {
	t.Parallel()
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	assert.Empty(t, secretArgGaps(m, secretFlagPattern, secretArgExempt))

	withArg := m
	withArg.Commands = slices.Clone(m.Commands)
	withArg.Commands[0].Args = append(slices.Clone(withArg.Commands[0].Args), runtime.ArgSpec{Name: "password", Type: "string"})
	assert.Equal(t, []string{"argument password of " + m.Commands[0].Name + " matches the secret pattern and has no exempt entry"},
		secretArgGaps(withArg, secretFlagPattern, secretArgExempt))

	onTwo := m
	onTwo.Commands = slices.Clone(m.Commands)
	for _, i := range []int{0, 1} {
		onTwo.Commands[i].Args = append(slices.Clone(onTwo.Commands[i].Args), runtime.ArgSpec{Name: "password", Type: "string"})
	}
	assert.Equal(t, []string{"argument password of " + m.Commands[0].Name + " matches the secret pattern and has no exempt entry"},
		secretArgGaps(onTwo, secretFlagPattern, secretArgExempt), "the same argument on two commands is one problem")

	assert.Equal(t, []string{"the exempt list has an entry for argument gone-arg, but no command has an argument with that name"},
		secretArgGaps(m, secretFlagPattern, withEntry(secretArgExempt, "gone-arg", "an argument that no longer exists")))

	assert.Equal(t, []string{"the exempt entry for argument secret-name has no reason"},
		secretArgGaps(m, secretFlagPattern, withEntry(secretArgExempt, "secret-name", "")))
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
	wr, err := buildWiredRegistry("v-test")
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

// secretServer starts a TLS server that responds through stub, points the
// process at it with an empty config directory, the file credential store and
// a temporary working directory, and returns it. Nothing reaches a real
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
	secretSeen.reset()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("request: %s %s", r.Method, r.URL.RequestURI())
		secretSeen.add(r.Method + " " + r.URL.RequestURI())
		if stub != nil && stub(w, r) {
			return
		}
		rancher.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	secretServerURL = srv.URL
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
// typed as its own word before the command tree acts on it), with stdin as the
// command's standard input and a time bound, and returns stdout, stderr and the exit
// code.
func runWithStdin(t *testing.T, argv []string, stdin string) (stdout, stderr string, code int) {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
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
			stdout, stderr, code := runSecretRow(t, row)

			canary := cmp.Or(row.canary, secretCanary)
			without := func(text string) string {
				if row.tolerate == "" {
					return text
				}
				return strings.ReplaceAll(text, row.tolerate, "")
			}
			assertNoCanary(t, canary, map[string]string{"stdout": without(stdout), "stderr": without(stderr)})
			if row.noRefusal {
				for _, bad := range append([]string{"the server URL in"}, row.forbid...) {
					assert.NotContains(t, stdout+stderr, bad)
				}
				if row.instead != "" {
					assert.Contains(t, stdout+stderr, row.instead)
				}
				if row.reaches {
					assert.NotEmpty(t, secretSeen.all(), "the URL that won was used: a request reached the stub")
				}
				return
			}
			require.NotZero(t, code, "stdout: %s stderr: %s", stdout, stderr)
			ce := renderedError(t, "json", stderr)
			assert.Equal(t, row.code, string(ce.Code), "stderr: %s", stderr)
			if row.exit != 0 {
				assert.Equal(t, row.exit, code, "stderr: %s", stderr)
			}
			message := withoutRunDirectory(ce.Message)
			assert.Contains(t, message, row.message)
			for _, want := range row.contains {
				assert.Contains(t, message, want)
			}
			for _, bad := range row.forbid {
				assert.NotContains(t, message, bad)
			}
			if row.hint != "" {
				assert.Equal(t, row.hint, ce.Hint)
			}
			assertNoCanary(t, canary, map[string]string{"code": ce.Code, "message": without(ce.Message), "hint": ce.Hint})
		})
	}
}

// withoutRunDirectory removes from text the words a remedy names the run's
// config directory with, exactly `--config '<directory>' ` for the directory
// of this run: the rows state each remedy once, and a remedy that named
// another directory would keep its words and fail the row. The canary checks
// read the text as it was written.
func withoutRunDirectory(text string) string {
	return strings.ReplaceAll(text, "--config "+clierr.ShellQuote(remedyDirFor(os.Getenv(config.EnvConfigDir)))+" ", "")
}

// runSecretRow runs the command line of row in a fresh stub server and
// config directory, as the table does, and returns what it wrote and its exit
// code. Not parallel: it sets the process environment.
func runSecretRow(t *testing.T, row secretRow) (stdout, stderr string, code int) {
	t.Helper()
	srv := secretServer(t, row.stub, secretBearer)
	fill := secretFiller(t, srv)
	for k, v := range row.setEnv {
		t.Setenv(k, fill(v))
	}
	for name, content := range row.files {
		require.NoError(t, os.WriteFile(filepath.Join(".", name), []byte(content), 0o600))
	}
	prepareSecretRow(t, srv, row, fill)
	for _, line := range row.before {
		stdout, stderr, code := runWithStdin(t, append([]string{"-o", "json"}, fillAll(fill, line)...), "")
		require.Zero(t, code, "before %v: stdout: %s stderr: %s", line, stdout, stderr)
	}
	argv := append([]string{"-o", "json"}, fillAll(fill, row.argv)...)
	return runWithStdin(t, argv, row.stdin)
}

// TestUsageErrorTextLeavesTheValueOutForASecretInput asserts that the text
// of a flag-parse error quotes the flag's value for an ordinary command, and
// names only the flag, and keeps only a reason that quotes nothing, for a
// command that takes a secret input, whatever the value holds.
func TestUsageErrorTextLeavesTheValueOutForASecretInput(t *testing.T) {
	t.Parallel()
	ordinary := &Command{Name: "a", descriptor: &runtime.Descriptor{Name: "a"}}
	secret := &Command{Name: "b", descriptor: &runtime.Descriptor{Name: "b", SecretInput: true}}
	undescribed := &Command{Name: "c"}

	for _, tc := range []struct {
		name             string
		err              usageError
		ordinary, secret string
	}{
		{"a flag given twice", usageError{class: repeatedFlag, flag: "from-literal", value: "k=v"}, `invalid value "k=v" for flag -from-literal: can't duplicate this flag`, "invalid value for flag -from-literal: can't duplicate this flag"},
		{"a bool flag with a value", usageError{class: badBoolValue, flag: "password-stdin", value: "pw"}, `invalid value "pw" for flag -password-stdin: parse error`, "invalid value for flag -password-stdin: the value must be true or false"},
		{"a value that holds the text around it", usageError{class: badBoolValue, flag: "z", value: "x for flag -y: parse error"}, `invalid value "x for flag -y: parse error" for flag -z: parse error`, "invalid value for flag -z: the value must be true or false"},
		{"a flag typed without its value, which the command defines", usageError{class: missingFlagValue, word: "-p"}, "flag needs an argument: -p", "flag needs an argument: -p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.ordinary, usageErrorText(ordinary, tc.err))
			wantUndescribed := tc.ordinary
			if tc.err.class == repeatedFlag || tc.err.class == badBoolValue {
				wantUndescribed = tc.secret
			}
			assert.Equal(t, wantUndescribed, usageErrorText(undescribed, tc.err), "the root or a group names the flag and not its value, and passes every other text")
			assert.Equal(t, tc.secret, usageErrorText(secret, tc.err))
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
	assert.Equal(t, "unknown flag at argument 6 after rancher", unknownFlagText("password", argv))
	assert.Equal(t, "unknown flag at argument 7 after rancher", unknownFlagText("s3cretcanary8", argv))
	assert.Equal(t, "unknown flag", unknownFlagText("absent", argv))
	assert.Equal(t, "unknown flag", unknownFlagText("bob", argv), "a word with no dash is not a flag")
	assert.Equal(t, "unknown flag", unknownFlagText("x", []string{" -x"}), "a word padded with a space gives no position")
	assert.Equal(t, "unknown flag", unknownFlagText("-x", []string{"---x"}), "a word with three dashes gives no position")

	ordinary := &Command{Name: "a", descriptor: &runtime.Descriptor{Name: "a"}}
	assert.Equal(t, "flag provided but not defined: -x", usageErrorText(ordinary, usageError{class: undefinedFlag, word: "x"}))
}

// leafNamed returns the name of the descriptor of the leaf, or "" for nil.
func leafNamed(leaf *Command) string {
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
// found, and the text that names the word when the leaf has a descriptor that takes none.
func TestUnknownFlagTextAtResolvesTheLeafOfTheLine(t *testing.T) {
	t.Parallel()
	leaf := func(name string, d *runtime.Descriptor) *Command { return &Command{Name: name, descriptor: d} }
	described := func(secret bool) *runtime.Descriptor {
		return &runtime.Descriptor{Name: "x", SecretInput: secret}
	}
	root := link(&Command{Name: "rancher", StopAtFirstWord: true, Commands: []*Command{
		leaf("secret", described(true)),
		leaf("plain", described(false)),
		leaf("bare", nil),
		{Name: "group", StopAtFirstWord: true, Commands: []*Command{leaf("secret", described(true)), leaf("bare", nil)}},
	}})
	const msg = "flag provided but not defined: -x"
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a leaf that takes a secret input", []string{"-x", "secret"}, "unknown flag at argument 1 after rancher"},
		{"a leaf that takes no secret input", []string{"-x", "plain"}, msg},
		{"a leaf with no descriptor", []string{"-x", "bare"}, "unknown flag at argument 1 after rancher"},
		{"a leaf of a group, with a secret input", []string{"group", "-x", "secret"}, "unknown flag at argument 2 after rancher"},
		{"a leaf of a group, with no descriptor", []string{"group", "-x", "bare"}, "unknown flag at argument 2 after rancher"},
		{"no leaf", []string{"-x"}, "unknown flag at argument 1 after rancher"},
		{"no leaf, at a group", []string{"group", "-x"}, "unknown flag at argument 2 after rancher"},
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
// or no command is named, and keeps the text "flag provided but not
// defined", with the flag as typed, for a command that takes none, at the
// root and at a group; a typo of such a flag still reads. The canary is the flag word of the first row.
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
		{"no command is named", []string{"-" + secretCanary}, "unknown flag at argument 3 after rancher", true},
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
	wr, err := buildWiredRegistry("v-test")
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
		{"an unknown command names the word and not the flag after it", []string{"nosuch", "--" + c}, `unknown command "nosuch"`, true},
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
