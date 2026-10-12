package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestOwnerPostsOneSelfSubjectReview asserts auth login's owner request:
// one POST of a SelfSubjectReview on the local cluster's path with the
// token as bearer, whose username is the owner; a rejected token is AUTH,
// an answer that names no user and a server failure are GENERIC; a 5xx is
// retryable unless its body is a Kubernetes Status, which the API server
// itself sent.
func TestOwnerPostsOneSelfSubjectReview(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		want      string
		code      string
		retryable bool
	}{
		{"the owner", http.StatusCreated, `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-abc12"}}}`, "u-abc12", "", false},
		{"no user", http.StatusCreated, `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{}}}`, "", clierr.CodeGeneric, false},
		{"rejected", http.StatusUnauthorized, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`, "", clierr.CodeAuth, false},
		{"server failure with a Status", http.StatusInternalServerError, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","code":500}`, "", clierr.CodeGeneric, false},
		{"server failure", http.StatusInternalServerError, `{"type":"error","code":"ServerError","status":500}`, "", clierr.CodeGeneric, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				mu       sync.Mutex
				requests []string
			)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+body["kind"].(string))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			got, err := Owner(t.Context(), config.Effective{URL: srv.URL, CA: serverCA(srv), Token: "id:secret"})
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []string{"POST /k8s/clusters/local/apis/authentication.k8s.io/v1/selfsubjectreviews Bearer id:secret SelfSubjectReview"}, requests)
			if tc.code == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
		})
	}
}

// TestOwnerAndPreflightMapARESTConfigFailureAlike asserts auth login's
// owner request and auth status's preflight report a connection they
// cannot build the same way: a typed refusal passes through as it is, and
// any other failure (a CA file that cannot be read) is AUTH with the
// entry's login as the hint.
func TestOwnerAndPreflightMapARESTConfigFailureAlike(t *testing.T) {
	t.Parallel()
	missing := config.Effective{URL: "https://r.example.com", Token: "id:secret", ServerName: "prod", CA: filepath.Join(t.TempDir(), "absent.pem")}
	for name, call := range map[string]func(config.Effective) error{
		"owner":     func(eff config.Effective) error { _, err := Owner(t.Context(), eff); return err },
		"preflight": func(eff config.Effective) error { _, err := Preflight(t.Context(), eff); return err },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce, ok := errors.AsType[*clierr.Error](call(missing))
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Equal(t, "rancher auth login 'prod'", clierr.ExpandRemedies(ce.Hint, ""))
			ce, ok = errors.AsType[*clierr.Error](call(config.Effective{}))
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code, "a typed refusal passes through")
		})
	}
}
