package token

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoginErrorClassifiesEachAnswer asserts the sign-in POST's answers:
// 401 and 403 are AUTH with no hint (no credential is stored yet), a 404
// is NOT_FOUND, a 5xx that is not a Status is retryable whatever its body,
// and the status stays readable.
func TestLoginErrorClassifiesEachAnswer(t *testing.T) {
	t.Parallel()
	rancher500 := `{"code":"ServerError","status":500,"type":"error","baseType":"error"}`
	for _, tc := range []struct {
		status    int
		body      string
		code      string
		message   string
		retryable bool
	}{
		{http.StatusUnauthorized, `{"code":"Unauthorized","message":"authentication failed","status":401,"type":"error"}`, clierr.CodeAuth, "login rejected: 401 Unauthorized", false},
		{http.StatusForbidden, `{"code":"PermissionDenied","status":403,"type":"error","baseType":"error"}`, clierr.CodeAuth, "login rejected: 403 Forbidden", false},
		{http.StatusNotFound, `{"code":"NotFound","status":404,"type":"error","baseType":"error"}`, clierr.CodeNotFound, "login failed: the server could not find the requested resource", false},
		{http.StatusInternalServerError, rancher500, clierr.CodeGeneric, `login failed: an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`, true},
		{http.StatusBadGateway, "upstream connect error", clierr.CodeGeneric, `login failed: an error on the server ("upstream connect error") has prevented the request from succeeding - retrying may succeed`, true},
	} {
		err := loginError(&http.Response{StatusCode: tc.status}, []byte(tc.body), nil)
		ce := clierr.From(err)
		require.NotNil(t, ce, tc.status)
		assert.Equal(t, tc.code, ce.Code, tc.status)
		assert.Equal(t, tc.message, ce.Message, tc.status)
		assert.Equal(t, tc.retryable, ce.Retryable, tc.status)
		assert.Empty(t, ce.Hint, tc.status)
		status, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, tc.status)
		assert.Equal(t, tc.status, status)
	}
}

// TestGetAuthProvidersNotFoundNamesTheServerURL asserts a 404 on the
// provider list, an API and no object, is GENERIC naming the server URL.
func TestGetAuthProvidersNotFoundNamesTheServerURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	_, _, err := getAuthProviders(t.Context(), srv.Client(), srv.URL)
	ce := clierr.From(err)
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, "error listing auth providers: the server could not find the requested resource; check the server URL", ce.Message)
	status, ok := clierr.HTTPStatus(err)
	assert.True(t, ok)
	assert.Equal(t, http.StatusNotFound, status)
}

// TestSAMLPollRefusalEndsAuth asserts a SAML poll answered 401 ends AUTH
// with the server's rejection worded by the status table, through the
// projection the login failure carries as its primary. Not parallel: it
// shortens the package's SAML timing.
func TestSAMLPollRefusalEndsAuth(t *testing.T) {
	restore := SetSAMLTimingForTest(time.Minute, time.Millisecond)
	t.Cleanup(restore)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	_, err := samlAuth(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL}, ui.Discard())
	require.Error(t, err)
	ce, pf := primaryOf(err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, clierr.CodeAuth, pf.Code)
	assert.Equal(t, "error fetching auth token: the server answered 401; the request carried no credential", pf.Message)
	assert.False(t, pf.Retryable)
	assert.Empty(t, pf.Hint, "the poll sends no credential, so no hint")
	status, ok := clierr.HTTPStatus(err)
	assert.True(t, ok)
	assert.Equal(t, http.StatusUnauthorized, status)
}

// lockedBuffer is a notice sink the SAML test reads from the server's
// goroutine while samlAuth writes to it from the test's.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var samlLoginURLRE = regexp.MustCompile(`Login to Rancher Server at (\S+)`)

// TestSAMLDeleteRefusalNoticeIsWorded asserts a refused delete of the
// deposited auth token is reported through the status table's wording and
// does not fail the login: the bearer was already decrypted. Not parallel:
// it shortens the package's SAML timing.
func TestSAMLDeleteRefusalNoticeIsWorded(t *testing.T) {
	restore := SetSAMLTimingForTest(time.Minute, time.Millisecond)
	t.Cleanup(restore)
	const bearer = "token-abcde:secret"
	var notices lockedBuffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		m := samlLoginURLRE.FindStringSubmatch(notices.String())
		if m == nil {
			http.Error(w, "login url not published yet", http.StatusInternalServerError)
			return
		}
		_, publicKey, err := testrancher.ParseCLISignInURL(m[1])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(publicKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var key rsa.PublicKey
		if err := json.Unmarshal(raw, &key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sealed, err := rsa.EncryptOAEP(crypto.SHA256.New(), rand.Reader, &key, []byte(bearer), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": base64.StdEncoding.EncodeToString(sealed)})
	}))
	t.Cleanup(srv.Close)

	res, err := samlAuth(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL}, ui.New(&notices, nil, true, false))
	require.NoError(t, err)
	assert.Equal(t, bearer, res.Create.Bearer)
	assert.Contains(t, notices.String(), "error deleting auth token: the server refused the request (403) and gave no reason")
}

// TestGetAuthProvidersRefusalIsClassified asserts a refused provider list
// is AUTH for a 401, worded as a request that carried no credential and
// with no hint, and RBAC for a 403.
func TestGetAuthProvidersRefusalIsClassified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status  int
		code    string
		message string
		hint    string
	}{
		{http.StatusUnauthorized, clierr.CodeAuth, "error listing auth providers: the server answered 401; the request carried no credential", ""},
		{http.StatusForbidden, clierr.CodeRBAC, "error listing auth providers: the server refused the request (403) and gave no reason", ""},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			_, _, err := getAuthProviders(t.Context(), srv.Client(), srv.URL)
			ce := clierr.From(err)
			require.NotNil(t, ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.message, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.False(t, ce.Retryable)
			status, ok := clierr.HTTPStatus(err)
			assert.True(t, ok)
			assert.Equal(t, tc.status, status)
		})
	}
}

// TestAcquireDerivedCreateRefusedEndsRBAC asserts a derived token create
// answered 403 ends the login RBAC with the credential's permission named,
// in the error and in the primary failure the auth login envelope carries
// as details.primary. The envelope itself is rendered in
// internal/commands/auth, which asserts the same code there.
func TestAcquireDerivedCreateRefusedEndsRBAC(t *testing.T) {
	t.Parallel()
	_, _, _, err := acquireAgainst(t, testrancher.Options{DerivedCreateStatus: http.StatusForbidden})
	ae, ok := errors.AsType[*AcquireError](err)
	require.True(t, ok, "got %v", err)
	require.ErrorIs(t, ae.Err, errCredentialForbidden)
	ce, pf := primaryOf(ae.Err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, clierr.CodeRBAC, pf.Code)
	assert.Equal(t, "credential lacks permission to create tokens: the server refused the request (403) and gave no reason", pf.Message)
	assert.False(t, pf.Retryable)
	status, ok := clierr.HTTPStatus(ae.Err)
	assert.True(t, ok)
	assert.Equal(t, http.StatusForbidden, status)

	failure, details := failureOf(t, FailAcquire(ae, "prod"))
	assert.Equal(t, clierr.CodeRBAC, failure.Code)
	assert.Contains(t, details.Recovery, "create on tokens.ext.cattle.io")
	assert.Contains(t, details.Recovery, "--global --role user --dry-run")
	assert.Contains(t, details.Recovery, "rancher get users")
	assert.Contains(t, details.Recovery, "log in again with: ")
	assert.Empty(t, failure.Hint)
}

// TestClassifyRejectionKeepsItsSentinels asserts a 401 and a 403 on the
// stored credential's create still reach the outcomes the sentinels select:
// a plugin-created login is pluginCredentialRejected, any other stored
// login is errStoredCredentialRejected, and a 403 is an RBAC error that
// words the refusal through the status table.
func TestClassifyRejectionKeepsItsSentinels(t *testing.T) {
	t.Parallel()
	create := func(t *testing.T, status int) error {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		_, err := CreateDerivedToken(t.Context(), srv.Client(), srv.URL, "tok:secret", "", 0, "d")
		return err
	}
	var notices bytes.Buffer
	p := ui.New(&notices, nil, true, false)
	plugin := storedCredential{name: "prod", entry: config.Server{Provenance: clierr.ProvenanceExecLogin}}
	user := storedCredential{name: "prod", entry: config.Server{}}

	unauthorized := create(t, http.StatusUnauthorized)
	require.ErrorIs(t, unauthorized, errCredentialUnauthorized)
	got := tokenCmd{}.classifyRejection(unauthorized, plugin, "", true, p)
	rej, ok := errors.AsType[*pluginCredentialRejected](got)
	require.True(t, ok, "got %v", got)
	assert.Equal(t, "prod", rej.name)
	assert.Empty(t, notices.String(), "a plugin-created login writes no notice")
	got = tokenCmd{}.classifyRejection(unauthorized, user, "", true, p)
	require.ErrorIs(t, got, errStoredCredentialRejected)
	assert.Contains(t, notices.String(), `the stored login credential for "prod" was rejected (credential rejected: the server rejected the credential (401)); falling back to interactive login`)

	forbidden := create(t, http.StatusForbidden)
	require.ErrorIs(t, forbidden, errCredentialForbidden)
	got = tokenCmd{}.classifyRejection(forbidden, user, "", true, p)
	ce, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok, "got %v", got)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, `the stored login credential for "prod" cannot create tokens: credential lacks permission to create tokens: the server refused the request (403) and gave no reason; `+noTokenPermissionText("", true), ce.Message)
}

// TestFreshCredentialErrorKeepsItsSentinels asserts the rejection of the
// credential a login just created keeps its AUTH and RBAC classes and the
// status table's wording.
func TestFreshCredentialErrorKeepsItsSentinels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status  int
		code    string
		message string
	}{
		{http.StatusUnauthorized, clierr.CodeAuth, "the server rejected the login credential it just issued: credential rejected: the server rejected the credential (401)"},
		{http.StatusForbidden, clierr.CodeRBAC, "the login credential cannot create tokens: credential lacks permission to create tokens: the server refused the request (403) and gave no reason"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		t.Cleanup(srv.Close)
		_, err := CreateDerivedToken(t.Context(), srv.Client(), srv.URL, "tok:secret", "", 0, "d")
		ce, ok := errors.AsType[*clierr.Error](freshCredentialError(err))
		require.True(t, ok, tc.status)
		assert.Equal(t, tc.code, ce.Code, tc.status)
		assert.Equal(t, tc.message, ce.Message, tc.status)
	}
}
