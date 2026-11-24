package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// tokenRequest is what the token server saw of one request.
type tokenRequest struct {
	method, path, query, authorization, userAgent string
}

// tokenServer is a TLS server that responds to GET /v3/tokens/<name> with the
// given status and body, and 404 to every other path. It records each request.
type tokenServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []tokenRequest
}

func newTokenServer(t *testing.T, status int, body func() string) *tokenServer {
	t.Helper()
	s := &tokenServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, tokenRequest{
			method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"), userAgent: r.Header.Get("User-Agent"),
		})
		s.mu.Unlock()
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.EscapedPath(), "/v3/tokens/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body()))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *tokenServer) requests() []tokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tokenRequest(nil), s.seen...)
}

func (s *tokenServer) effective() config.Effective {
	return config.Effective{URL: s.URL, CA: serverCA(s.Server), Token: "tok-1:secret"}
}

func constantBody(body string) func() string { return func() string { return body } }

// TestV3TokenMetadataReadsTheTokenThroughRancherTokenAPI asserts the v3 token
// read sends one GET of Rancher's Norman token API with the bearer, never through the
// Kubernetes API of the local cluster, escapes the token name in the path, and
// maps the response's fields.
func TestV3TokenMetadataReadsTheTokenThroughRancherTokenAPI(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name     string
		token    string
		body     string
		want     TokenMetadata
		wantPath string
	}{
		{
			name:  "a session token",
			token: "token-abc",
			body: `{"type":"token","id":"token-abc","name":"token-abc","authProvider":"local","expiresAt":"2030-01-01T00:00:00Z",` +
				`"isDerived":false,"current":true,"links":{"self":"https://rancher.example/v3/tokens/token-abc"},` +
				`"userPrincipal":{"name":"local://u-1"},"userId":"u-1"}`,
			want:     TokenMetadata{Name: "token-abc", Kind: "session", Provider: "local", ExpiresAt: "2030-01-01T00:00:00Z", Current: true},
			wantPath: "/v3/tokens/token-abc",
		},
		{
			name:     "a derived token",
			token:    "token-key",
			body:     `{"type":"token","id":"token-key","name":"token-key","authProvider":"github","expiresAt":"","isDerived":true,"current":false}`,
			want:     TokenMetadata{Name: "token-key", Kind: "derived", Provider: "github"},
			wantPath: "/v3/tokens/token-key",
		},
		{
			name:     "a response with only an id",
			token:    "token-id",
			body:     `{"id":"token-id","authProvider":"local"}`,
			want:     TokenMetadata{Name: "token-id", Kind: "session", Provider: "local"},
			wantPath: "/v3/tokens/token-id",
		},
		{
			name:     "a name with a question mark",
			token:    "a?b",
			body:     `{"id":"a?b","name":"a?b"}`,
			want:     TokenMetadata{Name: "a?b", Kind: "session"},
			wantPath: "/v3/tokens/a%3Fb",
		},
		{
			name:     "a name with a slash",
			token:    "a/b",
			body:     `{"id":"a/b","name":"a/b"}`,
			want:     TokenMetadata{Name: "a/b", Kind: "session"},
			wantPath: "/v3/tokens/a%2Fb",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			srv := newTokenServer(t, http.StatusOK, constantBody(row.body))
			eff := srv.effective()
			got, err := V3TokenMetadata(t.Context(), eff, row.token)
			require.NoError(t, err)
			assert.Equal(t, row.want, got)

			seen := srv.requests()
			require.Len(t, seen, 1)
			assert.Equal(t, http.MethodGet, seen[0].method)
			assert.Equal(t, row.wantPath, seen[0].path)
			assert.Empty(t, seen[0].query)
			assert.Equal(t, "Bearer "+eff.Token, seen[0].authorization)
			assert.Equal(t, httpx.UserAgent(), seen[0].userAgent)
			assert.False(t, strings.HasPrefix(seen[0].path, "/k8s/"), "the read does not go through a cluster proxy")
		})
	}
}

// TestV3TokenMetadataClassifiesTheResponse asserts each kind of response to the
// v3 token read maps to one error code with the status readable, and keeps the
// server's message.
func TestV3TokenMetadataClassifiesTheResponse(t *testing.T) {
	t.Parallel()
	// overLimit is larger than the limit the read puts on a body.
	const overLimit = 2 << 20
	pad := strings.Repeat("a", overLimit)
	rows := []struct {
		name     string
		status   int
		body     string
		code     string
		contains string
		notFound bool
		retry    bool
		httpCode int
	}{
		{name: "403 with a message", status: http.StatusForbidden, code: clierr.CodeRBAC, httpCode: http.StatusForbidden,
			body:     `{"type":"error","links":{},"code":"PermissionDenied","message":"can not get token","status":403}`,
			contains: "can not get token"},
		{name: "404", status: http.StatusNotFound, code: clierr.CodeNotFound, httpCode: http.StatusNotFound, notFound: true,
			body: `{"type":"error","code":"NotFound","message":"token-x not found","status":404}`},
		{name: "401 from the authentication filter", status: http.StatusUnauthorized, code: clierr.CodeAuth, httpCode: http.StatusUnauthorized,
			body: `{"type":"error","status":"401","message":"Unauthorized 401: must authenticate"}`},
		{name: "500", status: http.StatusInternalServerError, code: clierr.CodeGeneric, httpCode: http.StatusInternalServerError, retry: true,
			body: `{"type":"error","code":"ServerError","message":"boom","status":500}`, contains: "boom"},
		{name: "a 200 that is not JSON", status: http.StatusOK, code: clierr.CodeGeneric, body: `not json`, contains: "is not a JSON object with a name or id"},
		{name: "a 200 JSON array", status: http.StatusOK, code: clierr.CodeGeneric, body: `[]`, contains: "is not a JSON object with a name or id"},
		{name: "a 200 with neither name nor id", status: http.StatusOK, code: clierr.CodeGeneric,
			body: `{"type":"token","authProvider":"local"}`, contains: "is not a JSON object with a name or id"},
		{name: "a 200 over the limit", status: http.StatusOK, code: clierr.CodeGeneric,
			body: `{"name":"token-x","pad":"` + pad + `"}`, contains: "is larger than 1 MiB"},
		{name: "a 403 over the limit", status: http.StatusForbidden, code: clierr.CodeRBAC, httpCode: http.StatusForbidden,
			body: `{"type":"error","code":"PermissionDenied","message":"can not get token","status":403,"pad":"` + pad + `"}`},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			srv := newTokenServer(t, row.status, constantBody(row.body))
			_, err := V3TokenMetadata(t.Context(), srv.effective(), "token-x")
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, row.code, ce.Code, ce.Message)
			assert.Equal(t, row.retry, ce.Retryable, ce.Message)
			assert.Equal(t, row.notFound, clierr.IsNotFound(err))
			if row.contains != "" {
				require.ErrorContains(t, err, row.contains)
			}
			if row.contains == "is not a JSON object with a name or id" || row.contains == "is larger than 1 MiB" {
				require.ErrorContains(t, err, "/v3/tokens/token-x")
			}
			if row.httpCode != 0 {
				status, ok := clierr.HTTPStatus(err)
				assert.True(t, ok, "the status is readable")
				assert.Equal(t, row.httpCode, status)
			}
		})
	}
}

// TestV3TokenMetadataVerifiesTheServerWithTheEntryCA asserts the read trusts
// the certificate the entry's CA signs.
func TestV3TokenMetadataVerifiesTheServerWithTheEntryCA(t *testing.T) {
	t.Parallel()
	srv := newTokenServer(t, http.StatusOK, constantBody(`{"id":"token-x","name":"token-x","authProvider":"local"}`))
	got, err := V3TokenMetadata(t.Context(), srv.effective(), "token-x")
	require.NoError(t, err)
	assert.Equal(t, "token-x", got.Name)
}
