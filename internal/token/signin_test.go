package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/rancher/rancher-cli/internal/ui"
)

// loggedRequest is one request the fixture Rancher received, with the body
// of a POST.
type loggedRequest struct{ method, path, bearer, body string }

// kind says what the request's bearer is: none, an ext token's (the
// "ext/" prefix) or a v3 token's.
func (r loggedRequest) kind() string {
	switch {
	case r.bearer == "":
		return "none"
	case strings.HasPrefix(r.bearer, "ext/"):
		return "ext"
	}
	return "v3"
}

// requestLog records every request the facade handles.
type requestLog struct {
	mu   sync.Mutex
	reqs []loggedRequest
}

// hook is a RequestHook that records the request and then runs next.
func (l *requestLog) hook(next func(r *http.Request)) func(r *http.Request) {
	return func(r *http.Request) {
		var body []byte
		if r.Method == http.MethodPost && r.Body != nil {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		l.mu.Lock()
		l.reqs = append(l.reqs, loggedRequest{r.Method, r.URL.Path, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), string(body)})
		l.mu.Unlock()
		if next != nil {
			next(r)
		}
	}
}

func (l *requestLog) all() []loggedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]loggedRequest(nil), l.reqs...)
}

// sequence is each request as "METHOD path bearerKind".
func (l *requestLog) sequence() []string {
	var out []string
	for _, r := range l.all() {
		out = append(out, r.method+" "+r.path+" "+r.kind())
	}
	return out
}

// find is the requests with a method and a path.
func (l *requestLog) find(method, path string) []loggedRequest {
	var out []loggedRequest
	for _, r := range l.all() {
		if r.method == method && r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// signInAgainst runs the sign-in for a cluster token against a fresh facade
// with the e2e user typing the right credentials.
func signInAgainst(t *testing.T, opts testrancher.Options, cluster string) (*testrancher.Facade, *requestLog, clusterSignIn, error) {
	t.Helper()
	return signInWith(t, t.Context(), opts, cluster, localPrompter())
}

func signInWith(t *testing.T, ctx context.Context, opts testrancher.Options, cluster string, p runtime.Prompter) (*testrancher.Facade, *requestLog, clusterSignIn, error) {
	t.Helper()
	log := &requestLog{}
	opts.RequestHook = log.hook(opts.RequestHook)
	f, srv := newTokenFacade(t, opts)
	input := &loginInput{caFlag: "--cacert", server: srv.URL, authProvider: "localProvider"}
	si, err := signInForClusterToken(ctx, srv.Client(), input, cluster, p)
	return f, log, si, err
}

// failureOf is the typed error and its login failure details.
func failureOf(t *testing.T, err error) (*clierr.Error, clierr.LoginFailureDetails) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a typed error, got %v", err)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok, "want the login failure details, got %T", ce.Details)
	return ce, d
}

// sessionCleanup is what became of the login token, failing the test (not
// panicking) when the error carries no such fate.
func sessionCleanup(t *testing.T, d clierr.LoginFailureDetails) clierr.RevokeResult {
	t.Helper()
	require.NotNil(t, d.SessionToken, "the error carries the login token's fate")
	require.NotNil(t, d.SessionToken.Cleanup)
	return d.SessionToken.Cleanup.Result
}

// clusterCleanup is what became of the cluster token, failing the test when
// the error carries no such fate.
func clusterCleanup(t *testing.T, d clierr.LoginFailureDetails) clierr.RevokeResult {
	t.Helper()
	require.NotNil(t, d.ClusterToken, "the error carries the cluster token's fate")
	require.NotNil(t, d.ClusterToken.Cleanup)
	return d.ClusterToken.Cleanup.Result
}

// blockUntilGone cancels the caller and then holds the request until the
// client has gone, so the answer cannot race the cancellation.
func blockUntilGone(cancel context.CancelFunc) func(r *http.Request) {
	return func(r *http.Request) {
		cancel()
		testtransport.WaitUntilGone(r)
	}
}

// TestSignInForClusterTokenRequestSequence asserts the sign-in's requests
// and the bearer of each: the login (json, carrying the operation marker),
// the cluster lookup and the ext create with the login token as bearer (no
// ttl, the marker as description), then the login token's own logout.
func TestSignInForClusterTokenRequestSequence(t *testing.T) {
	t.Parallel()
	f, log, si, err := signInAgainst(t, testrancher.Options{}, "c-test")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"GET /v1-public/authproviders none",
		"POST /v1-public/login none",
		"GET " + testrancher.ClustersCollectionPath + " v3",
		"POST " + ExtTokensPath + " v3",
		"POST /v1/logout v3",
	}, log.sequence())

	login := log.find(http.MethodPost, "/v1-public/login")[0]
	var loginBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(login.body), &loginBody))
	assert.Equal(t, "json", loginBody["responseType"])
	assert.Equal(t, TokenDescription(hostname(), si.Marker), loginBody["description"])

	create := log.find(http.MethodPost, ExtTokensPath)[0]
	var createBody struct {
		Spec map[string]any `json:"spec"`
	}
	require.NoError(t, json.Unmarshal([]byte(create.body), &createBody))
	assert.Equal(t, "c-test", createBody.Spec["clusterName"])
	assert.Equal(t, TokenDescription(hostname(), si.Marker), createBody.Spec["description"])
	assert.NotContains(t, createBody.Spec, "ttl", "the server default applies")

	logout := log.find(http.MethodPost, LogoutPath)[0]
	assert.Equal(t, login.bearer, "", "the login request has no bearer")
	assert.Equal(t, create.bearer, logout.bearer, "the create and the logout both use the login token's bearer")
	assert.Equal(t, 1, f.TokenCount(), "only the cluster token remains")
}

// TestSignInForClusterTokenResult asserts what a successful sign-in returns:
// an ext cluster token whose create response names its owner, and the login
// token's fate - a marked v3 session token, revoked by its own logout.
func TestSignInForClusterTokenResult(t *testing.T) {
	t.Parallel()
	f, _, si, err := signInAgainst(t, testrancher.Options{}, "c-test")
	require.NoError(t, err)
	require.NotNil(t, si.Cluster.Ref)
	assert.Equal(t, TokenKindExt, si.Cluster.Ref.Kind)
	assert.Equal(t, "c-test", si.Cluster.ClusterName)
	assert.Equal(t, "user-e2e", si.Cluster.UserID)
	assert.NotEmpty(t, si.Cluster.Bearer)
	require.NotNil(t, si.Login)
	assert.True(t, si.Login.Marked)
	require.NotNil(t, si.Login.Ref)
	assert.Equal(t, TokenKindV3, si.Login.Ref.Kind)
	assert.Equal(t, ResultRevoked, si.Login.Cleanup.Result)
	assert.Empty(t, si.warning())
	deletes := f.DeleteRequests()
	require.Len(t, deletes, 1)
	assert.Equal(t, testrancher.RouteLogout, deletes[0].Route)
	assert.Equal(t, si.Login.Ref.Name, deletes[0].Name)
	creates := f.CreateRequests()
	require.Len(t, creates, 2)
	assert.Equal(t, creates[0].Description, creates[1].Description, "both tokens carry the same marker")
	assert.Equal(t, "cluster", creates[1].Role)
}

// TestSignInForClusterTokenOnARancherWithoutExtTokens asserts the Norman
// create: the ext create answers 404 and a cluster-scoped v3 token is
// created with the login token's bearer; the login token is still deleted.
func TestSignInForClusterTokenOnARancherWithoutExtTokens(t *testing.T) {
	t.Parallel()
	f, log, si, err := signInAgainst(t, testrancher.Options{Legacy: true, V3TokenCreate: true}, "c-test")
	require.NoError(t, err)
	assert.Equal(t, TokenKindV3, si.Cluster.Ref.Kind)
	assert.Equal(t, []string{
		"GET /v1-public/authproviders none",
		"POST /v1-public/login none",
		"GET " + testrancher.ClustersCollectionPath + " v3",
		"POST " + ExtTokensPath + " v3",
		"POST /v3/tokens v3",
		"POST /v1/logout v3",
	}, log.sequence())
	assert.Equal(t, ResultRevoked, si.Login.Cleanup.Result)
	assert.Len(t, f.DeleteRequests(), 1)
}

// TestSignInForClusterTokenForLocalLooksNothingUp asserts the cluster
// "local" is not looked up: no cluster read is made.
func TestSignInForClusterTokenForLocalLooksNothingUp(t *testing.T) {
	t.Parallel()
	_, log, si, err := signInAgainst(t, testrancher.Options{}, "local")
	require.NoError(t, err)
	assert.Equal(t, "local", si.Cluster.ClusterName)
	assert.Empty(t, log.find(http.MethodGet, testrancher.ClustersCollectionPath))
}

// TestSignInLoginFailsWithNoLoginTokenReturnsTheLoginError asserts a wrong
// password creates nothing and returns the login's own error, not a login
// failure with token fates, so the no-terminal and sign-in remedies see the
// error they always saw.
func TestSignInLoginFailsWithNoLoginTokenReturnsTheLoginError(t *testing.T) {
	t.Parallel()
	f, log, _, err := signInWith(t, t.Context(), testrancher.Options{}, "c-test",
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "wrong"), false, true))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Nil(t, ce.Details)
	_, isAcquire := errors.AsType[*AcquireError](err)
	assert.False(t, isAcquire)
	assert.Empty(t, f.DeleteRequests())
	assert.Empty(t, log.find(http.MethodPost, ExtTokensPath))
}

// TestSignInNoTerminalRefusesBeforeAnyRequestMakesAToken asserts a sign-in
// with no terminal is refused by the login's own error, before any token
// exists, so withNoTerminalRemedies still rewrites it.
func TestSignInNoTerminalRefusesBeforeAnyRequestMakesAToken(t *testing.T) {
	t.Parallel()
	f, _, _, err := signInWith(t, t.Context(), testrancher.Options{}, "c-test", ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false))
	_, ok := errors.AsType[*needsTerminal](err)
	require.True(t, ok, "got %v", err)
	assert.Zero(t, f.TokenCount())
	assert.Zero(t, f.LoginCount())
}

// TestSignInOwnedLoginTokenIsRevokedOnAnUnexpectedLoginStatus asserts a
// login the server answers with a 2xx other than 201 still carries a token
// the CLI can name: it is revoked and the error reports it.
func TestSignInOwnedLoginTokenIsRevokedOnAnUnexpectedLoginStatus(t *testing.T) {
	t.Parallel()
	f, _, _, err := signInAgainst(t, testrancher.Options{LoginStatus: http.StatusOK}, "c-test")
	_, d := failureOf(t, err)
	assert.Equal(t, clierr.StageAuthentication, d.Stage)
	require.NotNil(t, d.SessionToken)
	require.NotNil(t, d.SessionToken.Ref)
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
	assert.Nil(t, d.ClusterToken)
	assert.Len(t, f.DeleteRequests(), 1)
	assert.Equal(t, "run the kubectl command again", d.Recovery)
}

// TestSignInLostLoginResponseNamesTheMarker asserts a login whose answer
// never arrived leaves a token the CLI cannot name: the error carries it
// as marked, of unknown kind, and the recovery names both lists with the
// person who runs each.
func TestSignInLostLoginResponseNamesTheMarker(t *testing.T) {
	t.Parallel()
	f, _, _, err := signInAgainst(t, testrancher.Options{DropLoginResponse: true}, "c-test")
	_, d := failureOf(t, err)
	assert.Equal(t, clierr.StageAuthentication, d.Stage)
	require.NotNil(t, d.SessionToken)
	assert.Equal(t, CreateUnknown, d.SessionToken.Outcome)
	assert.True(t, d.SessionToken.Marked)
	assert.Nil(t, d.SessionToken.Ref)
	require.NotEmpty(t, d.OperationMarker)
	recovery := clierr.ExpandRemedies(d.Recovery, "")
	assert.Contains(t, recovery, `the session token: the user who signed in can list tokens from a directory signed in as that user; list them with: rancher get tokens; then delete the one whose description carries "[op:`+d.OperationMarker+`]"`)
	assert.Contains(t, recovery, `an administrator can list tokens with: rancher get management-tokens; then delete the one whose description carries "[op:`+d.OperationMarker+`]"`)
	assert.True(t, strings.HasSuffix(recovery, "; run the kubectl command again"), recovery)
	assert.Empty(t, f.DeleteRequests(), "nothing can be named, so nothing is deleted")
}

// TestSignInCancelledDuringTheLoginKeepsTheMarker asserts Ctrl-C while the
// login POST is in flight: the login fails as cancelled, the token the
// server may have created is reported as marked, nothing is deleted.
func TestSignInCancelledDuringTheLoginKeepsTheMarker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := testrancher.Options{RequestHook: func(r *http.Request) {
		if r.URL.Path == "/v1-public/login" {
			blockUntilGone(cancel)(r)
		}
	}}
	f, _, _, err := signInWith(t, ctx, opts, "c-test", localPrompter())
	require.ErrorIs(t, err, context.Canceled)
	_, d := failureOf(t, err)
	require.NotNil(t, d.SessionToken)
	assert.Equal(t, CreateUnknown, d.SessionToken.Outcome)
	assert.True(t, d.SessionToken.Marked)
	assert.NotEmpty(t, d.OperationMarker)
	assert.Empty(t, f.DeleteRequests())
}

// TestSignInClusterLookupFailureDeletesTheLoginToken asserts a cluster that
// is not among the user's clusters fails the sign-in with the lookup's own
// NOT_FOUND, deletes the login token with its own bearer, carries its fate
// and offers no hint.
func TestSignInClusterLookupFailureDeletesTheLoginToken(t *testing.T) {
	t.Parallel()
	f, log, _, err := signInAgainst(t, testrancher.Options{}, "nope")
	ce, d := failureOf(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "the clusters you can read: 'c-test' (testcluster)")
	assert.Empty(t, ce.Hint)
	assert.Equal(t, clierr.StageClusterToken, d.Stage)
	require.NotNil(t, d.SessionToken)
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
	assert.Nil(t, d.ClusterToken)
	assert.Equal(t, correctCluster, d.Recovery, "signing in again would name the same cluster")
	assert.Empty(t, log.find(http.MethodPost, ExtTokensPath), "no cluster token is created")
	assert.Equal(t, []string{"POST /v1/logout v3"}, log.sequence()[len(log.sequence())-1:])
	assert.Len(t, f.DeleteRequests(), 1)
}

// visibleClusters is a cluster list that shows the user a cluster the ext store
// refuses (c-denied) and one it does not know (c-gone), beside c-test.
func visibleClusters(bearer string) ([]testrancher.ClusterFixture, bool) {
	return []testrancher.ClusterFixture{{ID: "c-test", DisplayName: "testcluster"}, {ID: "c-denied"}, {ID: "c-gone"}}, bearer != ""
}

// clusterAccess is the fake's ExtCreateClusterAccess for one cluster that
// does not exist and one the user may not access.
func clusterAccess(_, cluster string) (exists, allowed bool) {
	return cluster != "c-gone", cluster != "c-denied"
}

// TestSignInClusterTokenCreateRefused asserts Rancher's refusals of the ext
// create, answered as its ext store does: 403 for a cluster the user may not
// access and 400 for one that does not exist are worded as Rancher's refusal
// for that cluster, with the person who can change it as the recovery, and a
// 401 keeps the token wording. The login token is deleted and the cluster
// token is reported as not created in every case.
func TestSignInClusterTokenCreateRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		opts     testrancher.Options
		cluster  string
		code     string
		text     string
		recovery string
	}{
		{"no access", testrancher.Options{ExtCreateClusterAccess: clusterAccess, ClusterVisibility: visibleClusters}, "c-denied", clierr.CodeRBAC,
			"Rancher refused to create a token for cluster 'c-denied' for this user: tokens.ext.cattle.io is forbidden: user user-e2e is not allowed to access cluster c-denied",
			"an administrator can grant the user access to cluster 'c-denied', or the user can correct --cluster in the kubeconfig exec entry"},
		{"no such cluster", testrancher.Options{ExtCreateClusterAccess: clusterAccess, ClusterVisibility: visibleClusters}, "c-gone", clierr.CodeValidation,
			"Rancher refused to create a token for cluster 'c-gone' for this user: cluster c-gone not found",
			"the user can correct --cluster in the kubeconfig exec entry"},
		{"a 403 with no message", testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, "c-test", clierr.CodeRBAC,
			"Rancher refused to create a token for cluster 'c-test' for this user: ",
			"an administrator can grant the user access to cluster 'c-test', or the user can correct --cluster in the kubeconfig exec entry"},
		{"rejected", testrancher.Options{ScopedCreateStatus: http.StatusUnauthorized}, "c-test", clierr.CodeAuth,
			"the server rejected the token this sign-in just created", signInAgain},
		{"Norman 403", testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden}, "c-test", clierr.CodeRBAC,
			"the token this sign-in just created cannot create tokens",
			"an administrator can grant the user access to cluster 'c-test', or the user can correct --cluster in the kubeconfig exec entry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _, _, err := signInAgainst(t, tc.opts, tc.cluster)
			ce, d := failureOf(t, err)
			assert.Equal(t, tc.code, ce.Code)
			assert.Contains(t, ce.Message, tc.text)
			assert.NotRegexp(t, `:\s*$`, ce.Message, "the refusal ends with Rancher's message, never an empty one")
			assert.NotContains(t, ce.Message, "login credential")
			assert.Equal(t, tc.recovery, d.Recovery)
			assert.Equal(t, clierr.StageClusterToken, d.Stage)
			assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
			require.NotNil(t, d.ClusterToken)
			assert.Equal(t, CreateNotCreated, d.ClusterToken.Outcome)
			assert.Len(t, f.DeleteRequests(), 1, "only the login token is deleted")
		})
	}
}

// TestSignInRefusalsWordNoCommandAndNeverGoUnexplained asserts the 403 and
// 400 texts name who acts and no rancher command, so no remedy marker is
// needed in them.
func TestSignInRefusalsWordNoCommandAndNeverGoUnexplained(t *testing.T) {
	t.Parallel()
	for _, text := range []string{correctCluster, grantCluster} {
		assert.False(t, clierr.HasRemedy(text), text)
		assert.NotContains(t, text, "rancher ")
	}
}

// TestSignInRecoveryFitsEveryFailureRow asserts the recovery of each failed
// sign-in is never empty and is the text for its row: signing in again where
// that can help (a lost or failed answer, a cancellation, a 401, a scope
// mismatch), the person who can change the cluster where it cannot, and the
// supported Rancher version for a server with no token create.
func TestSignInRecoveryFitsEveryFailureRow(t *testing.T) {
	t.Parallel()
	other := "c-other"
	cancelOn := func(match func(r *http.Request) bool) func(cancel context.CancelFunc) testrancher.Options {
		return func(cancel context.CancelFunc) testrancher.Options {
			return testrancher.Options{RequestHook: func(r *http.Request) {
				if match(r) {
					blockUntilGone(cancel)(r)
				}
			}}
		}
	}
	for _, tc := range []struct {
		name    string
		opts    testrancher.Options
		cancel  func(cancel context.CancelFunc) testrancher.Options
		cluster string
		// want is the whole recovery; suffix, when set, is what it ends with
		// after the text for a token left behind.
		want, suffix string
	}{
		{name: "a login answered 200", opts: testrancher.Options{LoginStatus: http.StatusOK}, cluster: "c-test", want: signInAgain},
		{name: "a lost login response", opts: testrancher.Options{DropLoginResponse: true}, cluster: "c-test", suffix: "; " + signInAgain},
		{name: "a cancelled login", cancel: cancelOn(func(r *http.Request) bool { return r.URL.Path == "/v1-public/login" }), cluster: "c-test", suffix: "; " + signInAgain},
		{name: "a cluster that is not found", cluster: "nope", want: correctCluster},
		{name: "a cancelled lookup", cancel: cancelOn(func(r *http.Request) bool { return r.URL.Path == testrancher.ClustersCollectionPath }), cluster: "c-test", want: signInAgain},
		{name: "a create answered 500", opts: testrancher.Options{ScopedCreateStatus: http.StatusInternalServerError}, cluster: "c-test", suffix: "; " + signInAgain},
		{name: "a lost create response", opts: testrancher.Options{DropDerivedCreateResponse: true}, cluster: "c-test", suffix: "; " + signInAgain},
		{name: "a cancelled create", cancel: func(cancel context.CancelFunc) testrancher.Options {
			return testrancher.Options{DerivedCreateHook: blockUntilGone(cancel)}
		}, cluster: "c-test", suffix: "; " + signInAgain},
		{name: "a create answered 401", opts: testrancher.Options{ScopedCreateStatus: http.StatusUnauthorized}, cluster: "c-test", want: signInAgain},
		{name: "an ext create answered 403", opts: testrancher.Options{ExtCreateClusterAccess: clusterAccess, ClusterVisibility: visibleClusters}, cluster: "c-denied",
			want: "an administrator can grant the user access to cluster 'c-denied', or " + correctCluster},
		{name: "an ext create answered 400", opts: testrancher.Options{ExtCreateClusterAccess: clusterAccess, ClusterVisibility: visibleClusters}, cluster: "c-gone", want: correctCluster},
		{name: "a Norman create answered 403", opts: testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusForbidden}, cluster: "c-test",
			want: "an administrator can grant the user access to cluster 'c-test', or " + correctCluster},
		{name: "a Norman create answered 400", opts: testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateStatus: http.StatusBadRequest}, cluster: "c-test", want: correctCluster},
		{name: "a scope mismatch", opts: testrancher.Options{ScopedCreateClusterName: &other}, cluster: "c-test", want: signInAgain},
		{name: "a server with no token create", opts: testrancher.Options{Legacy: true}, cluster: "c-test", want: "this command needs Rancher " + config.MinRancherVersion + " or later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := tc.opts
			if tc.cancel != nil {
				opts = tc.cancel(cancel)
			}
			_, _, _, err := signInWith(t, ctx, opts, tc.cluster, localPrompter())
			_, d := failureOf(t, err)
			require.NotEmpty(t, d.Recovery)
			if tc.want != "" {
				assert.Equal(t, tc.want, d.Recovery)
			}
			if tc.suffix != "" {
				assert.True(t, strings.HasSuffix(d.Recovery, tc.suffix), d.Recovery)
			}
		})
	}
}

// TestSignInNoExtAndNoV3CreateIsWordedForTheSignIn asserts the error for a
// Rancher that serves neither create names no token reuse.
func TestSignInNoExtAndNoV3CreateIsWordedForTheSignIn(t *testing.T) {
	t.Parallel()
	_, _, _, err := signInAgainst(t, testrancher.Options{Legacy: true}, "c-test")
	ce, d := failureOf(t, err)
	assert.Contains(t, ce.Message, "; the sign-in cannot create a cluster token")
	assert.NotContains(t, ce.Message, "token reuse")
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
}

// TestSignInScopeCheckFailureDeletesBothTokensInOrder asserts a create
// whose response names another cluster fails closed: the cluster token is
// deleted first with the login token's bearer, then the login token, and
// both fates are reported.
func TestSignInScopeCheckFailureDeletesBothTokensInOrder(t *testing.T) {
	t.Parallel()
	other := "c-other"
	f, _, _, err := signInAgainst(t, testrancher.Options{ScopedCreateClusterName: &other}, "c-test")
	require.ErrorIs(t, err, errClusterTokenUnscoped)
	_, d := failureOf(t, err)
	assert.Equal(t, clierr.StageClusterToken, d.Stage)
	require.NotNil(t, d.ClusterToken)
	require.NotNil(t, d.ClusterToken.Ref)
	assert.Equal(t, ResultRevoked, clusterCleanup(t, d))
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
	deletes := f.DeleteRequests()
	require.Len(t, deletes, 2)
	assert.Equal(t, d.ClusterToken.Ref.Name, deletes[0].Name, "the cluster token goes first")
	assert.Empty(t, deletes[0].Route, "by its name, on the ext path")
	assert.Equal(t, testrancher.RouteLogout, deletes[1].Route)
	assert.Equal(t, d.SessionToken.Ref.Name, deletes[1].Name)
	assert.Equal(t, deletes[1].Bearer, deletes[0].Bearer, "the cluster token is deleted with the login token's bearer")
}

// TestSignInNormanClusterTokenThatCannotBeDeletedIsReported asserts that on
// a Rancher without ext tokens a refused scope check leaves a cluster-scoped
// v3 token a standard user cannot delete: its failed delete is reported with
// the administrator's remedy, and the login token is still deleted.
func TestSignInNormanClusterTokenThatCannotBeDeletedIsReported(t *testing.T) {
	t.Parallel()
	other := "c-other"
	_, _, _, err := signInAgainst(t, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateClusterID: &other, V3DeleteForbiddenForNonAdmin: true}, "c-test")
	_, d := failureOf(t, err)
	require.NotNil(t, d.ClusterToken)
	assert.Equal(t, TokenKindV3, d.ClusterToken.Ref.Kind)
	assert.Equal(t, ResultFailed, clusterCleanup(t, d))
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
	recovery := clierr.ExpandRemedies(d.Recovery, "")
	assert.Contains(t, recovery, "the cluster token "+d.ClusterToken.Ref.String()+": "+d.ClusterToken.Ref.String()+" expires at ")
	assert.Contains(t, recovery, "; an administrator can delete it sooner with: rancher delete management-token '"+d.ClusterToken.Ref.Name+"' --apply --yes --allow-deprecated")
	assert.True(t, strings.HasSuffix(recovery, "; run the kubectl command again"), recovery)
}

// TestSignInLostClusterCreateResponseReportsTheMarkedToken asserts a create
// whose answer never arrived: the login token is deleted and the cluster
// token is reported as marked of ext kind, with the owner's command named.
func TestSignInLostClusterCreateResponseReportsTheMarkedToken(t *testing.T) {
	t.Parallel()
	f, _, _, err := signInAgainst(t, testrancher.Options{DropDerivedCreateResponse: true}, "c-test")
	_, d := failureOf(t, err)
	require.NotNil(t, d.ClusterToken)
	assert.Equal(t, CreateUnknown, d.ClusterToken.Outcome)
	assert.True(t, d.ClusterToken.Marked)
	assert.Equal(t, TokenKindExt, d.ClusterToken.Kind)
	assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
	assert.Len(t, f.DeleteRequests(), 1, "the login token only: the cluster token has no name")
	recovery := clierr.ExpandRemedies(d.Recovery, "")
	assert.Contains(t, recovery, "the cluster token: the user who signed in can list tokens from a directory signed in as that user; list them with: rancher get tokens; then delete the one whose description carries")
	assert.NotContains(t, recovery, "management-tokens", "an ext token needs no administrator")
}

// TestSignInCancelledAtEachStepDeletesTheLoginToken asserts Ctrl-C after the
// login: while the clusters are read, while the cluster token is created,
// and while the login token itself is being deleted. The cleanup runs on its
// own bound, so the login token is deleted in every case; the first two
// fail the sign-in and report it, the last one has nothing left to cancel.
func TestSignInCancelledAtEachStepDeletesTheLoginToken(t *testing.T) {
	t.Parallel()
	t.Run("during the cluster lookup", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opts := testrancher.Options{RequestHook: func(r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == testrancher.ClustersCollectionPath {
				blockUntilGone(cancel)(r)
			}
		}}
		f, _, _, err := signInWith(t, ctx, opts, "c-test", localPrompter())
		require.ErrorContains(t, err, "context canceled")
		_, d := failureOf(t, err)
		assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
		assert.Nil(t, d.ClusterToken)
		assert.Len(t, f.DeleteRequests(), 1)
	})
	t.Run("during the cluster token create", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opts := testrancher.Options{DerivedCreateHook: func(r *http.Request) {
			blockUntilGone(cancel)(r)
		}}
		f, _, _, err := signInWith(t, ctx, opts, "c-test", localPrompter())
		require.ErrorIs(t, err, context.Canceled)
		_, d := failureOf(t, err)
		assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
		require.NotNil(t, d.ClusterToken)
		assert.Equal(t, CreateUnknown, d.ClusterToken.Outcome, "the server may have created it")
		assert.True(t, d.ClusterToken.Marked)
		assert.Len(t, f.DeleteRequests(), 1)
	})
	t.Run("during the login token's delete", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opts := testrancher.Options{RequestHook: func(r *http.Request) {
			if r.URL.Path == LogoutPath {
				cancel()
			}
		}}
		f, _, si, err := signInWith(t, ctx, opts, "c-test", localPrompter())
		require.NoError(t, err)
		assert.Equal(t, ResultRevoked, si.Login.Cleanup.Result)
		assert.Len(t, f.DeleteRequests(), 1)
		assert.NotEmpty(t, si.Cluster.Bearer, "the cluster token is returned; later steps see the cancellation")
	})
}

// TestSignInSharesOneCleanupBoundBetweenTheTwoDeletes asserts the cluster
// token's delete and the login token's delete run under one bound: a first
// delete that uses most of it leaves the second too little, and the failure
// reports the login token as not deleted instead of waiting a second bound.
func TestSignInSharesOneCleanupBoundBetweenTheTwoDeletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		other := "c-other"
		f, rt := newInMemoryTokenFacade(t, testrancher.Options{ScopedCreateClusterName: &other, RequestHook: func(r *http.Request) {
			switch {
			case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/tokens/"):
				time.Sleep(cleanupTimeout - time.Second)
			case r.URL.Path == LogoutPath:
				time.Sleep(5 * time.Second)
			}
		}})
		start := time.Now()
		input := &loginInput{caFlag: "--cacert", server: rancherOrigin, authProvider: "localProvider"}
		_, err := signInForClusterToken(t.Context(), rt.client(), input, "c-test", localPrompter())
		_, d := failureOf(t, err)
		assert.Equal(t, ResultRevoked, clusterCleanup(t, d))
		assert.Equal(t, ResultFailed, sessionCleanup(t, d), "the login token's delete ran out of the shared bound")
		assert.Equal(t, cleanupTimeout, time.Since(start), "one bound, not one per delete")
		assert.Len(t, f.DeleteRequests(), 1, "the logout was attempted, and answered after the client gave up")
	})
}

// TestSignInFailedLoginTokenDeleteIsAWarningNotAFailure asserts forced
// single logout: the logout answers 500 and a standard user's delete is
// refused, the sign-in still succeeds, and the login token's fate and the
// warning say who can delete it.
func TestSignInFailedLoginTokenDeleteIsAWarningNotAFailure(t *testing.T) {
	t.Parallel()
	_, _, si, err := signInAgainst(t, testrancher.Options{ForcedSingleLogout: true, V3DeleteForbiddenForNonAdmin: true}, "c-test")
	require.NoError(t, err)
	assert.NotEmpty(t, si.Cluster.Bearer)
	require.NotNil(t, si.Login)
	assert.Equal(t, ResultFailed, si.Login.Cleanup.Result)
	assert.Equal(t, "logout 500; delete 403 Forbidden", si.Login.Cleanup.Error)
	w := clierr.ExpandRemedies(si.warning(), "")
	want := "the transient session token " + si.Login.Ref.String() + " created by this login could not be revoked (logout 500; delete 403 Forbidden); " +
		si.Login.Ref.String() + " expires at " + si.Login.ExpiresAt + "; an administrator can delete it sooner with: rancher delete management-token '" + si.Login.Ref.Name + "' --apply --yes --allow-deprecated"
	assert.Equal(t, want, w)
}

// TestSignInOverSAMLUsesTheKubeconfigTokenAsTheLoginToken asserts the SAML
// sign-in: the sign-in URL has responseType=kubeconfig, the only value the
// dashboard sign-in page accepts, and the kubeconfig token the browser sign-in
// creates is the login token (unmarked), it creates the cluster token with
// its bearer and is deleted by its own logout. With forced single logout its
// failed delete is worded as a kubeconfig token.
func TestSignInOverSAMLUsesTheKubeconfigTokenAsTheLoginToken(t *testing.T) {
	run := func(t *testing.T, opts testrancher.Options, cluster string) (*testrancher.Facade, *requestLog, clusterSignIn, string, error) {
		t.Helper()
		log := &requestLog{}
		opts.RequestHook = log.hook(opts.RequestHook)
		opts.EnableSAMLAuthTokens = true
		opts.ExtraProviders = []string{"oktaProvider"}
		f := testrancher.New(opts)
		srv := httptest.NewTestServer(t, f)
		rt := hostTransport{hostOf(rancherOrigin): srv.Client().Transport}
		p := &urlRecorder{handoffPrompter: newHandoffPrompter(t, func(id, key string) { f.DepositAuthToken(id, key) })}
		input := &loginInput{caFlag: "--cacert", server: rancherOrigin, authProvider: "oktaProvider"}
		si, err := signInForClusterToken(t.Context(), rt.client(), input, cluster, p)
		return f, log, si, p.loginURL(), err
	}
	t.Run("success", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, log, si, loginURL, err := run(t, testrancher.Options{}, "local")
			require.NoError(t, err)
			assert.Contains(t, loginURL, "responseType=kubeconfig")
			require.NotNil(t, si.Login)
			assert.False(t, si.Login.Marked, "the SAML login token carries no marker")
			require.NotNil(t, si.Login.Search, "the browser handoff narrows it")
			assert.Equal(t, ResultRevoked, si.Login.Cleanup.Result)
			assert.Equal(t, TokenKindExt, si.Cluster.Ref.Kind)
			assert.Zero(t, f.LoginCount(), "no login POST")
			create := log.find(http.MethodPost, ExtTokensPath)
			require.Len(t, create, 1)
			logout := log.find(http.MethodPost, LogoutPath)
			require.Len(t, logout, 1)
			assert.Equal(t, create[0].bearer, logout[0].bearer, "the login token's bearer creates the cluster token and logs itself out")
		})
	})
	t.Run("a login token that cannot be deleted is a kubeconfig token", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, si, _, err := run(t, testrancher.Options{ForcedSingleLogout: true, V3DeleteForbiddenForNonAdmin: true}, "local")
			require.NoError(t, err)
			w := si.warning()
			assert.True(t, strings.HasPrefix(w, "the transient kubeconfig token "+si.Login.Ref.String()+" created by this login could not be revoked"), w)
		})
	})
	t.Run("a failed lookup names no marker", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, _, _, err := run(t, testrancher.Options{ClusterVisibility: visibleClusters}, "nope")
			_, d := failureOf(t, err)
			assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
			assert.Nil(t, d.ClusterToken, "no cluster create was attempted")
			assert.Empty(t, d.OperationMarker, "the unmarked login token and no marked create leave nothing the marker finds")
			assert.NotNil(t, d.SessionToken.Search)
			assert.Len(t, f.DeleteRequests(), 1)
		})
	})
	t.Run("a failure leaves the handoff facts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			_, _, _, _, err := run(t, testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, "local")
			_, d := failureOf(t, err)
			require.NotNil(t, d.SessionToken)
			assert.Equal(t, ResultRevoked, sessionCleanup(t, d))
			assert.NotNil(t, d.SessionToken.Search, "the unmarked login token keeps the handoff facts")
		})
	})
}

// TestSignInLeftoverNamesWhoRunsEachCommand asserts the text for a token
// the sign-in could not delete: a command comes with the person who runs it.
func TestSignInLeftoverNamesWhoRunsEachCommand(t *testing.T) {
	t.Parallel()
	ext := clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}, ExpiresAt: "2026-10-12T12:00:00Z"}
	v3 := clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}, ExpiresAt: "2026-10-12T12:00:00Z"}
	search := clierr.UnmarkedTokenSearch{AuthProvider: "keycloakSAML", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:15:00Z"}
	for _, tc := range []struct {
		name   string
		fate   clierr.TokenFate
		userID string
		want   string
	}{
		{"v3 with a name", v3, "u-1",
			"v3/token-v3 expires at 2026-10-12T12:00:00Z; an administrator can delete it sooner with: rancher delete management-token 'token-v3' --apply --yes --allow-deprecated"},
		{"ext with a known user", ext, "u-1",
			"user 'u-1' can delete it from a directory signed in as user 'u-1'; run: rancher delete token 'token-abc' --apply --yes; or it expires at 2026-10-12T12:00:00Z"},
		{"ext with an unknown user", ext, "",
			"the user who signed in can delete it from a directory signed in as that user; run: rancher delete token 'token-abc' --apply --yes; or it expires at 2026-10-12T12:00:00Z"},
		{"ext with no expiry", clierr.TokenFate{Ref: ext.Ref}, "u-1",
			"user 'u-1' can delete it from a directory signed in as user 'u-1'; run: rancher delete token 'token-abc' --apply --yes"},
		{"marked ext with no name", clierr.TokenFate{Marked: true, Kind: clierr.TokenKindExt}, "u-1",
			`user 'u-1' can list tokens from a directory signed in as user 'u-1'; list them with: rancher get tokens; then delete the one whose description carries "[op:m]"`},
		{"marked v3 with no name", clierr.TokenFate{Marked: true, Kind: clierr.TokenKindV3}, "u-1",
			`an administrator can list tokens with: rancher get management-tokens; then delete the one whose description carries "[op:m]"`},
		{"marked, kind unknown", clierr.TokenFate{Marked: true}, "",
			`the user who signed in can list tokens from a directory signed in as that user; list them with: rancher get tokens; then delete the one whose description carries "[op:m]"; ` +
				`an administrator can list tokens with: rancher get management-tokens; then delete the one whose description carries "[op:m]"`},
		{"the SAML handoff with no token", clierr.TokenFate{Search: &search}, "",
			"a kubeconfig token may have been created through keycloakSAML at or after 2026-10-12T10:00:00Z; the CLI stopped waiting at 2026-10-12T10:15:00Z, " +
				"but the browser sign-in can still complete after that, and the token may be newer; these facts narrow the candidates but cannot identify one token; " +
				"an administrator can inspect them with: rancher get management-tokens -o json; the user who signed in can find them in the UI under Account & API Keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, clierr.ExpandRemedies(signInLeftover(tc.fate, tc.userID, "m"), ""))
		})
	}
}

// TestSignInRevokedClauseSaysWhatBecameOfTheToken asserts the three ways
// the clause ends: revoked, unnamed, or not revoked with the leftover text.
func TestSignInRevokedClauseSaysWhatBecameOfTheToken(t *testing.T) {
	t.Parallel()
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	assert.Equal(t, "the token was revoked", signInRevokedClause("the token", ref, RevokeOutcome{Result: ResultRevoked}, "x"))
	assert.Equal(t, "the token was revoked", signInRevokedClause("the token", ref, RevokeOutcome{Result: ResultAlreadyGone}, "x"))
	assert.Equal(t, "the token could not be identified; it was not revoked", signInRevokedClause("the token", nil, RevokeOutcome{Result: ResultNotAttempted}, "x"))
	assert.Equal(t, "the token could not be revoked (500 Internal Server Error); leftover text",
		signInRevokedClause("the token", ref, RevokeOutcome{Result: ResultFailed, Error: "500 Internal Server Error"}, "leftover text"))
}

// urlRecorder is the browser stand-in that also keeps every notice, so a
// test reads the login URL the sign-in published.
type urlRecorder struct {
	*handoffPrompter
	notices []string
}

func (u *urlRecorder) Notice(msg string) {
	u.notices = append(u.notices, msg)
	u.handoffPrompter.Notice(msg)
}

// loginURL is the notice that carries the dashboard login URL.
func (u *urlRecorder) loginURL() string {
	for _, n := range u.notices {
		if strings.Contains(n, "/dashboard/auth/login?") {
			return n
		}
	}
	return ""
}

// TestSignInSendsTheClusterLookupWarningToTheNotices asserts a --cluster value
// that is one cluster's id and another's display name creates the token for
// the id and the warning naming the other goes to the notices.
func TestSignInSendsTheClusterLookupWarningToTheNotices(t *testing.T) {
	t.Parallel()
	var notices bytes.Buffer
	p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
	clusters := func(bearer string) ([]testrancher.ClusterFixture, bool) {
		return []testrancher.ClusterFixture{{ID: "c-1", DisplayName: "prod"}, {ID: "c-2", DisplayName: "c-1"}}, bearer != ""
	}
	_, _, si, err := signInWith(t, t.Context(), testrancher.Options{ClusterVisibility: clusters}, "c-1", p)
	require.NoError(t, err)
	assert.Equal(t, "c-1", si.Cluster.ClusterName, "the id wins")
	assert.Contains(t, notices.String(), "--cluster 'c-1' is the id of cluster 'c-1' and the display name of cluster 'c-2'; the command uses the one with that id. To use the other one, pass --cluster 'c-2'")
}

// TestWithSignInFateKeepsTheWarningsItIsGiven asserts the error built after the
// login token's delete carries the warnings the call had collected, and none
// when it had none.
func TestWithSignInFateKeepsTheWarningsItIsGiven(t *testing.T) {
	t.Parallel()
	si := clusterSignIn{Login: &clierr.TokenFate{Outcome: CreateKnown, Ref: &TokenRef{Kind: TokenKindV3, Name: "t"},
		Cleanup: &RevokeOutcome{Result: ResultRevoked}}}
	err := si.withSignInFate(clierr.New(clierr.CodeGeneric, "cancelled"), []string{"first", "second"})
	ce, _ := failureOf(t, err)
	assert.Equal(t, []string{"first", "second"}, ce.Warnings())
	ce, _ = failureOf(t, si.withSignInFate(clierr.New(clierr.CodeGeneric, "cancelled"), nil))
	assert.Empty(t, ce.Warnings())
}

// TestSignInErrorFateCarriesTheClusterTokensExpiry asserts an error after the
// login token's delete states the cluster token's expiry in the fate and in
// the recovery text, which the cancelled wait's own details leave out, and
// that the other producers' wait details are not changed by it.
func TestSignInErrorFateCarriesTheClusterTokensExpiry(t *testing.T) {
	t.Parallel()
	fresh := &clierr.TokenFate{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-abc"},
		Cleanup: &RevokeOutcome{Result: ResultFailed, Error: "500"}}
	si := clusterSignIn{Cluster: CreateResult{UserID: "u-1", ExpiresAt: "2026-10-12T12:00:00Z"}, Login: &clierr.TokenFate{Outcome: CreateKnown, Ref: &TokenRef{Kind: TokenKindV3, Name: "t"},
		Cleanup: &RevokeOutcome{Result: ResultRevoked}}}
	wait := clierr.New(clierr.CodeGeneric, "cancelled").WithDetails(directWaitDetails{Fate: fresh})
	_, d := failureOf(t, si.withSignInFate(wait, nil))
	require.NotNil(t, d.ClusterToken)
	assert.Equal(t, "2026-10-12T12:00:00Z", d.ClusterToken.ExpiresAt)
	assert.Contains(t, clierr.ExpandRemedies(d.Recovery, ""), "user 'u-1' can delete it from a directory signed in as user 'u-1'; run: rancher delete token 'token-abc' --apply --yes; or it expires at 2026-10-12T12:00:00Z")
	assert.Empty(t, fresh.ExpiresAt, "the fate the wait carried is copied, not changed")
}
