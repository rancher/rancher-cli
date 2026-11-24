package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

const (
	localProxy        = "/k8s/clusters/local"
	extTokenReadPath  = localProxy + "/apis/ext.cattle.io/v1/tokens/"
	selfUserPath      = localProxy + "/apis/ext.cattle.io/v1/selfusers"
	proxiedV3Tokens   = "/apis/management.cattle.io/v3/tokens"
	v3TokenReadPrefix = "/v3/tokens/"

	noNameWarning = "token metadata unavailable: the token is not in the form <name>:<secret> or ext/<name>:<secret>; its details were not read"
)

// routeResponse is what a status server responds with to one route; a zero
// status means the route's normal 200 response.
type routeResponse struct {
	status int
	body   string
}

// statusRoutes are the responses of the routes auth status reads the token and
// the identity from.
type statusRoutes struct {
	v3, ext, selfUser routeResponse
}

type recordedRequest struct {
	method, path, authorization string
}

// statusServer is a TLS Rancher whose facade records every request it
// handles and responds to the routes auth status uses: the version of the local
// cluster, the identity read, the ext token read and Rancher's Norman token API.
// The Kubernetes API read of a v3 token is always refused with 403.
type statusServer struct {
	*installIDServer
	fix  tlsFixture
	mu   sync.Mutex
	seen []recordedRequest
}

func newStatusServer(t *testing.T, routes statusRoutes, seedToken string) *statusServer {
	t.Helper()
	s := &statusServer{}
	facade := testrancher.New(testrancher.Options{
		SeedToken: seedToken,
		RequestHook: func(r *http.Request) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.seen = append(s.seen, recordedRequest{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")})
		},
		Fallthrough: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.EscapedPath()
			name := path[strings.LastIndex(path, "/")+1:]
			if unescaped, err := url.PathUnescape(name); err == nil {
				name = unescaped
			}
			switch {
			case r.Method == http.MethodGet && path == localProxy+"/version":
				respond(w, routeResponse{}, `{"major":"1","minor":"31","gitVersion":"v1.31.0"}`)
			case r.Method == http.MethodPost && path == selfUserPath:
				respond(w, routes.selfUser, `{"apiVersion":"ext.cattle.io/v1","kind":"SelfUser","status":{"userID":"u-1"}}`)
			case r.Method == http.MethodGet && strings.HasPrefix(path, extTokenReadPath):
				respond(w, routes.ext, `{"apiVersion":"ext.cattle.io/v1","kind":"Token","metadata":{"name":"`+name+`"},`+
					`"spec":{"kind":"session","userPrincipal":{"provider":"local"}},"status":{"expiresAt":"2031-01-01T00:00:00Z","current":true}}`)
			case r.Method == http.MethodGet && strings.HasPrefix(path, v3TokenReadPrefix):
				respond(w, routes.v3, `{"type":"token","id":"`+name+`","name":"`+name+`","authProvider":"local","isDerived":false,`+
					`"current":true,"expiresAt":"2031-01-01T00:00:00Z"}`)
			case strings.Contains(path, proxiedV3Tokens):
				respond(w, routeResponse{status: http.StatusForbidden}, kubeStatus(http.StatusForbidden, "Forbidden",
					`tokens.management.cattle.io "`+name+`" is forbidden: User "u-1" cannot get resource "tokens" in API group "management.cattle.io" at the cluster scope`))
			default:
				http.NotFound(w, r)
			}
		}),
	})
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	s.fix = fix
	s.installIDServer = newInstallIDServer(t, fix, facade)
	return s
}

func respond(w http.ResponseWriter, a routeResponse, normal string) {
	w.Header().Set("Content-Type", "application/json")
	if a.status == 0 {
		_, _ = w.Write([]byte(normal))
		return
	}
	w.WriteHeader(a.status)
	_, _ = w.Write([]byte(a.body))
}

// kubeStatus is the body of a Kubernetes Status response.
func kubeStatus(code int, reason, message string) string {
	body, err := json.Marshal(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"message": message, "reason": reason, "code": code,
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func (s *statusServer) requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.seen...)
}

// count is the number of recorded requests with the given method and path.
func (s *statusServer) count(method, path string) int {
	n := 0
	for _, r := range s.requests() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

// command is auth status as the CLI builds it, for an entry that holds the
// stored token stored:token and trusts the server's CA.
func (s *statusServer) command(t *testing.T) runtime.Command {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers = map[string]config.Server{"default": {URL: s.URL, CA: s.fix.pem}}
	cfg.CurrentServer = "default"
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "default"}, "stored:token"))
	return NewCheck(dir, store)
}

// isolateStatusEnvironment keeps a developer's environment out of the run;
// token is the bearer of RANCHER_CLI_TOKEN, empty to use the stored token.
func isolateStatusEnvironment(t *testing.T, token string) {
	t.Helper()
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(config.EnvToken, token)
}

// runStatus runs auth status against s and returns its result.
func runStatus(t *testing.T, s *statusServer) runtime.Result {
	t.Helper()
	res, err := s.command(t).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)
	return res
}

// TestAuthStatusNeverReadsAV3TokenThroughTheKubernetesAPI asserts that auth
// status, built as the CLI builds it with every probe, sends no request to
// any path under the Kubernetes API's management.cattle.io v3 tokens (the
// local cluster's RBAC refuses it to everyone but administrators) and no
// request to a tokens path other than the one read the token needs: Rancher's
// Norman token API for a v3 token, the ext.cattle.io tokens of the local cluster for
// an ext token.
func TestAuthStatusNeverReadsAV3TokenThroughTheKubernetesAPI(t *testing.T) {
	rows := []struct {
		name  string
		token string
		want  recordedRequest
	}{
		{"v3 token from the variable", "envtok:secret", recordedRequest{http.MethodGet, v3TokenReadPrefix + "envtok", "Bearer envtok:secret"}},
		{"v3 token stored with the entry", "", recordedRequest{http.MethodGet, v3TokenReadPrefix + "stored", "Bearer stored:token"}},
		{"ext token", "ext/x:secret", recordedRequest{http.MethodGet, extTokenReadPath + "x", "Bearer ext/x:secret"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearFetcherRecord(t)
			isolateStatusEnvironment(t, row.token)
			srv := newStatusServer(t, statusRoutes{}, row.token)
			res := runStatus(t, srv)

			wanted := 0
			for _, r := range srv.requests() {
				assert.NotContains(t, r.path, proxiedV3Tokens, "no v3 token read through the Kubernetes API")
				if !strings.Contains(r.path, "tokens") {
					continue
				}
				assert.Equal(t, row.want, r, "the one tokens request the row expects")
				wanted++
			}
			assert.Equal(t, 1, wanted, "exactly one tokens request")
			assert.NotNil(t, res.Data.(map[string]any)["token"], "the token is read")
		})
	}
}

// TestAuthStatusReportsAV3TokenForEveryTokenSource asserts the v3 token read
// sends the bearer of the effective token, from RANCHER_CLI_TOKEN or from the
// entry, and fills the token object with no token warning.
func TestAuthStatusReportsAV3TokenForEveryTokenSource(t *testing.T) {
	rows := []struct {
		name, token, bearer, tokenName string
	}{
		{"the variable", "envtok:secret", "Bearer envtok:secret", "envtok"},
		{"the stored token", "", "Bearer stored:token", "stored"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearFetcherRecord(t)
			isolateStatusEnvironment(t, row.token)
			srv := newStatusServer(t, statusRoutes{}, "")
			res := runStatus(t, srv)

			assert.Equal(t, 1, srv.count(http.MethodGet, v3TokenReadPrefix+row.tokenName))
			for _, r := range srv.requests() {
				if r.path == v3TokenReadPrefix+row.tokenName {
					assert.Equal(t, row.bearer, r.authorization)
				}
			}
			token, ok := res.Data.(map[string]any)["token"].(map[string]any)
			require.True(t, ok, "the token object is filled")
			assert.Equal(t, row.tokenName, token["name"])
			assert.Equal(t, "session", token["kind"])
			assert.Equal(t, "local", token["provider"])
			assert.Equal(t, "2031-01-01T00:00:00Z", token["expiresAt"])
			assert.Equal(t, true, token["current"])
			assert.Empty(t, res.Warnings)
		})
	}
}

// TestAuthStatusV3TokenResponses asserts what auth status reports when
// Rancher's Norman token API refuses the v3 token read: a null token and a warning,
// with ok true; the refusal's message is in the warning, a 404 is the rotated
// token advisory: the login command for a stored login, the instruction to
// set RANCHER_CLI_TOKEN for a token from the variable.
func TestAuthStatusV3TokenResponses(t *testing.T) {
	const variable = "envtok:secret"
	rows := []struct {
		name string
		// token is the bearer of RANCHER_CLI_TOKEN, empty to use the stored
		// token.
		token    string
		response routeResponse
		// exact is the whole warning, when the row has one; otherwise the
		// warning starts with the token lead and holds contains.
		exact    string
		contains string
	}{
		{"403 with a message", variable, routeResponse{http.StatusForbidden, `{"type":"error","code":"PermissionDenied","message":"can not get token","status":403}`},
			"", "can not get token"},
		{"404 for the token of the variable", variable, routeResponse{http.StatusNotFound, `{"type":"error","code":"NotFound","message":"envtok not found","status":404}`},
			"token metadata unavailable: the server has no such token (revoked or rotated); set RANCHER_CLI_TOKEN to another token", ""},
		{"404 for the stored login", "", routeResponse{http.StatusNotFound, `{"type":"error","code":"NotFound","message":"stored not found","status":404}`},
			"token metadata unavailable: the server has no such token (revoked or rotated); re-authenticate with: " + clierr.Rancher + " auth login", ""},
		{"401 from the authentication filter", variable, routeResponse{http.StatusUnauthorized, `{"type":"error","status":"401","message":"Unauthorized 401: must authenticate"}`},
			"token metadata unavailable: the server rejected the credential (401)", ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clearFetcherRecord(t)
			isolateStatusEnvironment(t, row.token)
			srv := newStatusServer(t, statusRoutes{v3: row.response}, "")
			res := runStatus(t, srv)
			data := res.Data.(map[string]any)
			assert.Nil(t, data["token"])
			assert.Equal(t, true, data["ok"])
			require.Len(t, res.Warnings, 1)
			if row.exact != "" {
				assert.Equal(t, row.exact, res.Warnings[0])
				return
			}
			assert.True(t, strings.HasPrefix(res.Warnings[0], "token metadata unavailable: "), res.Warnings[0])
			assert.Contains(t, res.Warnings[0], row.contains)
		})
	}
}

// TestAuthStatusWarningKeepsTheServerMessage asserts a refused ext token read
// or identity read shows the server's message in its warning, and that a
// message with a line break and a control character stays on one warning line
// of standard error with its visible text.
func TestAuthStatusWarningKeepsTheServerMessage(t *testing.T) {
	const extForbidden = `tokens.ext.cattle.io "x" is forbidden: User "u-1" cannot get resource "tokens" in API group "ext.cattle.io" at the cluster scope`
	const selfUserForbidden = `selfusers.ext.cattle.io is forbidden: User "u-1" cannot create resource "selfusers" in API group "ext.cattle.io" at the cluster scope`

	t.Run("the ext token read", func(t *testing.T) {
		clearFetcherRecord(t)
		isolateStatusEnvironment(t, "ext/x:secret")
		srv := newStatusServer(t, statusRoutes{ext: routeResponse{http.StatusForbidden, kubeStatus(http.StatusForbidden, "Forbidden", extForbidden)}}, "ext/x:secret")
		res := runStatus(t, srv)
		assert.Nil(t, res.Data.(map[string]any)["token"])
		require.Len(t, res.Warnings, 1)
		assert.True(t, strings.HasPrefix(res.Warnings[0], "token metadata unavailable: "), res.Warnings[0])
		assert.Contains(t, res.Warnings[0], extForbidden)
	})

	t.Run("the identity read", func(t *testing.T) {
		clearFetcherRecord(t)
		isolateStatusEnvironment(t, "envtok:secret")
		srv := newStatusServer(t, statusRoutes{selfUser: routeResponse{http.StatusForbidden, kubeStatus(http.StatusForbidden, "Forbidden", selfUserForbidden)}}, "")
		res := runStatus(t, srv)
		assert.Nil(t, res.Data.(map[string]any)["identity"])
		require.Len(t, res.Warnings, 1)
		assert.True(t, strings.HasPrefix(res.Warnings[0], "identity metadata unavailable: "), res.Warnings[0])
		assert.Contains(t, res.Warnings[0], selfUserForbidden)
	})

	t.Run("a message with a line break and a control character", func(t *testing.T) {
		clearFetcherRecord(t)
		isolateStatusEnvironment(t, "ext/x:secret")
		message := "denied before\nthe break and\u0007after the bell"
		srv := newStatusServer(t, statusRoutes{ext: routeResponse{http.StatusForbidden, kubeStatus(http.StatusForbidden, "Forbidden", message)}}, "ext/x:secret")
		var out, errBuf bytes.Buffer
		_, ce := runtime.Run(t.Context(), srv.command(t), cmdtest.Input{},
			runtime.IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
		require.Nil(t, ce)

		stderr := errBuf.String()
		assert.NotContains(t, stderr, "\u0007", "the control character is not written")
		var warnings []string
		for line := range strings.SplitSeq(stderr, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				continue
			}
			assert.True(t, strings.HasPrefix(line, "# warning: "), "every line of standard error is a warning line: %q", line)
			warnings = append(warnings, line)
		}
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "token metadata unavailable: ")
		for _, visible := range []string{"denied before", "the break and", "after the bell"} {
			assert.Contains(t, warnings[0], visible)
		}
	})
}

// TestAuthStatusSendsNoTokenReadForATokenWithoutAName asserts a bearer with no
// name before its ':' (no ':', or an empty name) has no token name: auth
// status sends no token read, for the v3 and the ext form alike, keeps the
// bearer out of every request path, and warns.
func TestAuthStatusSendsNoTokenReadForATokenWithoutAName(t *testing.T) {
	for _, row := range []struct{ token, secret string }{
		{"abc", "abc"}, {"ext/abc", "abc"}, {":secret", "secret"}, {"ext/:secret", "secret"},
	} {
		token := row.token
		t.Run(token, func(t *testing.T) {
			clearFetcherRecord(t)
			isolateStatusEnvironment(t, token)
			srv := newStatusServer(t, statusRoutes{}, "")
			dir := t.TempDir()
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.Servers = map[string]config.Server{"default": {URL: srv.URL, CA: srv.fix.pem}}
			cfg.CurrentServer = "default"
			require.NoError(t, cfg.Write())
			cmd := &authCheckCmd{
				dir: dir, store: config.NewFileStore(dir),
				preflight:      alwaysOKPreflight,
				rancherVersion: alwaysOKVersion,
				identity:       func(context.Context, config.Effective) (string, error) { return "u-1", nil },
				token:          provider.EffectiveTokenMetadata,
				v3token:        provider.V3TokenMetadata,
			}
			res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
			require.NoError(t, err)

			for _, r := range srv.requests() {
				assert.NotContains(t, r.path, "tokens")
				assert.NotContains(t, r.path, row.secret)
			}
			data := res.Data.(map[string]any)
			assert.Nil(t, data["token"])
			assert.Equal(t, true, data["ok"])
			assert.Equal(t, []string{noNameWarning}, res.Warnings)
		})
	}
}
