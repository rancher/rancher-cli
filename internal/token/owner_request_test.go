package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRequestOwnerMapsEachAnswer asserts the owner request after a
// sign-in: one POST of a SelfSubjectReview through the cluster's path with
// the token as bearer, whose username is the owner; each other answer is
// a typed error - a rejected token AUTH, a refused review RBAC, a server
// failure or no answer GENERIC and retryable, any other status, an
// unreadable body or one naming no user GENERIC.
func TestRequestOwnerMapsEachAnswer(t *testing.T) {
	var (
		mu       sync.Mutex
		status   int
		body     string
		requests []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		want      string
		code      string
		retryable bool
		msg       string
	}{
		{"the owner", http.StatusCreated, `{"status":{"userInfo":{"username":"u-abc12"}}}`, "u-abc12", "", false, ""},
		{"no user", http.StatusCreated, `{"status":{"userInfo":{}}}`, "", clierr.CodeGeneric, false, "the SelfSubjectReview response named no user"},
		{"unreadable", http.StatusCreated, `not json`, "", clierr.CodeGeneric, false, "the SelfSubjectReview response is not readable"},
		{"rejected", http.StatusUnauthorized, ``, "", clierr.CodeAuth, false, "the server rejected the token: 401 Unauthorized"},
		{"refused", http.StatusForbidden, ``, "", clierr.CodeRBAC, false, "the server refused the SelfSubjectReview: 403 Forbidden"},
		{"not found", http.StatusNotFound, ``, "", clierr.CodeGeneric, false, "posting a SelfSubjectReview: 404 Not Found"},
		{"server failure", http.StatusBadGateway, ``, "", clierr.CodeGeneric, true, "posting a SelfSubjectReview: 502 Bad Gateway"},
		{"an answer past the bound", http.StatusCreated, `{"status":{"userInfo":{"username":"u-abc12"}},"padding":"` + strings.Repeat("x", maxOwnerBody) + `"}`, "", clierr.CodeGeneric, false,
			fmt.Sprintf("the SelfSubjectReview response is larger than %d bytes", maxOwnerBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			status, body, requests = tc.status, tc.body, nil
			mu.Unlock()
			got, err := requestOwner(t.Context(), srv.Client(), srv.URL, "c-1/x", "tok:secret")
			mu.Lock()
			assert.Equal(t, []string{"POST /k8s/clusters/c-1%2Fx/apis/authentication.k8s.io/v1/selfsubjectreviews Bearer tok:secret"}, requests,
				"one review through the cluster's own path, the cluster id escaped")
			mu.Unlock()
			if tc.code == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Equal(t, tc.msg, ce.Message)
		})
	}
	t.Run("no answer", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		_, err := requestOwner(t.Context(), http.DefaultClient, url, "c-1", "tok:secret")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
	})
	t.Run("the call cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := requestOwner(ctx, srv.Client(), srv.URL, "c-1", "tok:secret")
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestRevokedClauseNamesTheFate asserts what a refused token's error says
// became of it: revoked, already gone, or not revoked with the command
// that revokes it by kind, or not identified at all.
func TestRevokedClauseNamesTheFate(t *testing.T) {
	t.Parallel()
	ext := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	v3 := &TokenRef{Kind: TokenKindV3, Name: "token-v3"}
	for _, tc := range []struct {
		name string
		ref  *TokenRef
		out  RevokeOutcome
		want string
	}{
		{"revoked", ext, RevokeOutcome{Result: ResultRevoked}, "the token was revoked"},
		{"already gone", ext, RevokeOutcome{Result: ResultAlreadyGone}, "the token was revoked"},
		{"failed, ext", ext, RevokeOutcome{Result: ResultFailed, Error: "500 Internal Server Error"},
			"the token could not be revoked (500 Internal Server Error); revoke it with: rancher delete token token-abc --apply --yes"},
		{"failed, v3, no cause", v3, RevokeOutcome{Result: ResultFailed},
			"the token could not be revoked; revoke it with: rancher delete management-token token-v3 --apply --yes --allow-deprecated"},
		{"not identified", nil, RevokeOutcome{Result: ResultNotAttempted}, "the token could not be identified, so it was not revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, clierr.ExpandRemedies(revokedClause("the token", tc.ref, tc.out), ""))
		})
	}
}
