package token

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func acquireAgainst(t *testing.T, opts testrancher.Options) (*testrancher.Facade, *httptest.Server, LoginCredential, error) {
	t.Helper()
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var notices bytes.Buffer
	p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
	cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0, p)
	return f, srv, cred, err
}

// TestAcquireDerivedSuccess pins the happy path: S is created as a json
// login carrying the marker, D is created carrying the same marker, S is
// revoked with D as the bearer, and the credential is derived with D's
// resource name and S's cleanup outcome.
func TestAcquireDerivedSuccess(t *testing.T) {
	t.Parallel()
	f, _, cred, err := acquireAgainst(t, testrancher.Options{})
	require.NoError(t, err)
	assert.Equal(t, ProvenanceDerived, cred.Provenance)
	require.NotNil(t, cred.Ref)
	assert.Equal(t, TokenKindExt, cred.Ref.Kind)
	assert.Regexp(t, `^[0-9a-f]{12}$`, cred.Marker)
	descs := f.LoginDescriptions()
	require.Len(t, descs, 2, "login body and create body both carry the description")
	assert.Contains(t, descs[0], "[op:"+cred.Marker+"]")
	assert.Contains(t, descs[1], "[op:"+cred.Marker+"]")
	assert.Equal(t, ResultRevoked, cred.SessionTokenCleanup.Result)
	recs := f.DeleteRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, "v3", recs[0].Kind, "S is a v3 token on the json path: the façade answers json with the Norman shape")
	assert.Equal(t, cred.Token, recs[0].Bearer, "S is revoked with D as the bearer")
	assert.False(t, cred.SessionTokenUnmarked)
	assert.NotEmpty(t, cred.SessionCreatedAt)
	assert.NotEmpty(t, cred.DerivedCreatedAt)
	assert.LessOrEqual(t, cred.SessionCreatedAt, cred.DerivedCreatedAt)
}

// TestAcquireDerivedCreateFailuresRevokeSessionBeforeReturn pins that S is revoked
// and its exact outcome is on the returned error for every create failure
// class, including when the S delete itself answers 404 or 500, and that
// D's cleanup is notAttempted whenever D cannot be addressed and a real
// DELETE outcome when D is known with an unusable bearer.
func TestAcquireDerivedCreateFailuresRevokeSessionBeforeReturn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		opts        testrancher.Options
		wantD       CreateOutcome
		wantDClean  RevokeResult
		wantDeletes int    // total DELETEs the façade saw
		sDeleteMode string // "", "gone", "fail"
		wantS       RevokeResult
	}{
		{"create 500", testrancher.Options{DerivedCreateStatus: 500}, CreateUnknown, ResultNotAttempted, 1, "", ResultRevoked},
		{"create 401", testrancher.Options{DerivedCreateStatus: 401}, CreateNotCreated, ResultNotAttempted, 1, "", ResultRevoked},
		{"create drop response", testrancher.Options{DropDerivedCreateResponse: true}, CreateUnknown, ResultNotAttempted, 1, "", ResultRevoked},
		{"create malformed", testrancher.Options{MalformedDerivedCreateResponse: true}, CreateUnidentified, ResultNotAttempted, 1, "", ResultRevoked},
		{"create known, unusable bearer", testrancher.Options{DerivedBearerOverride: "garbage"}, CreateKnown, ResultRevoked, 2, "", ResultRevoked},
		{"create 500, S already gone", testrancher.Options{DerivedCreateStatus: 500}, CreateUnknown, ResultNotAttempted, 1, "gone", ResultAlreadyGone},
		{"create 500, S delete fails", testrancher.Options{DerivedCreateStatus: 500}, CreateUnknown, ResultNotAttempted, 1, "fail", ResultFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			opts.SessionDeleteMode = tc.sDeleteMode
			f, _, _, err := acquireAgainst(t, opts)
			ae, ok := errors.AsType[*AcquireError](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, CreateKnown, ae.SessionTokenOutcome)
			require.NotNil(t, ae.SessionTokenRef)
			assert.Equal(t, tc.wantD, ae.DerivedCreateOutcome)
			assert.Equal(t, tc.wantDClean, ae.DerivedTokenCleanup.Result, "D cleanup is always an explicit outcome")
			assert.Equal(t, tc.wantS, ae.SessionTokenCleanup.Result, "the cleanup outcome is on the error, not lost to a deferred write")
			assert.NotEmpty(t, ae.Marker)
			assert.NotEmpty(t, ae.SessionCreatedAt)
			recs := f.DeleteRequests()
			require.Len(t, recs, tc.wantDeletes)
			if tc.wantD == CreateKnown {
				require.NotNil(t, ae.DerivedTokenRef)
				assert.Equal(t, "ext", recs[0].Kind)
				assert.Equal(t, ae.DerivedTokenRef.Name, recs[0].Name, "D is deleted by name")
				assert.True(t, strings.HasPrefix(recs[0].Bearer, ae.SessionTokenRef.Name+":"), "with S as the bearer, before S is revoked (S is gone from the store by now, so the bearer is checked by its name prefix)")
				assert.Equal(t, "v3", recs[1].Kind)
			}
		})
	}
}

// TestAcquireCancellationDuringDerivedCreateStillRevokesSession pins that cleanup
// runs on a detached context: the façade sees S's DELETE after the
// operation context is cancelled.
func TestAcquireCancellationDuringDerivedCreateStillRevokesSession(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	f := testrancher.New(testrancher.Options{DerivedCreateHook: func(r *http.Request) { cancel(); <-r.Context().Done() }})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var notices bytes.Buffer
	p := ui.New(&notices, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err := AcquireLoginCredential(ctx, srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0, p)
	ae, ok := errors.AsType[*AcquireError](err)
	require.True(t, ok)
	require.ErrorIs(t, ae.Err, context.Canceled)
	assert.Equal(t, ResultRevoked, ae.SessionTokenCleanup.Result)
	require.Len(t, f.DeleteRequests(), 1)
}

// TestAcquireSessionCreateIndeterminate pins the S create classes: a lost
// response and a body that establishes no shape are unknown of unknown
// kind; a recognized body with neither name nor usable bearer is
// unidentified; a name-only body and a body whose bearer fails validation
// are known. In every case no derived token create is attempted and both cleanups are
// notAttempted.
func TestAcquireSessionCreateIndeterminate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts testrancher.Options
		want CreateOutcome
		kind TokenKind
		ref  bool
	}{
		{"dropped", testrancher.Options{DropLoginResponse: true}, CreateUnknown, "", false},
		{"no shape", testrancher.Options{MalformedLoginResponse: true}, CreateUnknown, "", false},
		{"recognized shape, no identity", testrancher.Options{LoginNoIdentity: true}, CreateUnidentified, TokenKindV3, false},
		{"name only", testrancher.Options{LoginNameOnly: true}, CreateKnown, TokenKindV3, true},
		{"name, unusable bearer", testrancher.Options{LoginBearerOverride: "malformed"}, CreateKnown, TokenKindV3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, _, _, err := acquireAgainst(t, tc.opts)
			ae, ok := errors.AsType[*AcquireError](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.want, ae.SessionTokenOutcome)
			assert.Equal(t, tc.kind, ae.SessionTokenKind)
			assert.Equal(t, tc.ref, ae.SessionTokenRef != nil)
			assert.Equal(t, CreateNotAttempted, ae.DerivedCreateOutcome)
			assert.Equal(t, ResultNotAttempted, ae.SessionTokenCleanup.Result)
			assert.Equal(t, ResultNotAttempted, ae.DerivedTokenCleanup.Result)
			assert.Empty(t, f.DeleteRequests(), "no usable bearer, no delete")
			assert.Empty(t, f.DerivedCreateRequests(), "no usable bearer, no derived token create")
		})
	}
}

// TestAcquireLegacyFallbackTransfersSession pins the ext-absent server:
// S itself is the credential, provenance legacySession, no S cleanup.
func TestAcquireLegacyFallbackTransfersSession(t *testing.T) {
	t.Parallel()
	_, _, cred, err := acquireAgainst(t, testrancher.Options{Legacy: true})
	require.NoError(t, err)
	assert.Equal(t, ProvenanceLegacySession, cred.Provenance)
	require.NotNil(t, cred.Ref)
	assert.Equal(t, TokenKindV3, cred.Ref.Kind)
	assert.Equal(t, RevokeOutcome{}, cred.SessionTokenCleanup)
	assert.NotEmpty(t, cred.SessionCreatedAt)
	assert.Empty(t, cred.DerivedCreatedAt)
}

// TestAcquireSAMLTimeoutCarriesNotAttemptedCleanups pins the no-create path
// of the full acquisition: a SAML sign-in the browser never completes is an
// unknown, unmarked S with the handoff, D not attempted, both cleanups
// explicitly notAttempted, and no operation marker (no marked create was
// attempted). Not parallel: it shrinks the SAML timing.
func TestAcquireSAMLTimeoutCarriesNotAttemptedCleanups(t *testing.T) {
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	defer restore()
	f, srv := samlProviderFacade(t, testrancher.Options{})
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "oktaProvider", ResponseType: "json"}, 0, newHandoffPrompter(nil))
	ae, ok := errors.AsType[*AcquireError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, CreateUnknown, ae.SessionTokenOutcome)
	assert.True(t, ae.SessionTokenUnmarked)
	require.NotNil(t, ae.SessionTokenSearch)
	assert.Equal(t, ResultNotAttempted, ae.SessionTokenCleanup.Result)
	assert.Equal(t, CreateNotAttempted, ae.DerivedCreateOutcome)
	assert.Equal(t, ResultNotAttempted, ae.DerivedTokenCleanup.Result)
	assert.Empty(t, ae.Marker)
	assert.Empty(t, f.DeleteRequests())
	assertRelayOnly(t, f)
}

// TestAcquireErrorWithPrimaryKeepsEvidence pins the composition rule: the
// primary is replaced, every fate field survives.
func TestAcquireErrorWithPrimaryKeepsEvidence(t *testing.T) {
	t.Parallel()
	ae := &AcquireError{Err: errors.New("x509"), Marker: "m", SessionTokenOutcome: CreateKnown, SessionTokenKind: TokenKindV3,
		SessionTokenRef: &TokenRef{Kind: TokenKindV3, Name: "t"}, SessionTokenCleanup: RevokeOutcome{Result: ResultFailed, Error: "503"}}
	replaced := errors.New("classified")
	got := ae.WithPrimary(replaced)
	assert.Same(t, replaced, got.Err)
	assert.Equal(t, ae.Marker, got.Marker)
	assert.Equal(t, ae.SessionTokenKind, got.SessionTokenKind)
	assert.Equal(t, ae.SessionTokenCleanup, got.SessionTokenCleanup)
	assert.Equal(t, ae.SessionTokenRef, got.SessionTokenRef)
	assert.Equal(t, "x509", ae.Err.Error(), "the original is untouched")
	// Every field is copied: a field added to the struct without being
	// checked here fails the count.
	assert.Equal(t, 13, reflect.TypeFor[AcquireError]().NumField(), "AcquireError field set: Err, Marker, SessionTokenOutcome, SessionTokenKind, SessionTokenRef, SessionTokenUnmarked, SessionTokenSearch, SessionTokenCleanup, SessionCreatedAt, DerivedCreateOutcome, DerivedTokenRef, DerivedTokenCleanup, DerivedCreatedAt")
}

// TestAcquireSessionCreateKindKnowledge pins what the acquisition knows
// about S's kind without a name: a lost or unrecognizable synchronous
// response leaves it unknown, a name-only Norman body is v3, and the
// façade's shape option shows that a json request answered with an ext
// object is an ext S.
func TestAcquireSessionCreateKindKnowledge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts testrancher.Options
		kind TokenKind
	}{
		{"dropped", testrancher.Options{DropLoginResponse: true}, ""},
		{"malformed", testrancher.Options{MalformedLoginResponse: true}, ""},
		{"login 500", testrancher.Options{LoginStatus: 500}, ""},
		{"name only", testrancher.Options{LoginNameOnly: true}, TokenKindV3},
		{"name only, ext shape", testrancher.Options{LoginNameOnly: true, LoginShape: "ext"}, TokenKindExt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := acquireAgainst(t, tc.opts)
			ae, ok := errors.AsType[*AcquireError](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.kind, ae.SessionTokenKind)
			if tc.kind == "" {
				assert.Nil(t, ae.SessionTokenRef)
			}
		})
	}
}

// TestAcquireExtShapedSessionThroughLogin pins the ownership state machine
// over an ext-shaped S (a json request Rancher answers with an ext Token
// object): S is revoked on the ext route with D as the bearer, a derived token create
// failure cleans the ext S, and the legacy fallback transfers an ext ref.
func TestAcquireExtShapedSessionThroughLogin(t *testing.T) {
	t.Parallel()
	t.Run("derived success revokes S on the ext route", func(t *testing.T) {
		t.Parallel()
		f, _, cred, err := acquireAgainst(t, testrancher.Options{LoginShape: "ext"})
		require.NoError(t, err)
		require.NotNil(t, cred.SessionTokenCleanup.Ref)
		assert.Equal(t, TokenKindExt, cred.SessionTokenCleanup.Ref.Kind)
		assert.Equal(t, ResultRevoked, cred.SessionTokenCleanup.Result)
		recs := f.DeleteRequests()
		require.Len(t, recs, 1)
		assert.Equal(t, "ext", recs[0].Kind)
		assert.Equal(t, cred.Token, recs[0].Bearer, "S is revoked with D as the bearer")
	})
	t.Run("create failure cleans the ext S", func(t *testing.T) {
		t.Parallel()
		f, _, _, err := acquireAgainst(t, testrancher.Options{LoginShape: "ext", DerivedCreateStatus: 500})
		ae, ok := errors.AsType[*AcquireError](err)
		require.True(t, ok, "got %v", err)
		require.NotNil(t, ae.SessionTokenRef)
		assert.Equal(t, TokenKindExt, ae.SessionTokenRef.Kind)
		assert.Equal(t, ResultRevoked, ae.SessionTokenCleanup.Result)
		assert.Equal(t, "ext", f.DeleteRequests()[0].Kind)
	})
	t.Run("legacy fallback transfers an ext ref", func(t *testing.T) {
		t.Parallel()
		_, _, cred, err := acquireAgainst(t, testrancher.Options{LoginShape: "ext", Legacy: true})
		require.NoError(t, err)
		assert.Equal(t, ProvenanceLegacySession, cred.Provenance)
		require.NotNil(t, cred.Ref)
		assert.Equal(t, TokenKindExt, cred.Ref.Kind)
	})
}

// samlProviderFacade lists a SAML provider and enables the authtokens
// collection the browser sign-in deposits into.
func samlProviderFacade(t *testing.T, opts testrancher.Options) (*testrancher.Facade, *httptest.Server) {
	t.Helper()
	opts.EnableSAMLRelay = true
	opts.ExtraProviders = []string{"oktaProvider"}
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// handoffPrompter is the test's stand-in for the browser: it reads the
// published login URL, remembers the request identity, and runs onHandoff
// (usually a façade deposit) before the CLI's first poll.
type handoffPrompter struct {
	// The embedded prompter is silent and non-TTY with NoInput false: the
	// SAML gate refuses a NoInput prompter before the handoff.
	runtime.Prompter
	requestID string
	publicKey string
	onHandoff func(requestID, publicKey string)
}

func (h *handoffPrompter) Notice(msg string) {
	before, _, found := strings.Cut(msg, "/dashboard/auth/login?")
	if !found {
		return
	}
	u, err := url.Parse(strings.TrimSpace(msg[strings.LastIndex(before, " ")+1:]))
	if err != nil {
		return
	}
	h.requestID, h.publicKey = u.Query().Get("requestId"), u.Query().Get("publicKey")
	if h.onHandoff != nil {
		h.onHandoff(h.requestID, h.publicKey)
	}
}

func newHandoffPrompter(onHandoff func(requestID, publicKey string)) *handoffPrompter {
	return &handoffPrompter{Prompter: ui.New(io.Discard, strings.NewReader(""), false, false), onHandoff: onHandoff}
}

func samlInput(server string) *loginInput {
	return &loginInput{server: server, authProvider: "oktaProvider", responseType: "json"}
}

// assertRelayOnly pins the SAML wire protocol: the CLI never posts a login
// body; it publishes a URL and polls the authtokens collection.
func assertRelayOnly(t *testing.T, f *testrancher.Facade) {
	t.Helper()
	assert.Zero(t, f.LoginCount(), "the SAML path issues no login POST")
	for _, r := range f.RelayRequests() {
		assert.Contains(t, []string{"GET", "DELETE"}, r.Method, r.Path)
	}
}

// TestSAMLPostHandoffFailuresAreUnknownWithHandoff pins the asynchronous
// state machine: after the browser URL is published, cancellation and
// timeout are unknown S creates carrying the handoff; a deposited auth
// token with a valid bearer is known; a payload that cannot be decoded is
// unknown; a decrypted bearer failing validation is unidentified.
func TestSAMLPostHandoffFailuresAreUnknownWithHandoff(t *testing.T) {
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	defer restore()

	t.Run("cancelled while polling", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		ctx, cancel := context.WithCancel(t.Context())
		// Synchronous: after the handoff is recorded, before the first poll,
		// so no timer race decides the outcome.
		p := newHandoffPrompter(func(string, string) { cancel() })
		res, err := loginAndGenerateCred(ctx, srv.Client(), samlInput(srv.URL), p)
		require.ErrorIs(t, err, context.Canceled)
		require.NotNil(t, res.Handoff)
		assert.Equal(t, "oktaProvider", res.Handoff.Provider)
		assert.Equal(t, p.requestID, res.Handoff.RequestID)
		assert.NotEmpty(t, res.Handoff.HandoffAt)
		assert.NotEmpty(t, res.Handoff.PollEndedAt)
		assert.Equal(t, CreateUnknown, res.Create.Outcome)
		assert.False(t, res.Create.Marked)
		assertRelayOnly(t, f)
	})
	t.Run("timeout while callback in flight", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		res, err := loginAndGenerateCred(t.Context(), srv.Client(), samlInput(srv.URL), newHandoffPrompter(nil))
		require.Error(t, err)
		assert.Equal(t, CreateUnknown, res.Create.Outcome)
		require.NotNil(t, res.Handoff)
		assertRelayOnly(t, f)
	})
	t.Run("deposit after timeout is still findable", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		p := newHandoffPrompter(nil)
		res, err := loginAndGenerateCred(t.Context(), srv.Client(), samlInput(srv.URL), p)
		require.Error(t, err)
		f.DepositRelay(p.requestID, p.publicKey) // the browser completes after the CLI stopped waiting
		// The recovery criteria search forward from HandoffAt; PollEndedAt is not a bound.
		assert.LessOrEqual(t, res.Handoff.HandoffAt, res.Handoff.PollEndedAt)
		assert.Equal(t, 1, f.TokenCount(), "the server created S after the CLI stopped waiting")
	})
	t.Run("valid deposit is known", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		p := newHandoffPrompter(func(id, pub string) { f.DepositRelay(id, pub) })
		res, err := loginAndGenerateCred(t.Context(), srv.Client(), samlInput(srv.URL), p)
		require.NoError(t, err)
		assert.Equal(t, CreateKnown, res.Create.Outcome)
		require.NotNil(t, res.Create.Ref)
		assert.Equal(t, TokenKindV3, res.Create.Ref.Kind)
		assert.NotEmpty(t, res.Create.Bearer)
		assert.Equal(t, 1, f.TokenCount())
		assertRelayOnly(t, f)
		recs := f.RelayRequests()
		assert.Equal(t, "DELETE", recs[len(recs)-1].Method, "the deposited copy is deleted after decryption")
	})
	t.Run("undecodable payload is unknown", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		p := newHandoffPrompter(func(id, _ string) { f.DepositRelayRaw(id, "!!not-base64") })
		res, err := loginAndGenerateCred(t.Context(), srv.Client(), samlInput(srv.URL), p)
		require.Error(t, err)
		assert.Equal(t, CreateUnknown, res.Create.Outcome)
		require.NotNil(t, res.Handoff)
	})
	t.Run("decrypted bearer failing validation is unidentified", func(t *testing.T) {
		f, srv := samlProviderFacade(t, testrancher.Options{})
		p := newHandoffPrompter(func(id, pub string) { f.DepositRelayPlaintext(id, pub, "no-colon-here") })
		res, err := loginAndGenerateCred(t.Context(), srv.Client(), samlInput(srv.URL), p)
		require.Error(t, err)
		assert.Equal(t, CreateUnidentified, res.Create.Outcome)
		assert.Nil(t, res.Create.Ref)
		assert.Empty(t, res.Create.Bearer)
	})
}

// TestAcquireRevokesSessionCreatedUnderAnUnexpectedStatus pins that
// ownership begins the moment a usable session token exists, whatever
// status carried it: a 200 with a full token body is a create the CLI
// refuses (a create answers 201), and the token it created is revoked
// before the failure is reported.
func TestAcquireRevokesSessionCreatedUnderAnUnexpectedStatus(t *testing.T) {
	t.Parallel()
	f, _, _, err := acquireAgainst(t, testrancher.Options{LoginStatus: http.StatusOK})
	ae, ok := errors.AsType[*AcquireError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, CreateKnown, ae.SessionTokenOutcome)
	require.NotNil(t, ae.SessionTokenRef)
	assert.Equal(t, ResultRevoked, ae.SessionTokenCleanup.Result, "a token the CLI could reach is revoked, not left for manual recovery")
	assert.Equal(t, CreateNotAttempted, ae.DerivedCreateOutcome)
	recs := f.DeleteRequests()
	require.Len(t, recs, 1)
	assert.Equal(t, "v3", recs[0].Kind)
	assert.Equal(t, ae.SessionTokenRef.Name, recs[0].Name)
}
