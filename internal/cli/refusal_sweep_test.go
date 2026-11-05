package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// refusalExemption is a command that does not end with the refusal's
// code: its exit under the answers it names, for the --cluster values it
// names (every value when none), and why. deletes is the number of DELETE
// requests the command sends under each answer; an answer it omits is not
// checked.
type refusalExemption struct {
	statuses []int
	clusters []string
	exit     int
	reason   string
	deletes  map[int]int
}

// applies reports whether e covers a run under status with cluster.
func (e refusalExemption) applies(status int, cluster string) bool {
	return slices.Contains(e.statuses, status) && (len(e.clusters) == 0 || slices.Contains(e.clusters, cluster))
}

// localDeletionReason is why the commands that delete a stored login
// exit 0 whatever the server answers.
const localDeletionReason = "local deletion is the command; a refused revoke is reported in data.notRevoked with its reason, retry and recovery"

// refusalExemptions are the commands that do not end with the refusal's
// code.
var refusalExemptions = map[string]refusalExemption{
	"config add-server":              {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, "the TLS check reads /rancherversion for its certificate only; any answer proves the trust", nil},
	"config update-server":           {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, "the TLS check reads /rancherversion for its certificate only; any answer proves the trust", nil},
	"kubeconfig":                     {[]int{http.StatusForbidden}, []string{"local"}, 0, "a 403 on a direct-endpoint read is an omission notice and the proxy-only file is printed; a 401 fails with AUTH", nil},
	"support-bundle":                 {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, "a bundle records each refused read and goes on", nil},
	"patch clusterregistrationtoken": {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 2, "refused before any request: the kind is read-only", nil},
	"patch management-token":         {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 2, "refused before any request: Rancher sets the token fields itself and the CLI does not change them", nil},
	"patch clusterrepo":              {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 2, "refused before any request: the CLI does not change repositories", nil},
	"patch app":                      {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 2, "refused before any request: Rancher writes app objects from the Helm release", nil},
	"patch operation":                {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 2, "refused before any request: Rancher writes operation objects", nil},
	"auth logout": {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, localDeletionReason,
		map[int]int{http.StatusUnauthorized: 3, http.StatusForbidden: 2}},
	"config delete-server": {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, localDeletionReason,
		map[int]int{http.StatusUnauthorized: 3, http.StatusForbidden: 2}},
	"auth delete-token": {[]int{http.StatusUnauthorized, http.StatusForbidden}, nil, 0, localDeletionReason,
		map[int]int{http.StatusUnauthorized: 1, http.StatusForbidden: 1}},
}

// refusalStoredLogin lists the commands that delete a login the CLI
// created: each needs one stored in its config directory, with a token
// the CLI created for the server, before it runs.
var refusalStoredLogin = []string{"auth logout", "auth delete-token", "config delete-server"}

// refusalStoredServer lists the commands that name a server with a flag
// that has no --cacert beside it: each trusts the server through the CA of
// the configured server entry whose URL matches, so its run clears
// RANCHER_CLI_CACERT, which would be the CA used instead.
var refusalStoredServer = []string{"token"}

// writeStoredServer stores, in the config directory dir, the server a1 at
// url trusting the CA in caPEM, with no credential.
func writeStoredServer(t *testing.T, dir, url, caPEM string) {
	t.Helper()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "a1", Servers: map[string]config.Server{
		"a1": {URL: url, CA: caPEM},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// writeStoredLogin stores, in the config directory dir, the server a1 at
// url trusting the CA in caPEM, with a login token and a cached cluster
// token the CLI created.
func writeStoredLogin(t *testing.T, dir, url, caPEM string) {
	t.Helper()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "a1", Servers: map[string]config.Server{
		"a1": {URL: url, CA: caPEM, Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-login"}, TokenUserID: "u-1", CacheBindingID: "b1"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "a1", URL: url}, "ext/token-login:secret"))
	b := config.CacheBindingFor(config.CanonicalDir(dir), cfg, "a1").WithCreatedToken(&clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cache"}, "u-1")
	require.NoError(t, store.SetCachedCred("a1@"+config.CanonicalServerIdentity(url)+"/c-1", "ext/token-cache:secret", time.Now().Add(time.Hour),
		"a1", config.CanonicalServerIdentity(url), "c-1", b))
}

// refusalSetup is the command line a command needs run first in its
// config directory; "{url}" is the server's URL and "{ca}" the path of its
// CA file.
var refusalSetup = map[string][]string{
	"config update-server": {"config", "add-server", "a1", "--url", "{url}", "--cacert", "{ca}"},
}

// refusalArgs completes a command line the manifest alone cannot: the
// values a command checks before its first request.
var refusalArgs = map[string][]string{
	"create cluster": {"--machine-config", "amazonec2config/x", "--cloud-credential", "cc", "--k8s-version", "v1.33.1+rke2r1", "--etcd", "--controlplane", "--worker"},
	"grant":          {"--global"},
	"revoke":         {"--global"},
}

// refusalServer answers every request with one status and body and counts
// the requests and, apart, the DELETE requests.
type refusalServer struct {
	status  int
	body    string
	n       atomic.Int64
	deletes atomic.Int64
}

func (s *refusalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.n.Add(1)
	if r.Method == http.MethodDelete {
		s.deletes.Add(1)
	}
	if s.body != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(s.status)
	_, _ = w.Write([]byte(s.body))
}

// refusalArgv is c's command line: its positionals, a value for every
// required flag and for --cluster and --namespace when it takes them
// (cluster is the --cluster value), a JSON patch for --patch, the stdin
// flags, the server's URL where a flag names a server, the path of its CA
// file where a flag takes one, and refusalArgs.
func refusalArgv(c runtime.SchemaCommand, cluster, url, caPath, manifest string) []string {
	argv := append(strings.Fields(c.Name), argValues(c.Args)...)
	extra := refusalArgs[c.Name]
	for _, f := range c.Flags {
		if slices.Contains(extra, "--global") && f.Name == "cluster" {
			continue
		}
		switch {
		case f.Name == "cluster":
			argv = append(argv, "--cluster", cluster)
		case f.Name == "namespace":
			argv = append(argv, "--namespace", "default")
		case f.Name == "patch":
			argv = append(argv, "--patch", `[{"op":"test","path":"/metadata/name","value":"a1"}]`)
		case f.Name == "password-stdin" || f.Name == "token-stdin":
			argv = append(argv, "--"+f.Name)
		case f.Name == "url" || f.Name == "server":
			argv = append(argv, "--"+f.Name, url)
		case f.Name == "cacert":
			argv = append(argv, "--cacert", caPath)
		case f.Name == "filename" && f.Required:
			argv = append(argv, "--filename", manifest)
		case f.Required && len(f.Enum) > 0:
			argv = append(argv, "--"+f.Name, f.Enum[0])
		case f.Required:
			argv = append(argv, "--"+f.Name, "1")
		}
	}
	return append(argv, extra...)
}

// takesCluster reports whether c has a --cluster flag.
func takesCluster(c runtime.SchemaCommand) bool {
	return slices.ContainsFunc(c.Flags, func(f runtime.ArgSpec) bool { return f.Name == "cluster" })
}

// refusalCode reads the error code a run wrote to stderr: the JSON
// envelope, or the "error (CODE): " line of the text form.
func refusalCode(stderr string) string {
	if i := strings.Index(stderr, "{\n"); i >= 0 {
		var env struct {
			Data struct {
				Error struct{ Code string } `json:"error"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(stderr[i:]), &env) == nil {
			return env.Data.Error.Code
		}
	}
	for l := range strings.SplitSeq(stderr, "\n") {
		if rest, ok := strings.CutPrefix(l, "error ("); ok {
			code, _, _ := strings.Cut(rest, ")")
			return code
		}
	}
	return ""
}

// assertKubeconfigWithOmission asserts that a kubeconfig run whose
// direct-endpoint read was refused printed a kubeconfig file on stdout and
// the omission notice on stderr.
func assertKubeconfigWithOmission(t *testing.T, label, stdout, stderr string) {
	t.Helper()
	var kc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(stdout), &kc), "%s: stdout is not a kubeconfig: %q", label, stdout)
	assert.Equal(t, "v1", kc.APIVersion, label)
	assert.Equal(t, "Config", kc.Kind, label)
	assert.Contains(t, stderr, "direct endpoint contexts omitted", "%s: the omission notice", label)
}

// TestEveryNetworkCommandReportsARefusal asserts that every command the
// manifest gives server access, pointed at a server that answers
// every request 401 with Rancher's error object, exits 3 with AUTH, and
// answering every request 403 with no body, exits 6 with RBAC - unless
// refusalExemptions names it for that answer, and that an exempt command sends
// the DELETE requests it names. A command with --cluster
// runs twice: with the local cluster, which needs no lookup, and with a
// cluster id, which the lookup through steve must report. A command that
// sends no request with its synthesized command line fails until
// refusalArgs completes it. Each run has its own config directory. Not
// parallel: it sets the process environment and the captured command
// line.
func TestEveryNetworkCommandReportsARefusal(t *testing.T) {
	restoreRawArgv(t)
	keyring.MockInit()
	for _, v := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"401", http.StatusUnauthorized, `{"type":"error","status":"401","message":"Unauthorized 401: must authenticate"}`, clierr.CodeAuth},
		{"403", http.StatusForbidden, "", clierr.CodeRBAC},
	} {
		srv := &refusalServer{status: v.status, body: v.body}
		hs := httptest.NewTLSServer(srv)
		t.Cleanup(hs.Close)
		caPEM := string(pemFromServer(t, hs))
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		t.Setenv(config.EnvCredentialStore, "file")
		t.Setenv(config.EnvURL, hs.URL)
		t.Setenv(config.EnvCACert, caPEM)
		t.Setenv(config.EnvToken, "token-x:y")
		wd := t.TempDir()
		t.Chdir(wd)
		caPath := filepath.Join(wd, "ca.pem")
		require.NoError(t, os.WriteFile(caPath, []byte(caPEM), 0o600))
		manifest := filepath.Join(wd, "m.yaml")
		require.NoError(t, os.WriteFile(manifest, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: default\n"), 0o600))

		wr, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
		require.NoError(t, err)
		m := wr.reg.Schema("v-test", config.MinRancherVersion)
		names := map[string]bool{}
		for _, c := range m.Commands {
			names[c.Name] = true
			if c.ServerAccess == runtime.ServerAccessNone {
				continue
			}
			clusters := []string{"local"}
			if takesCluster(c) {
				clusters = append(clusters, "c-abc12")
			}
			for _, cluster := range clusters {
				argv := refusalArgv(c, cluster, hs.URL, caPath, manifest)
				if (runtime.OutputPolicy{AllowedFormats: c.OutputPolicy}).Allows("json") {
					argv = append(argv, "-o", "json")
				}
				name := v.name + " " + c.Name + " " + cluster
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					t.Setenv(config.EnvConfigDir, dir)
					if slices.Contains(refusalStoredLogin, c.Name) {
						writeStoredLogin(t, dir, hs.URL, caPEM)
					}
					if slices.Contains(refusalStoredServer, c.Name) {
						t.Setenv(config.EnvCACert, "")
						writeStoredServer(t, dir, hs.URL, caPEM)
					}
					if setup, ok := refusalSetup[c.Name]; ok {
						setup = slices.Clone(setup)
						for i, w := range setup {
							setup[i] = strings.NewReplacer("{url}", hs.URL, "{ca}", caPath).Replace(w)
						}
						setupStderr, setupCode := runOnce(t, wr.root("v-test"), setup)
						require.Zero(t, setupCode, "setup %v failed: %s", setup, setupStderr)
					}
					root := wr.root("v-test")
					root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
					var stdout, stderr bytes.Buffer
					root.Writer, root.ErrWriter = &stdout, &stderr
					root.Reader = strings.NewReader("token-x:y\n")
					CaptureArgv(argv)
					before, deletesBefore := srv.n.Load(), srv.deletes.Load()
					code := exitCodeOf(root, root.Run(t.Context(), append([]string{"rancher"}, argv...)))
					label := "rancher " + strings.Join(argv, " ")
					if ex, ok := refusalExemptions[c.Name]; ok && ex.applies(v.status, cluster) {
						assert.Equal(t, ex.exit, code, "%s: %s", label, ex.reason)
						if want := ex.deletes[v.status]; want > 0 {
							assert.EqualValues(t, want, srv.deletes.Load()-deletesBefore, "%s: DELETE requests sent", label)
						}
						if c.Name == "kubeconfig" {
							assertKubeconfigWithOmission(t, label, stdout.String(), stderr.String())
						}
						return
					}
					require.Positive(t, srv.n.Load()-before, "%s sent no request; complete it in refusalArgs: %s", label, stderr.String())
					assert.Equal(t, clierr.ExitCode(v.code), code, "%s: %s", label, stderr.String())
					assert.Equal(t, v.code, refusalCode(stderr.String()), label)
				})
			}
		}
		for name := range refusalExemptions {
			assert.True(t, names[name], "exemption %q names no command", name)
		}
	}
}
