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
// a typed error classified by the status table - a rejected token AUTH, a
// refused review RBAC, a cluster Rancher does not know NOT_FOUND, each
// worded with the --cluster remedy through a cluster other than local, a
// server failure GENERIC and retryable, an unreadable body or one naming no
// user GENERIC; no answer is GENERIC and retryable.
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
		{"rejected", http.StatusUnauthorized, ``, "", clierr.CodeAuth, false, "posting a SelfSubjectReview: the server rejected the credential (401)"},
		{"refused", http.StatusForbidden, ``, "", clierr.CodeRBAC, false,
			"posting a SelfSubjectReview: the server refused the request (403) and gave no reason; Rancher answers 403 for a cluster the user cannot read and, to a user who cannot read every cluster, for one it does not know: check the --cluster value 'c-1/x'"},
		{"not found", http.StatusNotFound, ``, "", clierr.CodeNotFound, false,
			"posting a SelfSubjectReview: the server could not find the requested resource; Rancher answers 404 for a cluster it does not know: check the --cluster value 'c-1/x'"},
		{"server failure", http.StatusBadGateway, ``, "", clierr.CodeGeneric, true,
			`posting a SelfSubjectReview: an error on the server ("Bad Gateway") has prevented the request from succeeding - its agent may not be registered yet; retry once the cluster is active`},
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
	t.Run("through the local cluster", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			code   string
			msg    string
		}{
			{http.StatusNotFound, clierr.CodeGeneric, "posting a SelfSubjectReview: the server could not find the requested resource; check the server URL"},
			{http.StatusForbidden, clierr.CodeRBAC, "posting a SelfSubjectReview: the server refused the request (403) and gave no reason"},
		} {
			mu.Lock()
			status, body, requests = tc.status, ``, nil
			mu.Unlock()
			_, err := requestOwner(t.Context(), srv.Client(), srv.URL, "local", "tok:secret")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.code, ce.Code, tc.status)
			assert.False(t, ce.Retryable, tc.status)
			assert.Equal(t, tc.msg, ce.Message, tc.status)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		_, err := requestOwner(t.Context(), dead.Client(), url, "c-1", "tok:secret")
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
// that revokes it by kind (for a v3 token, the administrator's delete), or
// not identified at all.
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
			"the token could not be revoked; an administrator can delete v3/token-v3 with: rancher delete management-token token-v3 --apply --yes --allow-deprecated, or it expires"},
		{"not identified", nil, RevokeOutcome{Result: ResultNotAttempted}, "the token could not be identified; it was not revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, clierr.ExpandRemedies(revokedClause("the token", tc.ref, tc.out), ""))
		})
	}
}
