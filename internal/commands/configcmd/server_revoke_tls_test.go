package configcmd

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// newTLSServerRevokeDir is newServerRevokeDir over a TLS server whose
// certificate the revokes do not accept. With mismatch the entries name the
// server as localhost and save its certificate as their CA, which verifies
// but does not name that host; otherwise they name it by address and save no
// CA, so its certificate has no known authority. A function armed with the
// result's during method runs when the first connection of a revoke
// arrives, before its handshake fails.
func newTLSServerRevokeDir(t *testing.T, mismatch bool) *serverRevokeDir {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	r := &serverRevokeDir{dir: t.TempDir()}
	r.f = testrancher.New(testrancher.Options{})
	srv := httptest.NewUnstartedServer(r.f)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		if fn := r.armed.Swap(nil); fn != nil {
			(*fn)()
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	create := func() token.LoginCredential {
		cred, err := token.AcquireLoginCredential(t.Context(), srv.Client(),
			token.LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
			ui.New(&bytes.Buffer{}, &linesReader{lines: []string{"e2e-user", "e2e-pass"}}, false, true))
		require.NoError(t, err)
		return cred
	}
	r.login, r.cache = create(), create()
	r.url = srv.URL
	ca := ""
	if mismatch {
		u, err := url.Parse(srv.URL)
		require.NoError(t, err)
		r.url = "https://localhost:" + u.Port()
		ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	}
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": {URL: r.url, CA: ca, Provenance: clierr.ProvenanceDerived, TokenRef: r.login.Ref, TokenUserID: r.login.UserID, CacheBindingID: "p1"},
	}}
	cfg.SetPath(config.FilePath(r.dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(r.dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: r.url}, r.login.Token))
	b := config.CacheBindingFor(config.CanonicalDir(r.dir), cfg, "prod").WithCreatedToken(r.cache.Ref, r.cache.UserID)
	require.NoError(t, store.SetCachedCred("prod-c-1", r.cache.Token, time.Now().Add(time.Hour), "prod", config.CanonicalServerIdentity(r.url), "c-1", b))
	return r
}

// certificateFailures indexes a report's not-revoked tokens by name.
func certificateFailures(t *testing.T, rep token.RevocationReport) map[string]token.NotRevokedToken {
	t.Helper()
	byName := map[string]token.NotRevokedToken{}
	for _, n := range rep.NotRevoked {
		byName[n.Ref.Name] = n
	}
	return byName
}

const (
	recoveryLoginClause = " and add --cacert <file> to the auth login in the recovery steps"
	nextLoginSaveClause = "; save it for entry 'prod' with: rancher config update-server 'prod' --cacert <file>"
)

// TestDeleteServerReportsACertificateFailureOnTheEntryItRemoved asserts the
// command, on its success path, removes the entry and lists both tokens with
// code TLS_TRUST, retry manual and the message worded for the recovery
// login, with the certificate's kind; for a hostname mismatch the message
// names the host and port the entry used and the entry. Mutation: skip the
// refresh of the entry's view and RemoveEntry on the success path (either
// alone changes nothing: the refreshed view already lacks the entry).
func TestDeleteServerReportsACertificateFailureOnTheEntryItRemoved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mismatch bool
		kind     tlstrust.Kind
	}{
		{"an unknown authority", false, tlstrust.KindUnknownAuthority},
		{"a hostname mismatch", true, tlstrust.KindHostnameMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newTLSServerRevokeDir(t, tc.mismatch)
			data, err := r.deleteServer(t)
			require.NoError(t, err)
			_, present := r.entry(t)
			assert.False(t, present, "the entry is removed")
			notRevoked := data["notRevoked"].([]token.NotRevokedToken)
			require.Len(t, notRevoked, 2)
			for _, n := range notRevoked {
				assert.Equal(t, "failed", n.Reason)
				assert.Equal(t, clierr.CodeTLSTrust, n.Code)
				assert.Equal(t, "manual", n.Retry)
				assert.Empty(t, n.Entry)
				require.NotNil(t, n.Certificate)
				assert.Equal(t, tc.kind, n.Certificate.Kind)
				if tc.mismatch {
					u, perr := url.Parse(r.url)
					require.NoError(t, perr)
					assert.Contains(t, n.Error, "not valid for localhost:"+u.Port()+"; check that the server URL of entry 'prod' matches")
				} else {
					assert.Contains(t, n.Error, recoveryLoginClause)
				}
			}
		})
	}
}

// TestDeleteServerKeepsTheNextLoginWordingWhenALocalStepFails asserts a
// local-step failure that keeps the entry lists a token whose revoke failed
// on the server's certificate with retry nextLogin and the message worded for
// that login. Mutation: remove the entry on the local-step failure path.
func TestDeleteServerKeepsTheNextLoginWordingWhenALocalStepFails(t *testing.T) {
	r := newTLSServerRevokeDir(t, false)
	cmd := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd)
	cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, error) {
		return nil, nil, errors.New("the keychain refused")
	}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	_, present := r.entry(t)
	assert.True(t, present, "the entry is kept")
	n := certificateFailures(t, decodeRevocationDetails(t, ce.Details))[r.cache.Ref.Name]
	assert.Equal(t, clierr.CodeTLSTrust, n.Code)
	assert.Equal(t, "nextLogin", n.Retry)
	assert.Equal(t, "prod", n.Entry)
	assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), nextLoginSaveClause)
}

// TestDeleteServerCancelledDuringARevokeReportsTheCertificateFailure asserts a
// signal while the first revoke connects ends the command with INTERRUPTED
// and the entry in place. The token whose revoke failed on the certificate
// is listed with its code and with the message worded the way the rerun
// reports it (the rerun removes the entry); the token the command never
// reached is listed as cancelled.
// Mutation: skip RemoveEntry on the cancelled path.
func TestDeleteServerCancelledDuringARevokeReportsTheCertificateFailure(t *testing.T) {
	r := newTLSServerRevokeDir(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.during(cancel)
	_, err := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeInterrupted, ce.Code)
	_, present := r.entry(t)
	assert.True(t, present, "the entry is left in place")
	byName := certificateFailures(t, decodeRevocationDetails(t, ce.Details))
	failed := byName[r.cache.Ref.Name]
	assert.Equal(t, clierr.CodeTLSTrust, failed.Code)
	assert.Equal(t, "manual", failed.Retry)
	assert.Contains(t, failed.Error, recoveryLoginClause)
	assert.Equal(t, "cancelled", byName[r.login.Ref.Name].Reason)
	assert.Empty(t, byName[r.login.Ref.Name].Code)
}

// TestDeleteServerRefusedByAConflictReportsTheCertificateFailureOfAKeptEntry
// asserts that when a login lands on the entry while the revokes run, the
// command refuses with CONFLICT, keeps the entry, and lists the token whose
// revoke failed on the certificate with the message that matches its retry.
// The login lands from the server's connection hook, which runs outside the
// test goroutine, so the hook only reports the edit's error and the test
// checks it. Mutation: mark the entry removed on a conflict.
func TestDeleteServerRefusedByAConflictReportsTheCertificateFailureOfAKeptEntry(t *testing.T) {
	r := newTLSServerRevokeDir(t, false)
	edited := make(chan error, 1)
	r.during(func() {
		cfg, err := config.Load(config.FilePath(r.dir))
		if err == nil {
			s := cfg.Servers["prod"]
			s.LoggedOutAt, s.TokenRef = "", &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"}
			cfg.Servers["prod"] = s
			err = cfg.Write()
		}
		edited <- err
	})
	_, err := r.deleteServer(t)
	select {
	case eerr := <-edited:
		require.NoError(t, eerr)
	default:
		t.Fatal("the login did not land: no revoke connected")
	}
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	_, present := r.entry(t)
	assert.True(t, present, "the entry is kept")
	n := certificateFailures(t, decodeRevocationDetails(t, ce.Details))[r.cache.Ref.Name]
	assert.Equal(t, clierr.CodeTLSTrust, n.Code)
	assert.Equal(t, "nextLogin", n.Retry)
	assert.Contains(t, clierr.ExpandRemedies(n.Error, ""), nextLoginSaveClause)
}
