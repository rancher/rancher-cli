package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailLoginRendersThreeFatesInOrderAndRevokesTheCredentialLast asserts
// the plugin-created login's failure shape: the session, derived, and
// cluster fates in that order, the cluster token's cleanup as the caller
// recorded it, the credential revoked by FailLogin itself, and a recovery
// text that names every unclean token.
func TestFailLoginRendersThreeFatesInOrderAndRevokesTheCredentialLast(t *testing.T) {
	t.Parallel()
	var deletes []string
	srv := revokeRecorder(t, &deletes)
	dRef := &TokenRef{Kind: TokenKindExt, Name: "token-d"}
	cRef := &TokenRef{Kind: TokenKindExt, Name: "token-c"}
	cred := LoginCredential{Token: "ext/token-d:s", Provenance: ProvenanceDerived, Marker: "abcdef012345", Ref: dRef,
		SessionTokenCleanup: RevokeOutcome{Ref: &TokenRef{Kind: TokenKindV3, Name: "s1"}, Result: ResultRevoked},
		client:              srv.Client(), base: srv.URL}
	st := LoginStates{Config: clierr.ConfigNotWritten,
		ClusterToken: &clierr.TokenFate{Outcome: clierr.CreateKnown, Ref: cRef, Marked: true,
			Cleanup: &clierr.RevokeOutcome{Ref: cRef, Result: clierr.ResultFailed, Error: "503"}}}
	err := FailLogin(errors.New("scope mismatch"), clierr.StageClusterToken, st, cred, "exec-login-r.ex")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok)
	assert.Equal(t, clierr.StageClusterToken, d.Stage)
	require.NotNil(t, d.ClusterToken)
	assert.Equal(t, clierr.ResultFailed, d.ClusterToken.Cleanup.Result)
	require.NotNil(t, d.DerivedToken)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result, "FailLogin revoked the credential")
	assert.Equal(t, []string{ExtTokensPath + "/token-d"}, deletes, "the cluster token was the caller's revoke, not FailLogin's")
	assert.Contains(t, d.Recovery, "rancher delete token token-c --apply --yes")
	assert.NotContains(t, d.Recovery, "auth login exec-login", "a plugin entry cannot be logged in to by hand")
	assert.Contains(t, d.Recovery, "run the kubectl command again")
	assert.Empty(t, ce.Hint, "no paste-invalid hint for a plugin entry")
	assert.Empty(t, d.ConfigState, "no persistence system was reached at this stage")
	lines := d.TextDetails()
	sIdx, dIdx, cIdx := indexOfPrefix(lines, "session token"), indexOfPrefix(lines, "derived token"), indexOfPrefix(lines, "cluster token")
	assert.True(t, sIdx < dIdx && dIdx < cIdx, "%v", lines)
}

// TestFailLoginCarriesConfigRecoveryAtCommit asserts a failed rollback's
// component reaches the details and the recovery text at stage commit.
func TestFailLoginCarriesConfigRecoveryAtCommit(t *testing.T) {
	t.Parallel()
	var deletes []string
	srv := revokeRecorder(t, &deletes)
	cred := LoginCredential{Token: "ext/token-d:s", Provenance: ProvenanceDerived, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-d"}, client: srv.Client(), base: srv.URL}
	st := LoginStates{Config: clierr.ConfigRollbackFailed,
		Commit:         &config.CommitOutcome{Backend: clierr.BackendFile, State: config.CommitNotWritten},
		ConfigRecovery: &clierr.ConfigRecovery{Entry: "exec-login-r.ex", Command: "rancher config delete-server 'exec-login-r.ex'"}}
	err := FailLogin(errors.New("refused"), clierr.StageCommit, st, cred, "exec-login-r.ex")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok)
	assert.Equal(t, clierr.ConfigRollbackFailed, d.ConfigState)
	require.NotNil(t, d.ConfigRecovery)
	assert.Contains(t, d.Recovery, "remove it with: rancher config delete-server 'exec-login-r.ex'")
	assert.Empty(t, ce.Hint, "a multi-step recovery is never a hint")
}

// TestLoginFailureKeepsThePrimaryCauseChain asserts that composition never
// breaks errors.Is/As: a cancelled commit still is context.Canceled, a
// store sentinel is still reachable, and the classified primary is still
// found by errors.As beneath the composed error.
func TestLoginFailureKeepsThePrimaryCauseChain(t *testing.T) {
	t.Parallel()
	primary := clierr.New(clierr.CodeGeneric, "store failed").WithCause(fmt.Errorf("wrapped: %w", context.Canceled))
	err := FailLogin(primary, clierr.StageCommit, LoginStates{}, UserSupplied("id:secret"), "prod")
	require.ErrorIs(t, err, context.Canceled)
	got, ok := errors.AsType[*clierr.Error](errors.Unwrap(err))
	require.True(t, ok, "the classified primary is the cause")
	assert.Same(t, primary, got)
	sentinel := FailLogin(fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported), clierr.StageCommit, LoginStates{}, UserSupplied("id:secret"), "prod")
	require.ErrorIs(t, sentinel, config.ErrTokenStoreUnsupported)
}

// TestTokenFatesFrom asserts the projection of a credential onto its token
// fates: on the derived path the session fate is acquisition's cleanup and
// the derived fate takes the given outcome; on the legacy path the session
// token is the credential and takes it; a user-supplied credential has none.
func TestTokenFatesFrom(t *testing.T) {
	t.Parallel()
	sRef := &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "s"}
	dRef := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "d"}
	sClean := clierr.RevokeOutcome{Ref: sRef, Result: clierr.ResultRevoked}
	dClean := clierr.RevokeOutcome{Ref: dRef, Result: clierr.ResultFailed, Error: "500"}

	session, derived := tokenFatesFrom(LoginCredential{Provenance: ProvenanceDerived, Ref: dRef, SessionTokenCleanup: sClean, SessionCreatedAt: "s-at", DerivedCreatedAt: "d-at"}, dClean)
	require.NotNil(t, session)
	require.NotNil(t, derived)
	assert.Equal(t, sRef, session.Ref)
	assert.Equal(t, &sClean, session.Cleanup)
	assert.Equal(t, "s-at", session.CreatedAt)
	assert.True(t, session.Marked)
	assert.Equal(t, dRef, derived.Ref)
	assert.Equal(t, &dClean, derived.Cleanup)
	assert.Equal(t, "d-at", derived.CreatedAt)

	session, derived = tokenFatesFrom(LoginCredential{Provenance: ProvenanceLegacySession, Ref: sRef, SessionTokenUnmarked: true, SessionCreatedAt: "s-at"}, sClean)
	require.NotNil(t, session)
	assert.Nil(t, derived)
	assert.Equal(t, sRef, session.Ref)
	assert.Equal(t, &sClean, session.Cleanup, "the legacy session token is the credential and takes the given outcome")
	assert.False(t, session.Marked)

	session, derived = tokenFatesFrom(UserSupplied("id:secret"), clierr.RevokeOutcome{Result: clierr.ResultNotApplicable})
	assert.Nil(t, session)
	assert.Nil(t, derived)
}

func indexOfPrefix(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// revokeRecorder answers every DELETE with 200 and records the path.
func revokeRecorder(t *testing.T, paths *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			*paths = append(*paths, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}
