package cli

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ccCanary    = "fake-cc-canary-secret-0001"
	ccLineOne   = "fake-cc-first-line-0001"
	ccLineTwo   = "fake-cc-second-line-0002"
	ccKube      = "apiVersion: v1\nclusters:\n- name: c\nusers:\n- name: u\n  user:\n    token: kubeconfig-user-x:" + ccCanary + "\n"
	ccNodeDrv   = "/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers"
	ccSecrets   = "/k8s/clusters/local/api/v1/namespaces/cattle-global-data/secrets"
	ccReviews   = "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	ccSettings  = "/k8s/clusters/local/apis/management.cattle.io/v3/settings/kev2-operators"
	ccNormanURL = "/v3/cloudcredentials"
)

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

func ccDriverList() string {
	items := make([]any, 0, len(ccDrivers))
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

// ccNorman is how the stub answers the create: the status and the body.
type ccNorman struct {
	status int
	body   string
	drop   bool
}

// ccStub answers the reads and the review of the command, and the create as
// norman says. The posts it saw are returned through *posts.
func ccStub(norman ccNorman, posts *[]string, listRefused bool) secretStub {
	return func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == ccNodeDrv && r.Method == http.MethodGet:
			writeKubeJSON(w, http.StatusOK, ccDriverList())
		case r.URL.Path == ccSettings:
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"management.cattle.io/v3","kind":"Setting","value":"[{\"name\":\"alibaba\",\"active\":true}]"}`)
		case r.URL.Path == ccReviews:
			writeKubeJSON(w, http.StatusCreated, `{"status":{"allowed":true}}`)
		case r.URL.Path == ccSecrets && listRefused:
			writeKubeJSON(w, http.StatusForbidden, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"secrets is forbidden: User cannot list"}`)
		case r.URL.Path == ccSecrets:
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"SecretList","items":[]}`)
		case strings.HasPrefix(r.URL.Path, ccSecrets+"/"):
			writeKubeJSON(w, http.StatusOK, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"cc-abcde"},"data":{"amazonec2credentialConfig-accessKey":"eA=="}}`)
		case r.URL.Path == ccNormanURL && r.Method == http.MethodPost:
			body := make([]byte, 1<<16)
			n, _ := r.Body.Read(body)
			*posts = append(*posts, string(body[:n]))
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

// ccOutput runs argv with stdin and returns what the run wrote, and every file
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

// ccForms are the forms of a secret no output may hold: as typed, JSON-escaped
// and base64 in every alignment secretinput's canaryForms lists.
func ccForms(secret string) []string {
	forms := append([]string{secret}, canaryForms(secret)...)
	escaped, _ := json.Marshal(secret)
	forms = append(forms, strings.Trim(string(escaped), `"`), base64.StdEncoding.EncodeToString([]byte(secret)))
	return forms
}

// TestCloudCredentialCommandsNeverShowASecretValue asserts, through the real
// entry, that a secret read from stdin or a file is in none of stdout, stderr,
// the decoded envelope and the files the run leaves (the telemetry events),
// as typed, JSON-escaped or in base64, on a preview, a create, and each
// refusal and answer of the create. Not parallel: it sets the environment.
func TestCloudCredentialCommandsNeverShowASecretValue(t *testing.T) {
	restoreRawArgv(t)
	echo := `{"type":"error","code":"MissingRequired","message":"field value ` + ccCanary + ` is missing"}`
	for _, tc := range []struct {
		name   string
		argv   []string
		stdin  string
		norman ccNorman
		list   bool
		code   int
		want   string
	}{
		{"a preview", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--dry-run", "-o", "json"}, ccCanary + "\n", ccNorman{}, false, 0, ""},
		{"a create", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply", "-o", "json"}, ccCanary + "\n",
			ccNorman{status: 201, body: `{"id":"cattle-global-data:cc-abcde","amazonec2credentialConfig":{"secretKey":"` + ccCanary + `"}}`}, false, 0, ""},
		{"a 422 that echoes the value", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply"}, ccCanary + "\n",
			ccNorman{status: 422, body: echo}, false, 0, "VALIDATION"},
		{"a 403 that echoes the value", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply"}, ccCanary + "\n",
			ccNorman{status: 403, body: echo}, false, 0, "RBAC"},
		{"a 500", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply"}, ccCanary + "\n",
			ccNorman{status: 500, body: echo}, false, 0, "GENERIC"},
		{"a lost answer", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply"}, ccCanary + "\n",
			ccNorman{drop: true}, false, 0, "GENERIC"},
		{"a refused display name list", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--dry-run", "-o", "json"}, ccCanary + "\n", ccNorman{}, true, 0, ""},
		{"an extra word after the command line", []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", "secretKey=-", "--dry-run", ccCanary}, ccCanary + "\n", ccNorman{}, false, 0, "VALIDATION"},
		{"a --set word with no equals sign", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", ccCanary, "--from-file", "secretKey=-", "--dry-run"}, ccCanary + "\n", ccNorman{}, false, 0, "VALIDATION"},
		{"a --from-file word with no equals sign", []string{"create", "cloud-credential", "amazonec2", "prod", "--from-file", ccCanary, "--dry-run"}, "", ccNorman{}, false, 0, "VALIDATION"},
		{"a secret given with --set", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "secretKey=" + ccCanary, "--dry-run"}, "", ccNorman{}, false, 0, "VALIDATION"},
		{"a harvester refusal that quotes the kubeconfig", []string{"create", "cloud-credential", "harvester", "h", "--from-file", "kubeconfigContent=-", "--apply"}, ccKube,
			ccNorman{status: 500, body: `{"type":"error","code":"ServerError","message":"failed to process harvester cloud credential: failed to get tokens from kubeconfig: yaml: ` + ccCanary + `"}`}, false, 0, "VALIDATION"},
		{"a value with a byte order mark and CRLF", []string{"create", "cloud-credential", "amazonec2", "prod", "--set", "accessKey=AKIAFAKE", "--from-file", "secretKey=-", "--apply"}, "\xef\xbb\xbf\xef\xbb\xbf" + ccCanary + "\r\n",
			ccNorman{status: 422, body: echo}, false, 0, "VALIDATION"},
		{"a multi-line value with CRLF", []string{"create", "cloud-credential", "google", "g", "--from-file", "authEncodedJson=-", "--apply"}, ccLineOne + "\r\n" + ccLineTwo + "\r\n",
			ccNorman{status: 422, body: `{"type":"error","message":"bad ` + ccLineTwo + `"}`}, false, 0, "VALIDATION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts []string
			stdout, stderr, _, files := ccRun(t, ccStub(tc.norman, &posts, tc.list), tc.argv, tc.stdin)
			secrets := []string{ccCanary, ccLineOne, ccLineTwo}
			for _, secret := range secrets {
				for _, form := range ccForms(secret) {
					assert.NotContains(t, stdout, form, "stdout")
					assert.NotContains(t, stderr, form, "stderr")
					assert.NotContains(t, files, form, "the files the run left")
				}
			}
			if tc.want != "" {
				assert.Contains(t, stdout+stderr, tc.want)
			}
			for _, p := range posts {
				assert.NotContains(t, p, "\ufeff", "the byte order mark never leaves the CLI")
			}
			require.NotEmpty(t, stdout+stderr)
		})
	}
}
