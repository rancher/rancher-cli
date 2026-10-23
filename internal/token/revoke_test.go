package token

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRevokeTokenChoosesPathByKind asserts the two delete paths and the 404
// reading: ext names go to the ext store, v3 names to the v3 path, and a
// missing token is alreadyGone, not a failure.
func TestRevokeTokenChoosesPathByKind(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{SeedToken: "seed:s"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "seed:s")
	assert.Equal(t, ResultAlreadyGone, out.Result)
	recs := f.DeleteRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, testrancher.DeleteRecord{Kind: "v3", Name: "token-1", Bearer: "seed:s"}, recs[0])
	out = revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "ext-1"}, "seed:s")
	assert.Equal(t, ResultAlreadyGone, out.Result)
	assert.Equal(t, "ext", f.DeleteRequests()[1].Kind)
}

// TestRevokeTokenFailedCarriesStatus asserts the failed shape.
func TestRevokeTokenFailedCarriesStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(srv.Close)
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "ext-1"}, "b")
	assert.Equal(t, ResultFailed, out.Result)
	assert.Contains(t, out.Error, "500")
}

// TestCleanupContextIsDetachedAndBounded asserts the two properties every
// revoke relies on: a cancelled operation context does not cancel it, and
// it ends within the bound.
func TestCleanupContextIsDetachedAndBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		cancel()
		ctx, done := cleanupContext(parent)
		defer done()
		require.NoError(t, ctx.Err(), "detached from the cancelled operation context")
		time.Sleep(cleanupTimeout + time.Second)
		synctest.Wait()
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	})
}

// TestUserSuppliedRevokeIsNotApplicable asserts that a pasted token is never
// revoked and no request is sent.
func TestUserSuppliedRevokeIsNotApplicable(t *testing.T) {
	t.Parallel()
	c := UserSupplied("id:secret")
	assert.Equal(t, ProvenanceUserSupplied, c.Provenance)
	assert.Equal(t, ResultNotApplicable, c.Revoke().Result)
}

// TestRevokeHasNoContextParameter asserts the API shape: the exported revoke
// of the credential's own token cannot receive the cancelled operation
// context, and the credential exports no revoke beyond it and
// RevokeCreated, which takes a context only to shorten its bound and can
// never be cancelled by it.
func TestRevokeHasNoContextParameter(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[LoginCredential]()
	m, ok := typ.MethodByName("Revoke")
	require.True(t, ok)
	assert.Equal(t, 1, m.Type.NumIn(), "receiver only")
	rc, ok := typ.MethodByName("RevokeCreated")
	require.True(t, ok)
	require.Equal(t, 3, rc.Type.NumIn(), "receiver, context, ref")
	assert.Equal(t, reflect.TypeFor[context.Context](), rc.Type.In(1))
	assert.Equal(t, 2, typ.NumMethod(), "Revoke and RevokeCreated are the only exported methods")
}

// TestNoExportedRevokeFunctionBesidesTheMethod asserts that the package
// exports no free function whose name contains Revoke: the safe, bounded,
// detached cleanup call is the method alone.
func TestNoExportedRevokeFunctionBesidesTheMethod(t *testing.T) {
	t.Parallel()
	fset := gotoken.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, perr)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if fn.Name.IsExported() && strings.Contains(fn.Name.Name, "Revoke") {
				t.Errorf("%s: exported revoke function %s", fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
}

// TestRevokeCreatedUsesTheCredentialAsBearerAndBoundsItself asserts the
// new revoke deletes the named token with the credential as bearer on the
// kind's path, detached from the caller's cancellation, bounded by a still
// running caller deadline or else by the cleanup bound (an expired
// deadline never cancels the cleanup); a userSupplied credential is
// notApplicable.
func TestRevokeCreatedUsesTheCredentialAsBearerAndBoundsItself(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var gotPath, gotAuth string
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "slow-") {
				// Records nothing: this handler never answers, so a later
				// request's write to gotPath would race it. It aborts the
				// connection rather than returning, so the caller can never
				// decode an empty 200 that raced its own deadline.
				testtransport.HangAndAbort(r)
			}
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}))
		cred := LoginCredential{Token: "ext/d:s", Provenance: ProvenanceDerived, client: srv.Client(), base: srv.URL, parent: t.Context()}

		out := cred.RevokeCreated(t.Context(), TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-x"})
		assert.Equal(t, ResultRevoked, out.Result)
		assert.Equal(t, V3TokensPath+"/kubeconfig-u-x", gotPath)
		assert.Equal(t, "Bearer ext/d:s", gotAuth)

		// A cancelled caller context does not cancel the revoke: it completes.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		out = cred.RevokeCreated(ctx, TokenRef{Kind: TokenKindExt, Name: "token-c"})
		assert.Equal(t, ResultRevoked, out.Result)
		assert.Equal(t, ExtTokensPath+"/token-c", gotPath)

		// A deadline that already passed (kubectl gave up) is the reason for
		// the cleanup, never a reason to skip it: the DELETE still runs.
		ectx, ecancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer ecancel()
		out = cred.RevokeCreated(ectx, TokenRef{Kind: TokenKindExt, Name: "token-e"})
		assert.Equal(t, ResultRevoked, out.Result)
		assert.Equal(t, ExtTokensPath+"/token-e", gotPath)

		// A caller deadline shorter than the cleanup bound wins while it is
		// still running. Last, because its handler never answers.
		dctx, dcancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer dcancel()
		start := time.Now()
		out = cred.RevokeCreated(dctx, TokenRef{Kind: TokenKindExt, Name: "slow-1"})
		assert.Equal(t, ResultFailed, out.Result)
		assert.Equal(t, 150*time.Millisecond, time.Since(start), "the caller's deadline, not the cleanup bound, ended the wait on the fake clock")

		assert.Equal(t, ResultNotApplicable, UserSupplied("x").RevokeCreated(t.Context(), TokenRef{Kind: TokenKindExt, Name: "n"}).Result)
	})
}

// TestRevokeOutcomeCarriesTheStatus asserts that a revoke reports the HTTP
// status the server answered, 0 when no answer came, and that the status
// never reaches JSON.
func TestRevokeOutcomeCarriesTheStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "t"}, "ext/t:s")
			assert.Equal(t, status, out.Status)
			raw, err := json.Marshal(out)
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &decoded))
			assert.NotContains(t, decoded, "status")
		})
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindExt, Name: "t"}, "ext/t:s")
	assert.Equal(t, 0, out.Status)
	assert.Equal(t, ResultFailed, out.Result)
}

// logoutRecorder is a server that records every request and answers a
// logout and a DELETE with the given statuses.
type logoutRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method, path, query, auth, body string
}

func (l *logoutRecorder) handler(logoutStatus, deleteStatus int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.requests = append(l.requests, recordedRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(body)})
		l.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == LogoutPath {
			w.WriteHeader(logoutStatus)
			return
		}
		w.WriteHeader(deleteStatus)
	})
}

func (l *logoutRecorder) seen() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.requests)
}

func (l *logoutRecorder) logouts() []recordedRequest {
	return slices.DeleteFunc(l.seen(), func(r recordedRequest) bool { return r.path != LogoutPath })
}

func (l *logoutRecorder) deletes() []recordedRequest {
	return slices.DeleteFunc(l.seen(), func(r recordedRequest) bool { return r.method != http.MethodDelete })
}

// TestRevokeTokenV3OwnBearerUsesLogout asserts a v3 token deleted with its
// own bearer takes one POST to the logout endpoint with no query string,
// no body and that bearer, and no DELETE when logout answers 200 or 204.
func TestRevokeTokenV3OwnBearerUsesLogout(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			rec := &logoutRecorder{}
			srv := httptest.NewServer(rec.handler(status, http.StatusForbidden))
			defer srv.Close()
			out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:secret")
			assert.Equal(t, ResultRevoked, out.Result)
			assert.Equal(t, status, out.Status)
			assert.Empty(t, out.Error)
			got := rec.seen()
			require.Len(t, got, 1, "no DELETE follows a logout that answered")
			assert.Equal(t, recordedRequest{method: http.MethodPost, path: LogoutPath, auth: "Bearer token-1:secret"}, got[0], "no query string, no body")
		})
	}
}

// TestRevokeTokenNeverSendsLogoutWithAnotherBearer asserts logout, which
// deletes the bearer's own token, is never sent unless the bearer names the
// v3 token being deleted: another token's bearer, an ext bearer, a
// malformed bearer, and a name that is no object name all take the DELETE
// route only.
func TestRevokeTokenNeverSendsLogoutWithAnotherBearer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		ref    TokenRef
		bearer string
	}{
		{"another v3 token", TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-2:secret"},
		{"an ext bearer for a v3 ref", TokenRef{Kind: TokenKindV3, Name: "token-1"}, "ext/token-1:secret"},
		{"no colon", TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1"},
		{"empty secret", TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:"},
		{"two colons", TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:a:b"},
		{"empty name", TokenRef{Kind: TokenKindV3, Name: "token-1"}, ":secret"},
		{"empty bearer", TokenRef{Kind: TokenKindV3, Name: "token-1"}, ""},
		{"a ref name that is no object name", TokenRef{Kind: TokenKindV3, Name: "Token_1"}, "Token_1:secret"},
		{"an ext ref with a v3-shaped bearer", TokenRef{Kind: TokenKindExt, Name: "token-1"}, "token-1:secret"},
		{"an ext ref with its own bearer", TokenRef{Kind: TokenKindExt, Name: "token-1"}, "ext/token-1:secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &logoutRecorder{}
			srv := httptest.NewServer(rec.handler(http.StatusOK, http.StatusOK))
			defer srv.Close()
			out := revokeToken(t.Context(), srv.Client(), srv.URL, tc.ref, tc.bearer)
			assert.Equal(t, ResultRevoked, out.Result)
			assert.Empty(t, rec.logouts(), "logout deletes the bearer's token, so it needs the target's own bearer")
			require.Len(t, rec.deletes(), 1)
			wantPath := V3TokensPath + "/" + tc.ref.Name
			if tc.ref.Kind == TokenKindExt {
				wantPath = ExtTokensPath + "/" + tc.ref.Name
			}
			assert.Equal(t, wantPath, rec.deletes()[0].path)
			assert.Equal(t, strings.TrimSpace("Bearer "+tc.bearer), rec.deletes()[0].auth)
		})
	}
}

// TestRevokeTokenLogoutFallsBackToDelete asserts any logout answer other
// than 200 or 204 is followed by exactly one DELETE with the same token and
// bearer, whose answer decides the outcome, and that the error of a failed
// outcome names both answers. Logout is never retried.
func TestRevokeTokenLogoutFallsBackToDelete(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		logoutStatus, delStatus int
		want                    RevokeResult
		wantError               string
	}{
		{"logout 401, delete 200", http.StatusUnauthorized, http.StatusOK, ResultRevoked, ""},
		{"logout 404, delete 204", http.StatusNotFound, http.StatusNoContent, ResultRevoked, ""},
		{"logout 500, delete 404", http.StatusInternalServerError, http.StatusNotFound, ResultAlreadyGone, ""},
		{"logout 500, delete 403", http.StatusInternalServerError, http.StatusForbidden, ResultFailed, "logout 500; delete 403 Forbidden"},
		{"logout 401, delete 401", http.StatusUnauthorized, http.StatusUnauthorized, ResultFailed, "logout 401; delete 401 Unauthorized"},
		{"logout 404, delete 500", http.StatusNotFound, http.StatusInternalServerError, ResultFailed, "logout 404; delete 500 Internal Server Error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &logoutRecorder{}
			srv := httptest.NewServer(rec.handler(tc.logoutStatus, tc.delStatus))
			defer srv.Close()
			out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:secret")
			assert.Equal(t, tc.want, out.Result)
			assert.Equal(t, tc.delStatus, out.Status, "the DELETE's answer")
			assert.Equal(t, tc.wantError, out.Error)
			got := rec.seen()
			require.Len(t, got, 2, "one logout, one DELETE, no retry")
			assert.Equal(t, http.MethodPost, got[0].method)
			assert.Equal(t, LogoutPath, got[0].path)
			assert.Empty(t, got[0].query)
			assert.Empty(t, got[0].body)
			assert.Equal(t, http.MethodDelete, got[1].method)
			assert.Equal(t, V3TokensPath+"/token-1", got[1].path)
			assert.Equal(t, got[0].auth, got[1].auth, "the same bearer")
		})
	}
}

// TestRevokeTokenLogoutTransportErrorFallsBack asserts a logout that gets
// no answer is followed by the DELETE, and that the error names the
// transport error and the DELETE's answer.
func TestRevokeTokenLogoutTransportErrorFallsBack(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == LogoutPath {
			panic(http.ErrAbortHandler)
		}
		deletes.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:secret")
	assert.Equal(t, ResultFailed, out.Result)
	assert.True(t, strings.HasPrefix(out.Error, "logout "), out.Error)
	assert.Contains(t, out.Error, "; delete 403 Forbidden")
	assert.Equal(t, int32(1), deletes.Load())
}

// TestRevokeTokenLogoutAnswerWithUnreadableBodyIsRevoked asserts a 200 from
// logout is revoked even when its body cannot be read, with no DELETE.
func TestRevokeTokenLogoutAnswerWithUnreadableBodyIsRevoked(t *testing.T) {
	t.Parallel()
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	out := revokeToken(t.Context(), srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:secret")
	assert.Equal(t, ResultRevoked, out.Result)
	assert.Equal(t, http.StatusOK, out.Status)
	assert.Empty(t, out.Error)
	assert.Zero(t, deletes.Load())
}

// TestRevokeTokenLogoutAndFallbackShareOneBound asserts both requests run
// on the caller's one context: a logout that never answers ends at the
// context's deadline and the fallback has no time left.
func TestRevokeTokenLogoutAndFallbackShareOneBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			testtransport.HangAndAbort(r)
		}))
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		out := revokeToken(ctx, srv.Client(), srv.URL, TokenRef{Kind: TokenKindV3, Name: "token-1"}, "token-1:secret")
		assert.Equal(t, ResultFailed, out.Result)
		assert.Equal(t, 150*time.Millisecond, time.Since(start))
		assert.Equal(t, int32(1), requests.Load(), "the fallback could not send after the bound passed")
		assert.Contains(t, out.Error, "; delete ")
	})
}
