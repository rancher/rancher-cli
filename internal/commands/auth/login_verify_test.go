package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// newVerifyFake starts an httptest server standing in for the k8s discovery
// endpoint provider.Preflight probes (BaseURL + /k8s/clusters/local/version):
// a request bearing wantToken succeeds, anything else gets an unauthenticated
// 401 the way a real Rancher proxy rejects a bad token.
func newVerifyFake(t *testing.T, wantToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/k8s/clusters/local/version" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// verifyLoginCmd wires the real provider.Preflight into loginCmd.verify -
// unlike newPlainLoginCmd's nil-skip default, these tests exercise the
// verification round-trip itself.
func verifyLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		verify:   provider.Preflight,
	}
}

func TestLoginVerifiesValidTokenBeforeStoringSucceeds(t *testing.T) {
	t.Parallel()
	srv := newVerifyFake(t, "id:sec")
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	res, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "logged in to prod ("+srv.URL+")"))
	assert.Equal(t, true, res.Data.(map[string]any)["verified"], "the round-trip ran and succeeded")

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginVerifyRejects401TokenNothingPersisted(t *testing.T) {
	t.Parallel()
	srv := newVerifyFake(t, "good:sec")
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("bad:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Contains(t, ce.Message, "re-run with --no-verify to store the token without checking it")
	assert.Empty(t, ce.Hint)

	_, terr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, terr, "a rejected token must never be persisted")
}

func TestLoginVerifyUnreachableServerRefusalClassifiesReachFailure(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://127.0.0.1:1", "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "re-run with --no-verify to store the token without checking it")
	assert.Empty(t, ce.Hint)

	_, terr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, terr, "an unreachable server must never get a persisted token")
}

func TestLoginNoVerifyStoresAgainstDeadServerWithExactText(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://127.0.0.1:1", "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true, "no-verify": true}, Pos: []string{"prod"}}
	res, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "token stored for prod (https://127.0.0.1:1) - not verified"))
	assert.Equal(t, false, res.Data.(map[string]any)["verified"], "the round-trip was skipped via --no-verify")

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginNoVerifyWithExplicitProviderFlagsIsValidation(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{"auth-provider", "auth-flow"} {
		dir, store := cmdtest.FilePolicyDir(t)
		seedServer(t, dir, "prod", "https://r.ex", "", false)
		in := cmdtest.Input{
			Str:  map[string]string{extra: "x"},
			Flag: map[string]bool{"no-verify": true},
			Pos:  []string{"prod"},
		}
		_, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, extra)
		assert.Equal(t, clierr.CodeValidation, ce.Code, extra)
	}
}

// TestLoginProviderFlowSkipsVerifyEvenWhenWired asserts that a provider-issued
// token never takes the round-trip, even when verify is wired: the flag
// exclusivity check only catches the EXPLICIT --auth-provider/--auth-flow
// spelling of --no-verify's incompatibility, but the provider flow must skip
// verification unconditionally, including when reached through the
// interactive paste-or-provider menu.
func TestLoginProviderFlowSkipsVerifyEvenWhenWired(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess1:sessionsecret"})
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		verify: func(context.Context, config.Effective) (string, error) {
			t.Fatal("verify must not run for a provider-issued token")
			return "", nil
		},
	}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	res, err := cmd.Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, true, res.Data.(map[string]any)["verified"], "a provider-issued token is authenticated by construction")
}

// TestLoginDelegatedVerifiesTokenBeforeStoring asserts that the verification
// round-trip also runs on the --url (delegated) path, immediately after this
// login's own registration commits - not only for an already-registered
// entry.
func TestLoginDelegatedVerifiesTokenBeforeStoring(t *testing.T) {
	t.Parallel()
	srv := newVerifyFake(t, "id:sec")
	dir, store := cmdtest.FilePolicyDir(t)
	in := cmdtest.Input{Str: map[string]string{"url": srv.URL}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	res, err := verifyLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Text, "logged in to prod ("+srv.URL+")"))
}

// TestLoginDelegatedVerifyFailureGetsUndoGuidanceNotNoVerifyHint asserts that a
// verify failure on the --url (delegated) path is decorated exactly like any
// other delegated auth failure: the message keeps the undo-the-registration
// guidance and the hint stays the retry command, not the --no-verify hint a
// plain (already-registered) login's verify failure gets - an orphaned
// registration is the bigger problem, so undoing it is the safe default
// action, not silently storing a token against a server the operator just
// failed to reach. --no-verify still gets mentioned, but only in the message
// body, as a secondary option.
func TestLoginDelegatedVerifyFailureGetsUndoGuidanceNotNoVerifyHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := verifyLoginCmd(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://127.0.0.1:1"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"fresh"}}
	_, err := cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.Rancher+" auth login 'fresh'", ce.Hint, "the hint stays the undo-registration retry command, not --no-verify")
	assert.Contains(t, ce.Message, "delete-server", "the message keeps the undo-the-registration guidance")
	assert.Contains(t, ce.Message, "--no-verify", "the message also mentions --no-verify as a secondary option")

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	_, ok := cfg.Servers["fresh"]
	assert.True(t, ok, "registration committed before the verify attempt; a failed verify does not roll it back")

	_, terr := store.GetToken(config.ServerRef{Name: "fresh"})
	assert.Error(t, terr, "a token that failed verification must never be persisted")
}
