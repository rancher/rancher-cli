package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

const (
	tokenLookupText = "kubeconfigs.ext.cattle.io is forbidden: error getting request token token-abc: " +
		`tokens.management.cattle.io "token-abc" not found`
	kubeconfigsPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/kubeconfigs"
)

// tokenLookupServer answers the version endpoint, discovery and the
// Kubeconfig create. forbidDryRun and forbidCreate make the dry-run and the
// real create answer a 403 with forbidText.
type tokenLookupServer struct {
	version      string
	versionCode  int
	forbidText   string
	forbidDryRun bool
	forbidCreate bool

	mu       sync.Mutex
	versions int
	creates  int
}

func (s *tokenLookupServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	write := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	status := func(code int, reason, msg string) {
		write(code, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
			"status": "Failure", "message": msg, "reason": reason, "code": code})
	}
	switch {
	case r.URL.Path == "/rancherversion":
		s.mu.Lock()
		s.versions++
		s.mu.Unlock()
		if s.versionCode != 0 && s.versionCode != http.StatusOK {
			http.Error(w, "unavailable", s.versionCode)
			return
		}
		write(http.StatusOK, map[string]any{"Version": s.version, "GitCommit": "x", "RancherPrime": "false"})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/apis"):
		gv := map[string]any{"groupVersion": "ext.cattle.io/v1", "version": "v1"}
		write(http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{
			map[string]any{"name": "ext.cattle.io", "versions": []any{gv}, "preferredVersion": gv}}})
	case r.Method == http.MethodPost && r.URL.Path == kubeconfigsPath:
		dryRun := r.URL.Query().Get("dryRun") != ""
		if !dryRun {
			s.mu.Lock()
			s.creates++
			s.mu.Unlock()
		}
		if (dryRun && s.forbidDryRun) || (!dryRun && s.forbidCreate) {
			status(http.StatusForbidden, "Forbidden", s.forbidText)
			return
		}
		obj := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&obj)
		md := map[string]any{"name": "kubeconfig-abc", "resourceVersion": "1"}
		obj["metadata"] = md
		if !dryRun {
			obj["status"] = map[string]any{"value": "apiVersion: v1\nkind: Config\n"}
		}
		write(http.StatusCreated, obj)
	default:
		status(http.StatusNotFound, "NotFound", "not found")
	}
}

func (s *tokenLookupServer) versionRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.versions
}

type tokenLookupRun struct {
	err    *callerError
	exit   int
	stdout string
	stderr string
	// cfgDir is the config directory the run used, which its hints name.
	cfgDir string
}

// runTokenLookup runs create kubeconfig with args against s, logged in as
// entry "prod" in a config directory other than the default, or with
// RANCHER_CLI_TOKEN set to envToken instead of a stored login.
func runTokenLookup(t *testing.T, s *tokenLookupServer, envToken string, args ...string) tokenLookupRun {
	t.Helper()
	keyring.MockInit()
	hs := httptest.NewTLSServer(s)
	t.Cleanup(hs.Close)

	cfgDir, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("KUBECONFIG", filepath.Join(home, "kubeconfig"))
	t.Setenv(config.EnvConfigDir, "")
	t.Setenv(config.EnvCredentialStore, "file")
	require.NoError(t, os.Unsetenv(config.EnvURL))
	require.NoError(t, os.Unsetenv(config.EnvConfigDir))
	if envToken == "" {
		require.NoError(t, os.Unsetenv(config.EnvToken))
	} else {
		t.Setenv(config.EnvToken, envToken)
	}
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod",
		Servers: map[string]config.Server{"prod": {URL: hs.URL, CA: string(pemFromServer(t, hs))}}}
	cfg.SetPath(config.FilePath(cfgDir))
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(cfgDir).SetToken(config.ServerRef{Name: "prod", URL: hs.URL}, "ext/token-abc:secret"))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer, root.ErrWriter = &out, &errOut
	root.Reader = strings.NewReader("")
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	argv := append([]string{"--config", cfgDir, "create", "kubeconfig"}, args...)
	CaptureArgv(argv)
	t.Cleanup(func() { CaptureArgv(nil) })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	runErr := root.Run(ctx, append([]string{"rancher"}, argv...))

	run := tokenLookupRun{stdout: out.String(), stderr: errOut.String(), cfgDir: cfgDir}
	if coder, ok := runErr.(ucli.ExitCoder); ok {
		run.exit = coder.ExitCode()
	}
	// The command accepts only the table format, so its error is the text
	// block: a header line with the code, then the indented hint line.
	for line := range strings.Lines(run.stderr) {
		line = strings.TrimRight(line, "\n")
		if m := errorHeader.FindStringSubmatch(line); m != nil {
			run.err = &callerError{Code: m[1], Message: m[2]}
		} else if hint, ok := strings.CutPrefix(line, "  hint: "); ok && run.err != nil {
			run.err.Hint = hint
		}
	}
	return run
}

var errorHeader = regexp.MustCompile(`^error \(([A-Z_]+)\): (.*)$`)

// TestCreateKubeconfigOnOldServerNamesTheVersionItNeeds asserts the command
// end to end on a server that reports a version below 2.15: the request-token
// 403 of the dry-run becomes CAPABILITY_UNSUPPORTED (exit 11) with the login
// command as the hint, and a server at or above 2.15 keeps RBAC (exit 6).
func TestCreateKubeconfigOnOldServerNamesTheVersionItNeeds(t *testing.T) {
	tests := []struct {
		name      string
		version   string
		args      []string
		envToken  string
		wantCode  string
		wantExit  int
		wantHint  string
		wantAsked int
		wantMsg   string
	}{
		{"2.14.5 with a stored login", "v2.14.5", []string{"--apply", "--yes"}, "", clierr.CodeCapabilityUnsupported, 11,
			"rancher auth login 'prod' --token-stdin", 1,
			"Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or an API token (pipe id:secret to rancher auth login 'prod' --token-stdin)"},
		{"2.14.5 dry-run", "v2.14.5", []string{"--dry-run"}, "", clierr.CodeCapabilityUnsupported, 11,
			"rancher auth login 'prod' --token-stdin", 1,
			"Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or an API token (pipe id:secret to rancher auth login 'prod' --token-stdin)"},
		{"2.14.5 with RANCHER_CLI_TOKEN", "v2.14.5", []string{"--apply", "--yes"}, "ext/env-token:secret", clierr.CodeCapabilityUnsupported, 11,
			"", 1, "Rancher v2.14.5 cannot create a kubeconfig with the token this command used; use Rancher 2.15 or later, or set RANCHER_CLI_TOKEN to an API token"},
		{"2.15.2 keeps RBAC", "v2.15.2", []string{"--apply", "--yes"}, "", clierr.CodeRBAC, 6, "", 1, ""},
		{"dev build keeps RBAC", "dev", []string{"--apply", "--yes"}, "", clierr.CodeRBAC, 6, "", 1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &tokenLookupServer{version: tt.version, forbidText: tokenLookupText, forbidDryRun: true}
			run := runTokenLookup(t, s, tt.envToken, tt.args...)
			require.NotNil(t, run.err, "stdout: %s stderr: %s", run.stdout, run.stderr)
			assert.Equal(t, tt.wantCode, run.err.Code)
			assert.Equal(t, tt.wantExit, run.exit)
			assert.Equal(t, withDirectory(run.cfgDir, tt.wantHint), run.err.Hint)
			assert.Equal(t, tt.wantAsked, s.versionRequests())
			assert.Zero(t, s.creates, "the refusal comes from the dry-run")
			if tt.wantCode == clierr.CodeCapabilityUnsupported {
				assert.Equal(t, withDirectory(run.cfgDir, tt.wantMsg), run.err.Message)
			} else {
				assert.Contains(t, run.err.Message, tokenLookupText)
			}
		})
	}
}

// TestCreateKubeconfigKeepsOtherForbiddenErrors asserts that a server below
// 2.15 answering a 403 with other text, or with the request-token text and an
// unreadable version, still ends in RBAC.
func TestCreateKubeconfigKeepsOtherForbiddenErrors(t *testing.T) {
	tests := []struct {
		name string
		s    *tokenLookupServer
	}{
		{"other text", &tokenLookupServer{version: "v2.14.5", forbidText: "kubeconfigs.ext.cattle.io is forbidden: nope", forbidDryRun: true}},
		{"version endpoint fails", &tokenLookupServer{version: "v2.14.5", versionCode: http.StatusInternalServerError, forbidText: tokenLookupText, forbidDryRun: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := runTokenLookup(t, tt.s, "", "--apply", "--yes")
			require.NotNil(t, run.err)
			assert.Equal(t, clierr.CodeRBAC, run.err.Code)
			assert.Equal(t, 6, run.exit)
		})
	}
}

// TestCreateKubeconfigRequestsTheVersionOnlyAfterAFailure asserts that a
// create that succeeds never reads the server version.
func TestCreateKubeconfigRequestsTheVersionOnlyAfterAFailure(t *testing.T) {
	s := &tokenLookupServer{version: "v2.14.5"}
	run := runTokenLookup(t, s, "", "--apply", "--yes")
	assert.Nil(t, run.err)
	assert.Zero(t, run.exit)
	assert.Contains(t, run.stdout, "kind: Config")
	assert.Equal(t, 1, s.creates)
	assert.Zero(t, s.versionRequests())
}

// TestCreateKubeconfigRefusesWhenOnlyTheCreateFails asserts the refusal on
// the real create when the dry-run passed.
func TestCreateKubeconfigRefusesWhenOnlyTheCreateFails(t *testing.T) {
	s := &tokenLookupServer{version: "v2.14.5", forbidText: tokenLookupText, forbidCreate: true}
	run := runTokenLookup(t, s, "", "--apply", "--yes")
	require.NotNil(t, run.err, "stderr: %s", run.stderr)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, run.err.Code)
	assert.Equal(t, 11, run.exit)
	assert.Equal(t, 1, s.creates)
	assert.Equal(t, 1, s.versionRequests())
}

// TestClientFactoryServerVersionAndStoredLogin asserts the factory's answers
// behind the two optional engine interfaces: the version as the server
// states it, fetched once; and the stored login's entry name, empty when
// RANCHER_CLI_TOKEN or a RANCHER_CLI_URL for another server is in use.
func TestClientFactoryServerVersionAndStoredLogin(t *testing.T) {
	s := &tokenLookupServer{version: "v2.14.5"}
	srv := httptest.NewTLSServer(s)
	t.Cleanup(srv.Close)
	keyring.MockInit()
	t.Setenv(config.EnvURL, "")
	require.NoError(t, os.Unsetenv(config.EnvURL))
	t.Setenv(config.EnvToken, "")
	require.NoError(t, os.Unsetenv(config.EnvToken))

	f := seedRootConfig(t, srv)
	got, err := f.ServerVersion(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "v2.14.5", got)
	_, err = f.ServerVersion(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, s.versionRequests(), "the version is fetched once per invocation")

	name, err := f.StoredLoginName()
	require.NoError(t, err)
	assert.Equal(t, "acme", name)

	t.Setenv(config.EnvToken, "ext/env-token:secret")
	name, err = seedRootConfig(t, srv).StoredLoginName()
	require.NoError(t, err)
	assert.Empty(t, name, "RANCHER_CLI_TOKEN is the bearer, not a stored login")

	t.Setenv(config.EnvToken, "")
	require.NoError(t, os.Unsetenv(config.EnvToken))
	t.Setenv(config.EnvURL, "https://other.example.com")
	name, err = seedRootConfig(t, srv).StoredLoginName()
	require.NoError(t, err)
	assert.Empty(t, name, "another server's URL clears the entry")
}
