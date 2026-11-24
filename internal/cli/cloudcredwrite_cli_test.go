package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

const (
	ccCanary    = "fake-cc-canary-secret-0001"
	ccLineOne   = "fake-cc-first-line-0001"
	ccLineTwo   = "fake-cc-second-line-0002"
	ccTabLine   = "fake-cc-second\tline-0002"
	ccKube      = "apiVersion: v1\nclusters:\n- name: c\nusers:\n- name: u\n  user:\n    token: kubeconfig-user-x:" + ccCanary + "\n"
	ccNodeDrv   = "/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers"
	ccSecrets   = "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"
	ccReviews   = "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	ccSettings  = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/kev2-operators"
	ccNormanURL = "/v3/cloudcredentials"
	// ccItemURL is the Norman route of the stored credential cc-abcde, which an
	// update and a delete name.
	ccItemURL = "/v3/cloudCredentials/cattle-global-data:cc-abcde"
	// ccAnnotation is a stored annotation value, sent back by an update with a
	// new name or description, that no output may show.
	ccAnnotation = "fake-cc-annotation-0001"
	// ccStoredData is the decoded value of a data key of the stored credential,
	// which the CLI never keeps or shows.
	ccStoredData = "fake-cc-stored-data-0001"
)

// ccClusterPaths are the paths of the lists of provisioning clusters and of
// management clusters, through the Kubernetes API and through Steve.
var ccClusterPaths = []string{
	"/k8s/clusters/local/apis/provisioning.cattle.io/v1/clusters",
	"/k8s/clusters/local/apis/management.cattle.io/v3/clusters",
	"/v1/provisioning.cattle.io.clusters",
	"/v1/management.cattle.io.clusters",
}

// ccStoredSecret is the credential cc-abcde as the stub stores it; changed
// serves the next resourceVersion.
func ccStoredSecret(changed bool, driver string) string {
	rv := "7"
	if changed {
		rv = "8"
	}
	return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"cc-abcde","resourceVersion":"` + rv + `","annotations":{"field.cattle.io/name":"prod","field.cattle.io/description":"old","provisioning.cattle.io/driver":"aws","e2e/keep":"` + ccAnnotation + `"}},"data":{"` + driver + `credentialConfig-stored":"` + base64.StdEncoding.EncodeToString([]byte(ccStoredData)) + `"}}`
}

// ccDrivers is the node driver list the stub serves: one object per row of
// the driver table with its credential annotations.
var ccDrivers = []struct{ name, public, private, password, optional, defaults string }{
	{"amazonec2", "accessKey", "secretKey", "", "", ""},
	{"azure", "clientId,subscriptionId,environment,tenantId", "clientSecret", "", "tenantId", ""},
	{"digitalocean", "", "accessToken", "", "", ""},
	{"exoscale", "apiKey", "apiSecretKey", "", "", ""},
	{"google", "", "authEncodedJson", "", "", ""},
	{"harvester", "clusterType,clusterId", "kubeconfigContent", "", "clusterId", "clusterType:imported"},
	{"linode", "", "token", "", "", ""},
	{"nutanix", "endpoint,username,port", "password", "", "", ""},
	{"oci", "tenancyId,userId,fingerprint", "privateKeyContents", "", "", ""},
	{"otc", "accessKey,username", "secretKey,password,token", "", "", ""},
	{"openstack", "", "password", "", "", ""},
	{"packet", "", "apiKey", "", "", ""},
	{"pnap", "clientIdentifier,clientSecret", "", "clientSecret", "", ""},
	{"rackspace", "", "apiKey", "", "", ""},
	{"softlayer", "", "apiKey", "", "", ""},
	{"vmwarevsphere", "username,vcenter,vcenterPort", "password", "", "", "vcenterPort:443"},
	{"outscale", "accessKey,region", "secretKey", "", "", ""},
	{"twosecrets", "user", "keyOne,keyTwo", "", "", ""},
	{"publicpassword", "host,note", "", "note", "", ""},
}

// ccDormant is a node driver with credential fields that is not active.
var ccDormant = struct{ name, public, private string }{"dormant", "", "token"}

func ccDriverList(withDormant bool) string {
	items := make([]any, 0, len(ccDrivers)+1)
	if withDormant {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": ccDormant.name, "annotations": map[string]string{"privateCredentialFields": ccDormant.private}},
			"spec":     map[string]any{"displayName": ccDormant.name, "active": false},
		})
	}
	for _, d := range ccDrivers {
		ann := map[string]string{}
		for k, v := range map[string]string{"publicCredentialFields": d.public, "privateCredentialFields": d.private, "passwordFields": d.password, "optionalCredentialFields": d.optional, "defaults": d.defaults} {
			if v != "" {
				ann[k] = v
			}
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": d.name, "annotations": ann},
			"spec":     map[string]any{"displayName": d.name, "active": true},
		})
	}
	raw, _ := json.Marshal(map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "NodeDriverList", "items": items})
	return string(raw)
}

// ccNorman is how the stub responds to the create: the status and the body.
type ccNorman struct {
	status int
	body   string
	drop   bool
}

// ccOptions changes what the stub serves: an inactive node driver in the list,
// the alibaba operator marked inactive, a stored credential named "prod",
// or an access review that denies the create.
// With stored the stub has the credential cc-abcde, and with changed its read
// by name, the one after the list, responds with another resourceVersion.
type ccOptions struct {
	dormant, alibabaOff, existing, reviewDenied, stored, changed bool
	// clustersRefused makes the lists of provisioning and management clusters,
	// through the Kubernetes API and through Steve, respond with 403.
	clustersRefused bool
	// storedDriver is the driver of the stored credential; amazonec2 when empty.
	storedDriver string
}

func (o ccOptions) storedSecret(changed bool) string {
	driver := o.storedDriver
	if driver == "" {
		driver = "amazonec2"
	}
	return ccStoredSecret(changed, driver)
}

// ccStub responds to the reads and the review of the command, and to the create
// as norman says. The posts it saw are returned through *posts.
func ccStub(norman ccNorman, posts *[]string, listRefused bool) secretStub {
	return ccStubWith(ccOptions{}, norman, posts, listRefused)
}

// ccStubWith is ccStub with the changes of opts.
func ccStubWith(opts ccOptions, norman ccNorman, posts *[]string, listRefused bool) secretStub {
	alibaba := "true"
	if opts.alibabaOff {
		alibaba = "false"
	}
	stored := `{"apiVersion":"v1","kind":"SecretList","items":[]}`
	if opts.existing {
		stored = `{"apiVersion":"v1","kind":"SecretList","items":[{"metadata":{"name":"cc-other","annotations":{"field.cattle.io/name":"prod"}}}]}`
	}
	if opts.stored {
		stored = `{"apiVersion":"v1","kind":"SecretList","items":[` + opts.storedSecret(false) + `]}`
	}
	var deleted atomic.Bool
	return func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case opts.clustersRefused && slices.Contains(ccClusterPaths, r.URL.Path):
			writeKubeJSON(w, http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"clusters is forbidden: User cannot list"}`)
		case slices.Contains(ccClusterPaths[:2], r.URL.Path):
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"ClusterList","items":[]}`)
		case r.URL.Path == ccNodeDrv && r.Method == http.MethodGet:
			writeKubeJSON(w, http.StatusOK, ccDriverList(opts.dormant))
		case r.URL.Path == ccSettings:
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"management.cattle.io/v3","kind":"Setting","value":"[{\"name\":\"alibaba\",\"active\":`+alibaba+`}]"}`)
		case r.URL.Path == ccReviews:
			writeKubeJSON(w, http.StatusCreated, `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":`+strconv.FormatBool(!opts.reviewDenied)+`}}`)
		case r.URL.Path == ccSecrets && listRefused:
			writeKubeJSON(w, http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"secrets is forbidden: User cannot list"}`)
		case r.URL.Path == ccSecrets:
			writeKubeJSON(w, http.StatusOK, stored)
		case opts.stored && r.URL.Path == ccSecrets+"/cc-abcde" && deleted.Load():
			writeKubeJSON(w, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"secrets not found"}`)
		case opts.stored && r.URL.Path == ccSecrets+"/cc-abcde":
			writeKubeJSON(w, http.StatusOK, opts.storedSecret(opts.changed))
		case opts.stored && strings.HasPrefix(r.URL.Path, ccSecrets+"/"):
			writeKubeJSON(w, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"secrets not found"}`)
		case strings.HasPrefix(r.URL.Path, ccSecrets+"/"):
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"cc-abcde"},"data":{"amazonec2credentialConfig-accessKey":"eA=="}}`)
		case (r.URL.Path == ccNormanURL && r.Method == http.MethodPost) || (r.URL.Path == ccItemURL && (r.Method == http.MethodPut || r.Method == http.MethodDelete)):
			body := make([]byte, 1<<16)
			n, _ := r.Body.Read(body)
			*posts = append(*posts, string(body[:n]))
			if r.Method == http.MethodDelete && norman.status >= 200 && norman.status < 300 {
				deleted.Store(true)
			}
			if norman.drop {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return true
			}
			w.WriteHeader(norman.status)
			_, _ = w.Write([]byte(norman.body))
		default:
			return false
		}
		return true
	}
}

// ccRun runs argv with stdin and returns what the run wrote, and every file
// the run left in the config directory (the telemetry events included).
func ccRun(t *testing.T, stub secretStub, argv []string, stdin string) (stdout, stderr string, code int, files string) {
	t.Helper()
	secretServer(t, stub, secretBearer)
	stdout, stderr, code = runWithStdin(t, argv, stdin)
	var b strings.Builder
	_ = filepath.WalkDir(os.Getenv(config.EnvConfigDir), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(p)
			b.Write(raw)
		}
		return nil
	})
	return stdout, stderr, code, b.String()
}

// ccForms are the forms of a secret no output may have: as typed, JSON-escaped,
// base64 in every alignment secretinput's canaryForms lists, as output.Printable
// prints it (without the runes unsafe to display) and, as an apiserver prints a
// string value, the text between the quotes of each Go quoting function.
func ccForms(secret string) []string {
	forms := append([]string{secret}, canaryForms(secret)...)
	escaped, _ := json.Marshal(secret)
	forms = append(forms, strings.Trim(string(escaped), `"`), base64.StdEncoding.EncodeToString([]byte(secret)), clierr.WithoutUnsafeToDisplay(secret))
	for _, quoted := range []string{strconv.Quote(secret), strconv.QuoteToASCII(secret), strconv.QuoteToGraphic(secret)} {
		forms = append(forms, strings.Trim(quoted, `"`))
	}
	return forms
}

// ccStringLeaves lists every string of a decoded JSON value, keys excluded.
func ccStringLeaves(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			out = append(out, ccStringLeaves(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for _, e := range v {
			out = append(out, ccStringLeaves(e)...)
		}
		return out
	}
	return nil
}

// ccCase is one command line with what it must end in.
type ccCase struct {
	argv    []string
	stdin   string
	norman  ccNorman
	opts    ccOptions
	list    bool
	errCode string            // data.error.code of the error envelope; empty when the run succeeds
	message string            // the whole message of the error, asserted when set
	kind    string            // kind of the success envelope
	posts   int               // requests the Norman route saw
	sent    map[string]string // "<driver>credentialConfig.<field>" -> value in the request
	secrets []string          // every value a form of which no output may have
}

// ccEnableRecording turns telemetry recording on for the test that calls it.
func ccEnableRecording(t *testing.T) {
	t.Helper()
	t.Cleanup(telemetry.SetEnabledForTest())
	t.Cleanup(telemetry.SetDistinctSlotsForTest())
	t.Cleanup(telemetry.SetFixedClockForTest())
	t.Cleanup(telemetry.ResetServerForTest)
	t.Cleanup(resetRecordingState)
}

// ccNeedles lists the secrets of a case and each line of 4 or more
// characters of a multi-line one, as the filter splits it.
func ccNeedles(secrets []string) []string {
	var out []string
	for _, secret := range secrets {
		out = append(out, secret)
		if strings.ContainsAny(secret, "\r\n") {
			for _, line := range strings.FieldsFunc(secret, func(r rune) bool { return r == '\r' || r == '\n' }) {
				if len(line) >= 4 {
					out = append(out, line)
				}
			}
		}
	}
	return out
}

// ccCheck runs the case in JSON and in text and asserts what it ended in, that
// the request count and values show it reached its path, that recording left
// one event, and that no secret is in stdout, stderr, the decoded envelope, the
// files the run left or the decoded events.
func ccCheck(t *testing.T, c ccCase) {
	t.Helper()
	for _, asJSON := range []bool{true, false} {
		name := "text"
		argv := slices.Clone(c.argv)
		if asJSON {
			name, argv = "json", append(argv, "-o", "json")
		}
		t.Run(name, func(t *testing.T) {
			ccEnableRecording(t)
			var posts []string
			stdout, stderr, code, files := ccRun(t, ccStubWith(c.opts, c.norman, &posts, c.list), argv, c.stdin)
			dir := os.Getenv(config.EnvConfigDir)
			require.Len(t, posts, c.posts, "the requests the Norman route saw")
			if c.errCode == "" {
				require.Zero(t, code, stderr)
			} else {
				require.Equal(t, clierr.New(c.errCode, "x").ExitCode(), code, stderr)
			}
			var envelope struct {
				Kind string         `json:"kind"`
				Data map[string]any `json:"data"`
			}
			var decoded string
			switch {
			case asJSON && c.errCode == "":
				var listed struct {
					Kind string `json:"kind"`
					Data any    `json:"data"`
				}
				require.NoError(t, json.Unmarshal([]byte(stdout), &listed), stdout)
				assert.Equal(t, c.kind, listed.Kind)
				assert.NotNil(t, listed.Data)
				decoded = strings.Join(ccStringLeaves(listed.Data), "\n")
			case asJSON:
				require.NoError(t, json.Unmarshal([]byte(stderr), &envelope), stderr)
				errBody, _ := envelope.Data["error"].(map[string]any)
				assert.Equal(t, "Error", envelope.Kind)
				assert.Equal(t, c.errCode, errBody["code"])
				if c.message != "" {
					assert.Equal(t, c.message, errBody["message"])
				}
				decoded = strings.Join(ccStringLeaves(envelope.Data), "\n")
			case c.errCode != "":
				assert.Contains(t, stderr, "error ("+c.errCode)
				assert.Contains(t, stderr, c.message)
			}
			events := storedRuns(t, dir)
			require.Len(t, events, 1, "recording is on, so the run left one event")
			rawEvents, err := json.Marshal(events[0].Fields)
			require.NoError(t, err)
			for _, secret := range ccNeedles(c.secrets) {
				for _, form := range ccForms(secret) {
					assert.NotContains(t, stdout, form, "stdout")
					assert.NotContains(t, stderr, form, "stderr")
					assert.NotContains(t, decoded, form, "the decoded envelope")
					assert.NotContains(t, files, form, "the files the run left")
					assert.NotContains(t, string(rawEvents), form, "the decoded event")
				}
			}
			for _, p := range posts {
				assert.NotContains(t, p, "\ufeff", "the byte order mark never leaves the CLI")
			}
			if len(c.sent) > 0 {
				var body map[string]any
				require.NoError(t, json.Unmarshal([]byte(posts[0]), &body))
				for path, want := range c.sent {
					key, field, _ := strings.Cut(path, ".")
					config, _ := body[key].(map[string]any)
					assert.Equal(t, want, config[field], "the value of "+path+" in the request")
				}
			}
		})
	}
}

// ccAmazon is a create of amazonec2 with the secret key read from stdin.
func ccAmazon(extra ...string) []string {
	return append([]string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-"}, extra...)
}

// ccEcho is the 422 body that quotes text in its message.
func ccEcho(text string) string {
	raw, _ := json.Marshal(map[string]string{"type": "error", "code": "MissingRequired", "message": "field value " + text + " is missing"})
	return string(raw)
}

// ccFieldError is the body of a 422 Status as the apiserver reports an invalid
// string value: the message and the cause quote the value with %q.
func ccFieldError(secret string) string {
	raw, _ := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "Invalid", "code": 422,
		"message": fmt.Sprintf("Secret %q is invalid: data[secretKey]: Invalid value: %q: must be valid", "cc-abcde", secret),
		"details": map[string]any{"name": "cc-abcde", "kind": "Secret", "causes": []map[string]string{
			{"reason": "FieldValueInvalid", "message": fmt.Sprintf("Invalid value: %q: must be valid", secret), "field": "data[secretKey]"},
		}},
	})
	return string(raw)
}

// ccDriverCase builds the command line of one row of the driver table: each
// public field with --set, each secret field from its own file, and the response
// the case asks for.
func ccDriverCase(t *testing.T, driver string, public, secret []string, norman ccNorman, apply bool, errCode string) ccCase {
	t.Helper()
	c := ccCase{argv: []string{"create", "cloud-credential", driver, "row"}, norman: norman, errCode: errCode, kind: "CloudCredential", sent: map[string]string{}}
	for _, name := range public {
		c.argv = append(c.argv, "--set", name+"=fake-public-"+name)
	}
	var all []string
	for _, name := range secret {
		value := "fake-" + driver + "-" + name + "-0001"
		switch {
		case driver == "harvester" && name == "kubeconfigContent":
			value = ccKube
		case name == "authEncodedJson" || name == "kubeconfigContent" || name == "privateKeyContents":
			value = "fake-" + driver + "-" + name + "-line-one\n" + "fake-" + driver + "-" + name + "-line-two\n"
		}
		path := filepath.Join(t.TempDir(), name+".txt")
		require.NoError(t, os.WriteFile(path, []byte(value), 0o600))
		c.argv = append(c.argv, "--from-file", name+"="+path)
		cleaned := strings.TrimSpace(value)
		c.secrets = append(c.secrets, cleaned)
		c.sent[driver+"credentialConfig."+name] = cleaned
		all = append(all, cleaned)
	}
	if apply {
		c.argv = append(c.argv, "--apply")
		c.posts = 1
	} else {
		c.argv = append(c.argv, "--dry-run")
		c.kind, c.sent = "Effect", nil
	}
	if norman.status >= 400 {
		c.norman.body = ccEcho(strings.Join(all, " | "))
	}
	return c
}

// ccDriverRows lists the public and secret fields of every driver the stub
// serves, and of the drivers that have no node driver.
func ccDriverRows() map[string][2][]string {
	rows := map[string][2][]string{}
	for _, prefix := range cloudcredfields.StaticPrefixes() {
		d, _ := cloudcredfields.Static(prefix)
		rows[prefix] = [2][]string{
			d.Names(func(f cloudcredfields.Field) bool { return !f.Secret && !f.Multiline }),
			d.Names(func(f cloudcredfields.Field) bool { return f.Secret }),
		}
	}
	split := func(list string) []string {
		var out []string
		for name := range strings.SplitSeq(list, ",") {
			if name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	for _, d := range ccDrivers {
		password := split(d.password)
		var public []string
		for _, name := range split(d.public) {
			if !slices.Contains(password, name) {
				public = append(public, name)
			}
		}
		rows[d.name] = [2][]string{public, slices.Concat(split(d.private), password)}
	}
	return rows
}

// TestCloudCredentialCommandsNeverShowASecretValue asserts, through the real
// entry, that no secret value read from stdin or a file is in stdout, stderr,
// the decoded envelope, the files the run leaves or the telemetry events the
// run records, as typed, JSON-escaped or in base64, on a preview, a create and
// each refusal and response of the create, for every driver the stub serves and
// for s3 and alibaba. Each row also asserts it reached its path: the exit code,
// the error code of the envelope or its kind, the number of requests the
// Norman route saw, and the values in the request. It also asserts the error of
// the Norman client has none of the forms of a value the response echoes, as a
// command that returned that error without converting it would print it. A
// secret of fewer than 4 characters is not hidden by the filter, so every
// secret planted here has 4 or more. A user who types a secret as the path of
// --from-file is out of this table: the message quotes the path as typed, and
// the command line already has it. Not parallel: it sets the environment.
func TestCloudCredentialCommandsNeverShowASecretValue(t *testing.T) {
	restoreRawArgv(t)
	t.Run("every driver", func(t *testing.T) {
		for driver, fields := range ccDriverRows() {
			for _, step := range []struct {
				name    string
				norman  ccNorman
				apply   bool
				errCode string
			}{
				{"a preview", ccNorman{}, false, ""},
				{"a create", ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde"}`}, true, ""},
				{"a 422 that echoes every secret", ccNorman{status: 422}, true, clierr.CodeValidation},
				{"a 500 that echoes every secret", ccNorman{status: 500}, true, clierr.CodeGeneric},
			} {
				t.Run(driver+" "+step.name, func(t *testing.T) {
					ccCheck(t, ccDriverCase(t, driver, fields[0], fields[1], step.norman, step.apply, step.errCode))
				})
			}
		}
	})

	const base = "fake-cc-canary-secret-0001"
	const quoted = `fake-cc-"quoted"-0001`
	const nbsp = "fake-cc-nbsp\u00a0\asecret-0001"
	created := ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde","amazonec2credentialConfig":{"secretKey":"` + base + `"}}`}
	key := map[string]string{"amazonec2credentialConfig.secretKey": base}
	answer := func(status int, body string) ccNorman { return ccNorman{status: status, body: body} }
	rows := []struct {
		name string
		c    ccCase
	}{
		{"a preview", ccCase{argv: ccAmazon("--dry-run"), stdin: base + "\n", kind: "Effect", secrets: []string{base}}},
		{"a refused display name list", ccCase{argv: ccAmazon("--dry-run"), stdin: base + "\n", list: true, kind: "Effect", secrets: []string{base}}},
		{"a create", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: created, kind: "CloudCredential", posts: 1, sent: key, secrets: []string{base}}},
		{"a 401 that echoes the value", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(401, ccEcho(base)), errCode: clierr.CodeAuth, posts: 1, secrets: []string{base}}},
		{"a 403 that echoes the value", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(403, ccEcho(base)), errCode: clierr.CodeRBAC, posts: 1, secrets: []string{base}}},
		{"a 409 that echoes the value", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(409, ccEcho(base)), errCode: clierr.CodeConflict, posts: 1, secrets: []string{base}}},
		{"a 422 that echoes the value", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(422, ccEcho(base)), errCode: clierr.CodeValidation, posts: 1, secrets: []string{base}}},
		{"a 422 that quotes the base64 form", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(422, ccEcho(base64.StdEncoding.EncodeToString([]byte(base)))), errCode: clierr.CodeValidation, posts: 1, secrets: []string{base}}},
		{"a 422 field error that quotes the value", ccCase{argv: ccAmazon("--apply"), stdin: nbsp + "\n", norman: answer(422, ccFieldError(nbsp)), errCode: clierr.CodeValidation, posts: 1, secrets: []string{nbsp}}},
		{"a 422 that echoes the value JSON-escaped", ccCase{argv: ccAmazon("--apply"), stdin: quoted + "\n",
			norman: answer(422, `{"type":"error","message":"bad value fake-cc-\\"quoted\\"-0001 here"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{quoted}}},
		{"a 500", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(500, ccEcho(base)), errCode: clierr.CodeGeneric, posts: 1, secrets: []string{base}}},
		{"a 502", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(502, ccEcho(base)), errCode: clierr.CodeGeneric, posts: 1, secrets: []string{base}}},
		{"a 504", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: answer(504, ccEcho(base)), errCode: clierr.CodeGeneric, posts: 1, secrets: []string{base}}},
		{"a lost response", ccCase{argv: ccAmazon("--apply"), stdin: base + "\n", norman: ccNorman{drop: true}, errCode: clierr.CodeGeneric, posts: 1, secrets: []string{base}}},
		{"a value with one byte order mark", ccCase{argv: ccAmazon("--apply"), stdin: "\xef\xbb\xbf" + base + "\n", norman: answer(422, ccEcho(base)), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}}},
		{"a value with two byte order marks and CRLF", ccCase{argv: ccAmazon("--apply"), stdin: "\xef\xbb\xbf\xef\xbb\xbf" + base + "\r\n", norman: answer(422, ccEcho(base)), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}}},
		{"a CRLF value echoed JSON-escaped", ccCase{argv: ccAmazon("--apply"), stdin: base + "\r\n", norman: answer(422, `{"type":"error","message":"bad value `+base+`\\r\\n here"}`), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}}},
		{"a multi-line value with CRLF", ccCase{argv: []string{"create", "cloud-credential", "google", "g", "--from-file", "authEncodedJson=-", "--apply"}, stdin: ccLineOne + "\r\n" + ccLineTwo + "\r\n",
			norman: answer(422, `{"type":"error","message":"bad `+ccLineTwo+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccLineOne + "\r\n" + ccLineTwo}}},
		{"a multi-line value echoed JSON-escaped", ccCase{argv: []string{"create", "cloud-credential", "google", "g", "--from-file", "authEncodedJson=-", "--apply"}, stdin: ccLineOne + "\r\n" + ccLineTwo + "\r\n",
			norman: answer(422, `{"type":"error","message":"bad `+ccLineOne+`\\r\\n`+ccLineTwo+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccLineOne + "\r\n" + ccLineTwo}}},
		{"a harvester parser error that quotes the kubeconfig", ccCase{argv: []string{"create", "cloud-credential", "harvester", "h", "--from-file", "kubeconfigContent=-", "--apply"}, stdin: ccKube,
			norman: answer(500, `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: failed to get tokens from kubeconfig: yaml: `+ccCanary+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccCanary, strings.TrimSpace(ccKube)}}},
		{"a harvester token message", ccCase{argv: []string{"create", "cloud-credential", "harvester", "h", "--from-file", "kubeconfigContent=-", "--apply"}, stdin: ccKube,
			norman: answer(500, `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: token kubeconfig-user-x is expired at the line     token: kubeconfig-user-x:`+ccCanary+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{strings.TrimSpace(ccKube)}}},
		{"an extra argument after the command line", ccCase{argv: ccAmazon("--dry-run", base), stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a --set argument with no equals sign", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", base, "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a --from-file argument with no equals sign", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", base, "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a pasted secret that ends in equals signs as a --set argument", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", base + "==", "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a pasted secret that ends in equals signs as a --from-file argument", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", base + "==", "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a pasted secret that ends in equals signs given twice", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", base + "==", "--set", base + "==", "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a secret given with --set", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "secretKey=" + base, "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a password field given with --set", ccCase{argv: []string{"create", "cloud-credential", "pnap", "p", "--set", "clientSecret=" + base, "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"an unknown driver", ccCase{argv: []string{"create", "cloud-credential", "nope", "n", "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"an inactive driver", ccCase{argv: []string{"create", "cloud-credential", "dormant", "d", "--from-file", "token=-", "--dry-run"}, stdin: base + "\n", opts: ccOptions{dormant: true}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"alibaba while its operator is inactive", ccCase{argv: []string{"create", "cloud-credential", "alibaba", "a", "--from-file", "accessKeySecret=-", "--dry-run"}, stdin: base + "\n", opts: ccOptions{alibabaOff: true}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a missing required field", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a line break in a single-line value", ccCase{argv: ccAmazon("--dry-run"), stdin: ccLineOne + "\n" + ccLineTwo + "\n", errCode: clierr.CodeValidation, secrets: []string{ccLineOne, ccLineTwo}}},
		{"a line break and a tab in a single-line value", ccCase{argv: ccAmazon("--dry-run"), stdin: ccLineOne + "\n" + ccTabLine + "\n", errCode: clierr.CodeValidation, secrets: []string{ccLineOne, ccTabLine}}},
		{"a secret as the field of a --set argument", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", base + "=x", "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a secret given with --set in another case", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "SECRETKEY=" + base, "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a public field given with --from-file", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", "accessKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a missing required field with stdin given", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "defaultRegion=us-east-1", "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"an empty value", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=", "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, message: "the 1st --set has an empty value", secrets: []string{base}}},
		{"an access review that denies", ccCase{argv: ccAmazon("--dry-run"), stdin: base + "\n", opts: ccOptions{reviewDenied: true}, errCode: clierr.CodeRBAC, secrets: []string{base}}},
		{"white space at the end of a value", ccCase{argv: ccAmazon("--dry-run"), stdin: base + " \n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a UTF-16 value", ccCase{argv: ccAmazon("--dry-run"), stdin: "\xff\xfe" + "f\x00a\x00k\x00e\x00", errCode: clierr.CodeValidation}},
		{"a harvester file that is not a kubeconfig", ccCase{argv: []string{"create", "cloud-credential", "harvester", "h", "--from-file", "kubeconfigContent=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"stdin given twice", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", "secretKey=-", "--from-file", "accessKey=-", "--dry-run"}, stdin: base + "\n", errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a file that cannot be read", ccCase{argv: []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=missing.txt", "--dry-run"}, errCode: clierr.CodeValidation, secrets: []string{base}}},
		{"a display name in use", ccCase{argv: ccAmazon("--dry-run"), stdin: base + "\n", opts: ccOptions{existing: true}, errCode: clierr.CodeValidation, secrets: []string{base}}},
	}
	stored := ccOptions{stored: true}
	patch := func(extra ...string) []string {
		return append([]string{"patch", "cloud-credential", "cc-abcde", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-"}, extra...)
	}
	applied := func(extra ...string) []string { return patch(append([]string{"--apply", "--yes"}, extra...)...) }
	both := []string{base, ccAnnotation}
	changedRow := func(name string, c ccCase) {
		rows = append(rows, struct {
			name string
			c    ccCase
		}{name, c})
	}
	changedRow("a patch preview", ccCase{argv: patch("--dry-run"), stdin: base + "\n", opts: stored, kind: "Effect", secrets: both})
	changedRow("a patch preview with a refused display name list", ccCase{argv: patch("--dry-run", "--display-name", "renamed"), stdin: base + "\n", opts: stored, list: true, kind: "Effect", secrets: both})
	changedRow("a patch", ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: stored, norman: ccNorman{status: 200, body: `{"id":"cattle-global-data:cc-abcde"}`}, kind: "CloudCredential", posts: 1, sent: key, secrets: both})
	for _, status := range []struct {
		code int
		want string
	}{{401, clierr.CodeAuth}, {403, clierr.CodeRBAC}, {404, clierr.CodeNotFound}, {409, clierr.CodeConflict}, {422, clierr.CodeValidation}} {
		changedRow("a patch responded with "+strconv.Itoa(status.code)+" that echoes the value and the annotation", ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: stored,
			norman: answer(status.code, ccEcho(base+" "+ccAnnotation)), errCode: status.want, posts: 1, secrets: both})
	}
	for _, status := range []int{500, 502, 504} {
		changedRow("a patch responded with "+strconv.Itoa(status), ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: stored,
			norman: answer(status, ccEcho(base+" "+ccAnnotation)), errCode: clierr.CodeGeneric, posts: 1, secrets: both})
	}
	changedRow("a patch responded with 422 with the base64 form", ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: stored,
		norman: answer(422, ccEcho(base64.StdEncoding.EncodeToString([]byte(base)))), errCode: clierr.CodeValidation, posts: 1, secrets: both})
	changedRow("a patch responded with 422 with the value JSON-escaped", ccCase{argv: applied(), stdin: quoted + "\n", opts: stored,
		norman: answer(422, `{"type":"error","message":"bad value fake-cc-\\"quoted\\"-0001 here"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{quoted}})
	changedRow("a patch with a lost response", ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: stored, norman: ccNorman{drop: true}, errCode: clierr.CodeGeneric, posts: 1, secrets: both})
	changedRow("a patch with one byte order mark", ccCase{argv: applied(), stdin: "\xef\xbb\xbf" + base + "\n", opts: stored, norman: answer(422, ccEcho(base)), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}})
	changedRow("a patch with two byte order marks and CRLF", ccCase{argv: applied(), stdin: "\xef\xbb\xbf\xef\xbb\xbf" + base + "\r\n", opts: stored, norman: answer(422, ccEcho(base)), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}})
	changedRow("a patch with a CRLF value echoed JSON-escaped", ccCase{argv: applied(), stdin: base + "\r\n", opts: stored,
		norman: answer(422, `{"type":"error","message":"bad value `+base+`\\r\\n here"}`), errCode: clierr.CodeValidation, posts: 1, sent: key, secrets: []string{base}})
	changedRow("a patch of a multi-line value with CRLF", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--from-file", "authEncodedJson=-", "--apply", "--yes"}, stdin: ccLineOne + "\r\n" + ccLineTwo + "\r\n",
		opts: ccOptions{stored: true, storedDriver: "google"}, norman: answer(422, `{"type":"error","message":"bad `+ccLineTwo+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccLineOne + "\r\n" + ccLineTwo}})
	changedRow("a patch of a harvester kubeconfig whose parser error quotes it", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--from-file", "kubeconfigContent=-", "--apply", "--yes"}, stdin: ccKube, opts: ccOptions{stored: true, storedDriver: "harvester"},
		norman: answer(500, `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: failed to get tokens from kubeconfig: yaml: `+ccCanary+`"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccCanary, strings.TrimSpace(ccKube)}})
	changedRow("a patch of a harvester kubeconfig whose token is in use", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--from-file", "kubeconfigContent=-", "--apply", "--yes"}, stdin: ccKube, opts: ccOptions{stored: true, storedDriver: "harvester"},
		norman: answer(500, `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: token kubeconfig-user-x is already in use by secret"}`), errCode: clierr.CodeValidation, posts: 1, secrets: []string{ccCanary, strings.TrimSpace(ccKube)}})
	changedRow("a patch after a change found by the read before the request", ccCase{argv: applied("--description", "new"), stdin: base + "\n", opts: ccOptions{stored: true, changed: true}, errCode: clierr.CodeConflict, secrets: both})
	changedRow("a patch with an extra argument", ccCase{argv: patch("--dry-run", base), stdin: base + "\n", opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a --set argument with no equals sign", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", base, "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a --from-file argument with no equals sign", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--from-file", base, "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a secret given with --set", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", "secretKey=" + base, "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with no field flag", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--dry-run"}, opts: stored, errCode: clierr.CodeValidation})
	changedRow("a patch with a secret that needs --yes", ccCase{argv: patch("--apply"), stdin: base + "\n", opts: stored, errCode: clierr.CodeConfirmationRequired, secrets: []string{base}})
	changedRow("a patch with an access review that denies", ccCase{argv: patch("--dry-run"), stdin: base + "\n", opts: ccOptions{stored: true, reviewDenied: true}, errCode: clierr.CodeRBAC, secrets: []string{base}})
	changedRow("a patch of a credential that is not there", ccCase{argv: []string{"patch", "cloud-credential", "nope", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--dry-run"}, stdin: base + "\n", opts: stored, errCode: clierr.CodeNotFound, secrets: []string{base}})
	changedRow("a patch with a line break in a single-line value", ccCase{argv: patch("--dry-run"), stdin: ccLineOne + "\n" + ccLineTwo + "\n", opts: stored, errCode: clierr.CodeValidation, secrets: []string{ccLineOne, ccLineTwo}})
	changedRow("a patch with a line break and a tab in a single-line value", ccCase{argv: patch("--dry-run"), stdin: ccLineOne + "\n" + ccTabLine + "\n", opts: stored, errCode: clierr.CodeValidation, secrets: []string{ccLineOne, ccTabLine}})
	changedRow("a patch responded with 422 with a field error that quotes the value", ccCase{argv: applied(), stdin: nbsp + "\n", opts: stored, norman: answer(422, ccFieldError(nbsp)), errCode: clierr.CodeValidation, posts: 1, secrets: []string{nbsp}})
	changedRow("a patch with a secret as the field of a --set argument", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", base + "=x", "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a secret given with --set in another case", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", "SECRETKEY=" + base, "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a pasted secret that ends in equals signs as a --set argument", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", base + "==", "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a pasted secret that ends in equals signs as a --from-file argument", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--from-file", base + "==", "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	changedRow("a patch with a pasted secret that ends in equals signs given twice", ccCase{argv: []string{"patch", "cloud-credential", "cc-abcde", "--set", base + "==", "--set", base + "==", "--dry-run"}, opts: stored, errCode: clierr.CodeValidation, secrets: []string{base}})
	storedValues := []string{ccAnnotation, ccStoredData}
	remove := func(extra ...string) []string {
		return append([]string{"delete", "cloud-credential", "cc-abcde"}, extra...)
	}
	removed := ccNorman{status: 204}
	changedRow("a delete preview", ccCase{argv: remove("--dry-run"), opts: stored, kind: "Effect", secrets: storedValues})
	changedRow("a delete preview with the lists of clusters refused", ccCase{argv: remove("--dry-run"), opts: ccOptions{stored: true, clustersRefused: true}, kind: "Effect", secrets: storedValues})
	changedRow("a delete", ccCase{argv: remove("--apply", "--yes"), opts: stored, norman: removed, kind: "CloudCredential", posts: 1, secrets: storedValues})
	changedRow("a delete that does not wait", ccCase{argv: remove("--apply", "--yes", "--wait=false"), opts: stored, norman: removed, kind: "CloudCredential", posts: 1, secrets: storedValues})
	changedRow("a delete with the lists of clusters refused", ccCase{argv: remove("--apply", "--yes"), opts: ccOptions{stored: true, clustersRefused: true}, norman: removed, kind: "CloudCredential", posts: 1, secrets: storedValues})
	for _, status := range []struct {
		code int
		want string
	}{{401, clierr.CodeAuth}, {403, clierr.CodeRBAC}, {404, clierr.CodeNotFound}, {409, clierr.CodeConflict}, {422, clierr.CodeValidation}} {
		changedRow("a delete responded with "+strconv.Itoa(status.code), ccCase{argv: remove("--apply", "--yes"), opts: stored,
			norman: answer(status.code, ccEcho("cc-abcde")), errCode: status.want, posts: 1, secrets: storedValues})
	}
	for _, status := range []int{500, 502, 504} {
		changedRow("a delete responded with "+strconv.Itoa(status), ccCase{argv: remove("--apply", "--yes"), opts: stored,
			norman: answer(status, ccEcho("cc-abcde")), errCode: clierr.CodeGeneric, posts: 1, secrets: storedValues})
	}
	changedRow("a delete with a lost response", ccCase{argv: remove("--apply", "--yes"), opts: stored, norman: ccNorman{drop: true}, errCode: clierr.CodeGeneric, posts: 1, secrets: storedValues})
	changedRow("a delete after a change found by the read before the request", ccCase{argv: remove("--apply", "--yes"), opts: ccOptions{stored: true, changed: true}, errCode: clierr.CodeConflict, secrets: storedValues})
	changedRow("a delete with no --yes", ccCase{argv: remove("--apply"), opts: stored, errCode: clierr.CodeConfirmationRequired, secrets: storedValues})
	changedRow("a delete of a credential that is not there", ccCase{argv: []string{"delete", "cloud-credential", "nope", "--dry-run"}, opts: stored, errCode: clierr.CodeNotFound, secrets: storedValues})
	for _, shaped := range []string{"hunter2Secret", "q3XrT9vLm2Pz8KdWb1YcNa"} {
		word := shaped + "=="
		secrets := []string{shaped, word}
		amazon := []string{"create", "cloud-credential", "amazonec2", "prod"}
		rows = append(rows, []struct {
			name string
			c    ccCase
		}{
			{"a pasted secret shaped like a field name as a --set argument: " + shaped, ccCase{argv: append(slices.Clone(amazon), "--set", word, "--dry-run"), errCode: clierr.CodeValidation, secrets: secrets}},
			{"a pasted secret shaped like a field name as a --from-file argument: " + shaped, ccCase{argv: append(slices.Clone(amazon), "--from-file", word, "--dry-run"), errCode: clierr.CodeValidation, secrets: secrets}},
			{"a pasted secret shaped like a field name given twice: " + shaped, ccCase{argv: append(slices.Clone(amazon), "--set", word, "--set", word, "--dry-run"), errCode: clierr.CodeValidation, secrets: secrets}},
		}...)
	}
	for _, shaped := range []string{"hunter2Secret", "q3XrT9vLm2Pz8KdWb1YcNa"} {
		word := shaped + "=="
		secrets := []string{shaped, word}
		patchArgv := []string{"patch", "cloud-credential", "cc-abcde"}
		rows = append(rows, []struct {
			name string
			c    ccCase
		}{
			{"a patch with a pasted secret shaped like a field name as a --set argument: " + shaped, ccCase{argv: append(slices.Clone(patchArgv), "--set", word, "--dry-run"), opts: ccOptions{stored: true}, errCode: clierr.CodeValidation, secrets: secrets}},
			{"a patch with a pasted secret shaped like a field name as a --from-file argument: " + shaped, ccCase{argv: append(slices.Clone(patchArgv), "--from-file", word, "--dry-run"), opts: ccOptions{stored: true}, errCode: clierr.CodeValidation, secrets: secrets}},
			{"a patch with a pasted secret shaped like a field name given twice: " + shaped, ccCase{argv: append(slices.Clone(patchArgv), "--set", word, "--set", word, "--dry-run"), opts: ccOptions{stored: true}, errCode: clierr.CodeValidation, secrets: secrets}},
		}...)
	}
	for _, pasted := range []string{"hunter2Secret", "q3XrT9vLm2Pz8KdWb1YcNa0"} {
		word := pasted + "="
		secrets := []string{pasted, word}
		patchArgv := []string{"patch", "cloud-credential", "cc-abcde"}
		rows = append(rows, []struct {
			name string
			c    ccCase
		}{
			{"a patch with a pasted secret ending in one equals sign as a --set argument: " + pasted, ccCase{argv: append(slices.Clone(patchArgv), "--set", word, "--dry-run"), opts: ccOptions{stored: true}, errCode: clierr.CodeValidation, message: "the 1st --set has an empty value", secrets: secrets}},
			{"a patch with a pasted secret ending in one equals sign as a --from-file argument: " + pasted, ccCase{argv: append(slices.Clone(patchArgv), "--from-file", word, "--dry-run"), opts: ccOptions{stored: true}, errCode: clierr.CodeValidation, message: "the 1st --from-file has an empty path", secrets: secrets}},
		}...)
	}
	for _, pasted := range []string{"hunter2Secret", "q3XrT9vLm2Pz8KdWb1YcNa0"} {
		word := pasted + "="
		secrets := []string{pasted, word}
		amazon := []string{"create", "cloud-credential", "amazonec2", "prod"}
		rows = append(rows, []struct {
			name string
			c    ccCase
		}{
			{"a pasted secret ending in one equals sign as a --set argument: " + pasted, ccCase{argv: append(slices.Clone(amazon), "--set", word, "--dry-run"), errCode: clierr.CodeValidation, message: "the 1st --set has an empty value", secrets: secrets}},
			{"a pasted secret ending in one equals sign as a --from-file argument: " + pasted, ccCase{argv: append(slices.Clone(amazon), "--from-file", word, "--dry-run"), errCode: clierr.CodeValidation, message: "the 1st --from-file has an empty path", secrets: secrets}},
		}...)
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { ccCheck(t, row.c) })
	}

	t.Run("the error of the client returned unconverted by an update", func(t *testing.T) {
		echo := ccEcho(ccCanary)
		for _, status := range []int{http.StatusUnprocessableEntity, http.StatusInternalServerError} {
			n := newNorman(t)
			n.status = status
			n.answer = echo
			err := credentialClientFor(t, n).Update(t.Context(), "cattle-global-data:cc-abcde", clientBody())
			require.Error(t, err)
			require.Equal(t, 1, len(n.seen()), "the client sent the request once")
			for _, form := range ccForms(ccCanary) {
				assert.NotContains(t, err.Error(), form, "the error text")
				assert.NotContains(t, clierr.From(err).Message, form, "the rendered message")
			}
		}
	})

	t.Run("the error of the client returned unconverted", func(t *testing.T) {
		echo := ccEcho(ccCanary)
		for _, status := range []int{http.StatusUnprocessableEntity, http.StatusInternalServerError} {
			n := newNorman(t)
			n.status = status
			n.answer = echo
			_, err := credentialClientFor(t, n).Create(t.Context(), clientBody())
			require.Error(t, err)

			require.Equal(t, 1, len(n.seen()), "the client sent the request once")
			for _, form := range ccForms(ccCanary) {
				assert.NotContains(t, err.Error(), form, "the error text")
				assert.NotContains(t, clierr.From(err).Message, form, "the rendered message")
			}
		}
	})
}

// TestCloudCredentialPatchYAMLDiffLeavesOutFromOfUnreadValues asserts the -o yaml
// preview of a patch has no `from` on the entries of the config fields, whose
// stored values the CLI does not read, and keeps it on the name and the
// description.
func TestCloudCredentialPatchYAMLDiffLeavesOutFromOfUnreadValues(t *testing.T) {
	argv := []string{"patch", "cloud-credential", "cc-abcde", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-",
		"--display-name", "renamed", "--description", "new", "--dry-run", "-o", "yaml"}
	var posts []string
	stdout, stderr, code, _ := ccRun(t, ccStubWith(ccOptions{stored: true}, ccNorman{}, &posts, false), argv, ccCanary+"\n")
	require.Zero(t, code, stderr)
	var envelope struct {
		Data struct {
			Diff []map[string]any `json:"diff"`
		} `json:"data"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(stdout), &envelope), stdout)
	byPath := map[string]map[string]any{}
	for _, e := range envelope.Data.Diff {
		byPath[e["path"].(string)] = e
	}
	require.Len(t, byPath, 4)
	assert.Equal(t, "prod", byPath["/name"]["from"])
	assert.Equal(t, "old", byPath["/description"]["from"])
	for _, path := range []string{"/amazonec2credentialConfig/accessKey", "/amazonec2credentialConfig/secretKey"} {
		require.Contains(t, byPath, path)
		assert.NotContains(t, byPath[path], "from", path)
		assert.Contains(t, byPath[path], "to", path)
	}
	assert.NotContains(t, stdout, ccCanary)
}
