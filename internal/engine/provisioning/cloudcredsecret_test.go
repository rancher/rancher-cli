package provisioning

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/provisioning/cloudcredfields"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ccsDrivers is one row per built-in node driver that takes a cloud
// credential, plus a driver with two secret fields and one whose passwordFields
// names a public field.
var ccsDrivers = []struct{ name, public, private, password, optional, defaults string }{
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

// ccsKubeconfig is a kubeconfig whose token line holds the secret.
func ccsKubeconfig(secret string) string {
	return "apiVersion: v1\nclusters:\n- name: c\nusers:\n- name: u\n  user:\n    token: kubeconfig-user-x:" + secret + "\n"
}

// ccsRun is one command line over a driver of the table: the input, the files
// it reads, and every secret value and line of a value the run holds.
type ccsRun struct {
	in      ccwInput
	files   map[string]string
	needles []string
	fields  map[string]string
}

// ccsFields splits a comma-separated annotation into its names.
func ccsFields(list string) []string {
	var out []string
	for name := range strings.SplitSeq(list, ",") {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ccsInputFor builds the command line of a driver from the names of its public
// and secret fields: each public field with --set, each secret field read from
// its own file. A multi-line field holds two lines.
func ccsInputFor(driver string, public, secret []string) ccsRun {
	run := ccsRun{in: ccwInput{args: []string{driver, "row"}}, files: map[string]string{}, fields: map[string]string{}}
	for _, name := range public {
		run.in.set = append(run.in.set, name+"=fake-public-"+name)
	}
	for _, name := range secret {
		value := "fake-" + driver + "-" + name + "-0001"
		switch {
		case driver == "harvester" && name == "kubeconfigContent":
			value = ccsKubeconfig(value)
		case name == "authEncodedJson" || name == "kubeconfigContent" || name == "privateKeyContents":
			value = "fake-" + driver + "-" + name + "-line-one\n" + "fake-" + driver + "-" + name + "-line-two\n"
		}
		run.files[name+".txt"] = value
		run.in.fromFile = append(run.in.fromFile, name+"="+name+".txt")
		cleaned := strings.TrimSpace(value)
		run.fields[name] = cleaned
		run.needles = append(run.needles, cleaned)
		for line := range strings.SplitSeq(cleaned, "\n") {
			if strings.Contains(line, "fake") {
				run.needles = append(run.needles, line)
			}
		}
	}
	return run
}

// ccsRuns lists the command line of every driver the table and the static
// rows hold.
func ccsRuns() map[string]ccsRun {
	out := map[string]ccsRun{}
	for _, d := range ccsDrivers {
		password := ccsFields(d.password)
		var public []string
		for _, name := range ccsFields(d.public) {
			if !slices.Contains(password, name) {
				public = append(public, name)
			}
		}
		secret := slices.Concat(ccsFields(d.private), password)
		out[d.name] = ccsInputFor(d.name, public, secret)
	}
	for _, prefix := range cloudcredfields.StaticPrefixes() {
		d, _ := cloudcredfields.Static(prefix)
		public := d.Names(func(f cloudcredfields.Field) bool { return !f.Secret && !f.Multiline })
		secret := d.Names(func(f cloudcredfields.Field) bool { return f.Secret })
		out[prefix] = ccsInputFor(prefix, public, secret)
	}
	return out
}

// ccsForms are the forms of a secret no output may hold: as typed, JSON-escaped
// and standard base64.
func ccsForms(secret string) []string {
	escaped, _ := json.Marshal(secret)
	return []string{secret, strings.Trim(string(escaped), `"`), base64.StdEncoding.EncodeToString([]byte(secret))}
}

func ccsRig(files map[string]string) *ccwRig {
	r := newCCWRig(files)
	r.client.drivers = nil
	for _, d := range ccsDrivers {
		r.client.drivers = append(r.client.drivers, nodeDriver(d.name, d.name, true, false, d.public, d.private, d.password, d.optional, d.defaults))
	}
	r.client.setting = `[{"name":"alibaba","active":true}]`
	return r
}

// ccsExecute runs the command through the pipeline and returns what it wrote
// and the error it ended with, rendered in every form a caller can read, and
// what it wrote to the output alone.
func ccsExecute(t *testing.T, r *ccwRig, in ccwInput, format string, dryRun bool) (written, stdout string, ce *clierr.Error) {
	t.Helper()
	var out, errOut bytes.Buffer
	_, ce = runtime.Run(t.Context(), r.cmd, in, runtime.IO{Out: &out, Err: &errOut, Format: format, DryRun: dryRun, Apply: !dryRun}, safety.Binding{})
	written = out.String() + errOut.String()
	if ce != nil {
		raw, _ := json.Marshal(ce)
		written += ce.Error() + ce.Message + ce.Hint + string(raw)
	}
	return written, out.String(), ce
}

// ccsAssertKind asserts what a successful run wrote: in JSON, an envelope of
// this kind whose data holds a string equal to want; in text, the text want.
func ccsAssertKind(t *testing.T, format, stdout, kind, want string) {
	t.Helper()
	if format != "json" {
		assert.Contains(t, stdout, want)
		return
	}
	var envelope struct {
		Kind string `json:"kind"`
		Data any    `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	assert.Equal(t, kind, envelope.Kind)
	assert.True(t, ccsHasString(envelope.Data, want), "the data of the envelope holds %q: %s", want, stdout)
}

// ccsHasString reports whether a string leaf of the decoded JSON value
// contains want.
func ccsHasString(v any, want string) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, want)
	case []any:
		return slices.ContainsFunc(v, func(e any) bool { return ccsHasString(e, want) })
	case map[string]any:
		return slices.ContainsFunc(slices.Collect(maps.Values(v)), func(e any) bool { return ccsHasString(e, want) })
	}
	return false
}

func ccsRefused(status int, body string) error {
	request := &clierr.Request{Server: "https://rancher.test"}
	return &engine.ActionRefused{Err: clierr.FromResponse("POST x on the Rancher server", http.MethodPost, status, nil, request), Body: []byte(body), Request: request}
}

func ccsUnknown(status int, body string) error {
	return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, status, nil), Body: []byte(body)}
}

// assertNoForms asserts none of the forms of any needle is in text.
func assertNoForms(t *testing.T, needles []string, text, where string) {
	t.Helper()
	for _, needle := range needles {
		for _, form := range ccsForms(needle) {
			assert.NotContains(t, text, form, where)
		}
	}
}

// TestCloudCredentialCommandsNeverShowASecretValue asserts that no secret value
// of any driver, as typed, JSON-escaped or in base64, and no line of a
// multi-line value, is in anything the command writes or returns: on a
// preview, a create, and each answer a create can get, in JSON and in text.
// Each row also asserts it reached its path: the exit code, the error code or
// the result, the number of requests sent, and the values in the request. A
// secret of fewer than 4 characters is not hidden by the filter, so every
// secret planted here has 4 or more. The same rows run through the entry point
// in internal/cli, which also asserts the decoded envelope and the telemetry.
func TestCloudCredentialCommandsNeverShowASecretValue(t *testing.T) {
	t.Parallel()
	answers := func(driver string, needles []string) []struct {
		name    string
		err     error
		code    string
		creates int
	} {
		answer := func(code, message string) string {
			raw, _ := json.Marshal(map[string]string{"type": "error", "code": code, "message": message})
			return string(raw)
		}
		echo := answer("X", "bad value "+strings.Join(needles, " | ")+" here")
		harvester := func(tail string) string {
			return answer("ServerError", "failed to process harvester cloud credential: "+tail)
		}
		rows := []struct {
			name    string
			err     error
			code    string
			creates int
		}{
			{"a create", nil, "", 1},
			{"a 401", ccsRefused(http.StatusUnauthorized, echo), clierr.CodeAuth, 1},
			{"a 403", ccsRefused(http.StatusForbidden, echo), clierr.CodeRBAC, 1},
			{"a 409", ccsRefused(http.StatusConflict, echo), clierr.CodeConflict, 1},
			{"a 422", ccsRefused(http.StatusUnprocessableEntity, echo), clierr.CodeValidation, 1},
			{"a 500", ccsUnknown(http.StatusInternalServerError, echo), clierr.CodeGeneric, 1},
			{"a 502", ccsUnknown(http.StatusBadGateway, echo), clierr.CodeGeneric, 1},
			{"a 504", ccsUnknown(http.StatusGatewayTimeout, echo), clierr.CodeGeneric, 1},
			{"a lost answer", &engine.ActionOutcomeUnknown{Err: clierr.New(clierr.CodeGeneric, "the connection failed")}, clierr.CodeGeneric, 1},
		}
		if driver == "harvester" {
			rows = append(rows,
				struct {
					name    string
					err     error
					code    string
					creates int
				}{"a harvester parser error", ccsUnknown(http.StatusInternalServerError, harvester("failed to get tokens from kubeconfig: yaml: "+strings.Join(needles, " | "))), clierr.CodeValidation, 1},
				struct {
					name    string
					err     error
					code    string
					creates int
				}{"a harvester token message", ccsUnknown(http.StatusInternalServerError, harvester("token kubeconfig-user-x is expired: "+strings.Join(needles, " | "))), clierr.CodeValidation, 1})
		}
		return rows
	}
	for name, run := range ccsRuns() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, format := range []string{"json", ""} {
				t.Run("a preview "+format, func(t *testing.T) {
					t.Parallel()
					r := ccsRig(run.files)
					written, stdout, ce := ccsExecute(t, r, run.in, format, true)
					require.Nil(t, ce)
					ccsAssertKind(t, format, stdout, "Effect", "cloud-credential")
					assert.Zero(t, r.norman.creates)
					assertNoForms(t, run.needles, written, "the preview")
				})
				for _, tc := range answers(name, run.needles) {
					t.Run(tc.name+" "+format, func(t *testing.T) {
						t.Parallel()
						r := ccsRig(run.files)
						r.norman.err = tc.err
						written, stdout, ce := ccsExecute(t, r, run.in, format, false)
						if tc.code == "" {
							require.Nil(t, ce)
							ccsAssertKind(t, format, stdout, "CloudCredential", "cc-abcde")
						} else {
							require.NotNil(t, ce)
							assert.Equal(t, tc.code, ce.Code)
						}
						require.Equal(t, tc.creates, r.norman.creates)
						sent := r.norman.body(t)[name+"credentialConfig"].(map[string]any)
						for field, want := range run.fields {
							assert.Equal(t, want, sent[field], "the value of "+field+" in the request")
						}
						assertNoForms(t, run.needles, written, "what the run wrote")
					})
				}
			}
		})
	}
}

// TestCloudCredentialRefusalsNeverShowASecretValue asserts a refusal of the
// command line or of a value, with a secret in stdin, a file or an argument,
// ends with the code of its kind, sends no request, and shows none of the
// secrets, in JSON and in text.
func TestCloudCredentialRefusalsNeverShowASecretValue(t *testing.T) {
	t.Parallel()
	const secret = "fake-refusal-secret-0001"
	lines := "fake-refusal-line-one-0001\nfake-refusal-line-two-0002\n"
	tabLines := "fake-refusal-line-one-0001\nfake-refusal-line-two\t0002\n"
	type row struct {
		name    string
		in      ccwInput
		files   map[string]string
		prep    func(*ccwRig)
		code    string
		needles []string
	}
	amazon := func(set []string, fromFile ...string) ccwInput {
		return ccwInput{args: []string{"amazonec2", "prod"}, set: set, fromFile: fromFile}
	}
	rows := []row{
		{name: "an unknown driver", in: ccwInput{args: []string{"nope", "n"}, fromFile: []string{"secretKey=secret.txt"}}, code: clierr.CodeValidation},
		{name: "an inactive driver", in: ccwInput{args: []string{"linode", "n"}, fromFile: []string{"token=secret.txt"}}, code: clierr.CodeValidation,
			prep: func(r *ccwRig) { r.client.drivers = defaultDrivers() }},
		{name: "alibaba while its operator is inactive", in: ccwInput{args: []string{"alibaba", "n"}, fromFile: []string{"accessKeySecret=secret.txt"}}, code: clierr.CodeValidation,
			prep: func(r *ccwRig) { r.client.setting = `[{"name":"alibaba","active":false}]` }},
		{name: "a secret as the field of a --set word", in: amazon([]string{secret + "=x"}), code: clierr.CodeValidation},
		{name: "a secret ending in equals signs as a --set word", in: amazon([]string{secret + "=="}), code: clierr.CodeValidation},
		{name: "a secret ending in equals signs as a --from-file word", in: amazon(nil, secret+"=="), code: clierr.CodeValidation},
		{name: "a secret ending in equals signs given twice", in: amazon([]string{secret + "==", secret + "=="}), code: clierr.CodeValidation},
		{name: "a --set word with no equals sign", in: amazon([]string{secret}, "secretKey=secret.txt"), code: clierr.CodeValidation},
		{name: "a --from-file word with no equals sign", in: amazon(nil, secret), code: clierr.CodeValidation},
		{name: "a secret given with --set", in: amazon([]string{"secretKey=" + secret}), code: clierr.CodeValidation},
		{name: "a secret given with --set in another case", in: amazon([]string{"SECRETKEY=" + secret}), code: clierr.CodeValidation},
		{name: "a password field given with --set", in: ccwInput{args: []string{"pnap", "n"}, set: []string{"clientSecret=" + secret}}, code: clierr.CodeValidation},
		{name: "a public field given with --from-file", in: amazon(nil, "accessKey=secret.txt"), code: clierr.CodeValidation},
		{name: "a missing required field", in: amazon([]string{"accessKey=AKIAFAKE"}), code: clierr.CodeValidation},
		{name: "a missing required field with stdin that fails", in: amazon([]string{"defaultRegion=us-east-1"}, "secretKey=-"), code: clierr.CodeValidation},
		{name: "an empty value", in: amazon([]string{"accessKey="}, "secretKey=secret.txt"), code: clierr.CodeValidation},
		{name: "a line break in a single-line value", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=lines.txt"), code: clierr.CodeValidation,
			files: map[string]string{"lines.txt": lines}, needles: []string{"fake-refusal-line-one-0001", "fake-refusal-line-two-0002"}},
		{name: "a line break and a tab in a single-line value", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=lines.txt"), code: clierr.CodeValidation,
			files: map[string]string{"lines.txt": tabLines}, needles: []string{"fake-refusal-line-one-0001", "fake-refusal-line-two\t0002", tabLines}},
		{name: "white space at the end of a value", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=padded.txt"), code: clierr.CodeValidation,
			files: map[string]string{"padded.txt": secret + " \n"}},
		{name: "a UTF-16 value", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=utf16.txt"), code: clierr.CodeValidation,
			files: map[string]string{"utf16.txt": "\xff\xfe" + "f\x00a\x00k\x00e\x00"}},
		{name: "a harvester file that is not a kubeconfig", in: ccwInput{args: []string{"harvester", "h"}, fromFile: []string{"kubeconfigContent=secret.txt"}}, code: clierr.CodeValidation},
		{name: "stdin given twice", in: amazon(nil, "secretKey=-", "accessKey=-"), code: clierr.CodeValidation},
		{name: "an unreadable file", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=missing.txt"), code: clierr.CodeValidation},
		{name: "a display name in use", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=secret.txt"), code: clierr.CodeValidation,
			prep: func(r *ccwRig) {
				r.client.secrets = listOf(secretObj("cc-other", map[string]string{"field.cattle.io/name": "prod"}, map[string]any{"amazonec2credentialConfig-accessKey": "x"}))
			}},
		{name: "an access review that denies", in: amazon([]string{"accessKey=AKIAFAKE"}, "secretKey=secret.txt"), code: clierr.CodeRBAC,
			prep: func(r *ccwRig) {
				r.client.review = func() (engine.ReviewResponse, error) { return reviewAnswer(false), nil }
			}},
	}
	for _, shaped := range []string{"hunter2Secret", "q3XrT9vLm2Pz8KdWb1YcNa"} {
		word := shaped + "=="
		needles := []string{shaped, word}
		rows = append(rows,
			row{name: "a pasted secret shaped like a field name as a --set word: " + shaped, in: amazon([]string{word}), code: clierr.CodeValidation, needles: needles},
			row{name: "a pasted secret shaped like a field name as a --from-file word: " + shaped, in: amazon(nil, word), code: clierr.CodeValidation, needles: needles},
			row{name: "a pasted secret shaped like a field name given twice: " + shaped, in: amazon([]string{word, word}), code: clierr.CodeValidation, needles: needles})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, format := range []string{"json", ""} {
				files := map[string]string{"secret.txt": secret + "\n"}
				maps.Copy(files, tc.files)
				r := ccsRig(files)
				if tc.prep != nil {
					tc.prep(r)
				}
				written, _, ce := ccsExecute(t, r, tc.in, format, false)
				require.NotNil(t, ce, "the row ends with an error")
				assert.Equal(t, tc.code, ce.Code, ce.Message)
				assert.Zero(t, r.norman.creates, "a refusal sends no request")
				assertNoForms(t, slices.Concat([]string{secret}, tc.needles), written, "what the run wrote")
				assert.NotContains(t, written, "OS text")
			}
		})
	}
}
