package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// revokeDir is a directory whose entry prod holds a login of e2e-user the
// CLI created and named on the fixture Rancher, with one cache record of
// prod's own credential that names its token.
type revokeDir struct {
	dir   string
	url   string
	f     *testrancher.Facade
	login token.LoginCredential
	cache token.LoginCredential
}

func newRevokeDir(t *testing.T, opts testrancher.Options) revokeDir {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	create := func() token.LoginCredential {
		cred, err := token.AcquireLoginCredential(t.Context(), srv.Client(),
			token.LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
			ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
		require.NoError(t, err)
		return cred
	}
	r := revokeDir{dir: t.TempDir(), url: srv.URL, f: f, login: create(), cache: create()}
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": {URL: srv.URL, Provenance: clierr.ProvenanceDerived, TokenRef: r.login.Ref, TokenUserID: r.login.UserID, CacheBindingID: "b1"},
	}}
	cfg.SetPath(config.FilePath(r.dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(r.dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, r.login.Token))
	b := config.CacheBindingFor(config.CanonicalDir(r.dir), cfg, "prod").WithCreatedToken(r.cache.Ref, r.cache.UserID)
	require.NoError(t, store.SetCachedCred("prod@"+config.CanonicalServerIdentity(srv.URL)+"/c-1", r.cache.Token, time.Now().Add(time.Hour),
		"prod", config.CanonicalServerIdentity(srv.URL), "c-1", b))
	return r
}

// TestLogoutReportsWhatTheServerRevoked asserts the logout's payload and
// text: both lists, decoded from the JSON envelope's data, with the cache
// token and the login revoked; a notRevoked list that is empty, never
// null; and one text line per token under its server.
func TestLogoutReportsWhatTheServerRevoked(t *testing.T) {
	r := newRevokeDir(t, testrancher.Options{})
	res, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var data struct {
		LoggedOut  []string                `json:"loggedOut"`
		Revoked    []token.RevokedToken    `json:"revoked"`
		NotRevoked []token.NotRevokedToken `json:"notRevoked"`
	}
	require.NoError(t, json.Unmarshal(raw, &data))
	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	assert.Equal(t, []any{}, generic["notRevoked"], "an empty list, never null")
	assert.Equal(t, []string{"prod"}, data.LoggedOut)
	require.Len(t, data.Revoked, 2)
	assert.Equal(t, token.RevokedToken{Ref: *r.cache.Ref, Source: "cache", Server: config.CanonicalServerIdentity(r.url)}, data.Revoked[0])
	assert.Equal(t, token.RevokedToken{Ref: *r.login.Ref, Source: "login", Server: config.CanonicalServerIdentity(r.url)}, data.Revoked[1])
	_, alive := r.f.TokenOwner(r.login.Ref.Name)
	assert.False(t, alive)
	assert.Contains(t, res.Text, "\ntokens on "+config.CanonicalServerIdentity(r.url)+":\n  revoked "+r.cache.Ref.String()+" (cluster token, c-1)\n  revoked "+r.login.Ref.String()+" (login token)")
}

// TestLogoutRevokesNothingUnderTheConfigLock asserts no request runs while
// the config lock is held.
func TestLogoutRevokesNothingUnderTheConfigLock(t *testing.T) {
	var dir string
	var violations, deletes atomic.Int32
	r := newRevokeDir(t, testrancher.Options{RequestHook: func(req *http.Request) {
		if req.Method != http.MethodDelete || dir == "" {
			return
		}
		deletes.Add(1)
		fl := flock.New(config.LockPath(dir))
		if ok, err := fl.TryLock(); err != nil || !ok {
			violations.Add(1)
			return
		}
		_ = fl.Unlock()
	}})
	dir = r.dir
	_, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, int32(2), deletes.Load())
	assert.Equal(t, int32(0), violations.Load())
}

// TestLogoutReadsNoLoginTheCLIDidNotCreate asserts that the helper of an
// entry the CLI did not create, and of a CLI-created entry whose TokenRef a
// read-only store's login cleared, is asked only to erase; a login the CLI
// created and named is read once before it is erased.
func TestLogoutReadsNoLoginTheCLIDidNotCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    config.Server
		wantVerb []string
	}{
		{"a pasted login", config.Server{Provenance: clierr.ProvenanceUserSupplied}, []string{"erase"}},
		{"no provenance", config.Server{}, []string{"erase"}},
		{"a CLI-created login with its TokenRef cleared", config.Server{Provenance: clierr.ProvenanceDerived}, []string{"erase"}},
		{"a CLI-created and named login", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}}, []string{"get", "erase"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, statePath := cmdtest.SeedExecLogout(t, "", "prod")
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			e := tc.entry
			e.URL = cfg.Servers["prod"].URL
			cfg.Servers["prod"] = e
			require.NoError(t, cfg.Write())
			clearHelperLog(t, statePath)

			_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			require.NoError(t, err)
			var verbs []string
			for _, line := range helperLog(t, statePath) {
				verb, _, _ := strings.Cut(line, " ")
				verbs = append(verbs, verb)
			}
			assert.Equal(t, tc.wantVerb, verbs)
		})
	}
}

func helperLog(t *testing.T, statePath string) []string {
	t.Helper()
	b, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var st struct {
		Log []string `json:"log"`
	}
	require.NoError(t, json.Unmarshal(b, &st))
	return st.Log
}

func clearHelperLog(t *testing.T, statePath string) {
	t.Helper()
	b, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var st map[string]any
	require.NoError(t, json.Unmarshal(b, &st))
	delete(st, "log")
	out, err := json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, out, 0o600))
}

// TestALogoutThatFailsCarriesBothLists asserts a failed login delete: the
// error's details carry the cache token the run still revoked and the
// login reported notDeleted, and the exit is the delete's failure.
func TestALogoutThatFailsCarriesBothLists(t *testing.T) {
	r := newRevokeDir(t, testrancher.Options{})
	creds := config.CredentialsFilePath(r.dir)
	require.NoError(t, os.Remove(creds))
	require.NoError(t, os.Mkdir(creds, 0o700))
	_, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.ErrorContains(t, err, `failed to log out "prod"`)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	require.Len(t, details.Revoked, 1)
	assert.Equal(t, r.cache.Ref.Name, details.Revoked[0].Ref.Name)
	require.Len(t, details.NotRevoked, 1)
	assert.Equal(t, "notDeleted", details.NotRevoked[0].Reason)
	lines := details.TextDetails()
	assert.Contains(t, lines, "not revoked: "+r.login.Ref.String()+" on "+config.CanonicalServerIdentity(r.url)+" (notDeleted, retry manual)")
	assert.Equal(t, "those credentials are gone regardless of this failure - run "+clierr.Rancher+" auth logout 'prod' to finish",
		lines[len(lines)-1], "a single-entry logout names that entry's rerun")
	_, statErr := os.Stat(filepath.Join(r.dir, "config.yaml"))
	require.NoError(t, statErr)
}

// TestLogoutAllReportsACredentialWithNoEntryUnproven asserts that the
// keychain credential of an entry removed from config.yaml by hand, which
// logout --all deletes, is reported unproven with no entry and no server:
// nothing names its token.
func TestLogoutAllReportsACredentialWithNoEntryUnproven(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	keychain, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, keychain.SetToken(config.ServerRef{Name: "gone", URL: "https://gone.ex"}, "id:sec"))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	var unproven []token.NotRevokedToken
	for _, n := range res.Data.(map[string]any)["notRevoked"].([]token.NotRevokedToken) {
		if n.Reason == "unproven" {
			unproven = append(unproven, n)
		}
	}
	require.Len(t, unproven, 1)
	assert.Empty(t, unproven[0].Server)
	assert.Empty(t, unproven[0].Entry)
	assert.Equal(t, "login", unproven[0].Source)
	assert.Equal(t, "manual", unproven[0].Retry)
}

// holdCacheLock takes dir's token-cache lock for the rest of the test and
// shortens the wait for it, as another process holding it would.
func holdCacheLock(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(config.SetCacheLockWaitForTest(200 * time.Millisecond))
	fl := flock.New(filepath.Join(dir, "token-cache.lock"))
	require.NoError(t, fl.Lock())
	t.Cleanup(func() { _ = fl.Unlock() })
}

// TestALogoutThatCannotTakeTheCacheLockDeletesNothing asserts that a
// token-cache lock the logout cannot take fails it before anything is
// written or deleted, with the lock's own error, for one entry and for
// --all: the login token, the cache record and the server's tokens all
// stay.
func TestALogoutThatCannotTakeTheCacheLockDeletesNothing(t *testing.T) {
	for name, in := range map[string]cmdtest.Input{
		"one entry": {Pos: []string{"prod"}},
		"--all":     {Flag: map[string]bool{"all": true}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRevokeDir(t, testrancher.Options{})
			before, err := config.Load(config.FilePath(r.dir))
			require.NoError(t, err)
			deletes := len(r.f.DeleteRequests())
			holdCacheLock(t, r.dir)
			_, err = NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), in, ui.Discard())
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
			assert.Len(t, r.f.DeleteRequests(), deletes, "nothing is revoked")
			tok, terr := config.NewFileStore(r.dir).GetToken(config.ServerRef{Name: "prod", URL: r.url})
			require.NoError(t, terr)
			assert.Equal(t, r.login.Token, tok, "the login token stays")
			after, err := config.Load(config.FilePath(r.dir))
			require.NoError(t, err)
			assert.Equal(t, before.Servers, after.Servers, "nothing is written")
			b, rerr := os.ReadFile(filepath.Join(r.dir, "token-cache.yaml"))
			require.NoError(t, rerr)
			assert.NotEmpty(t, b, "the cache record stays")
		})
	}
}

// TestALogoutInterruptedDuringTheRevokesReportsBothLists asserts that a
// Ctrl-C during the revokes ends the logout with INTERRUPTED, whose
// details carry what was revoked and what was not reached, while the
// logout itself completed.
func TestALogoutInterruptedDuringTheRevokesReportsBothLists(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var armed atomic.Bool
	r := newRevokeDir(t, testrancher.Options{RequestHook: func(req *http.Request) {
		if req.Method == http.MethodDelete && armed.CompareAndSwap(true, false) {
			cancel()
		}
	}})
	armed.Store(true)
	_, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeInterrupted, ce.Code)
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Equal(t, []string{"prod"}, details.LoggedOut)
	require.Len(t, details.Revoked, 1, "the request in flight completes")
	require.Len(t, details.NotRevoked, 1)
	assert.Equal(t, "cancelled", details.NotRevoked[0].Reason)
	assert.Contains(t, strings.Join(ce.Warnings(), "\n"), "1 token(s) recorded on prod are revoked by the next login of prod", "the warnings carry what revokes the cancelled token")
	assert.NotContains(t, strings.Join(details.TextDetails(), "\n"), "to finish", "nothing is left to finish once the local delete completed")
	_, terr := config.NewFileStore(r.dir).GetToken(config.ServerRef{Name: "prod", URL: r.url})
	require.ErrorIs(t, terr, config.ErrNoToken, "the local delete completed")
}

// TestALogoutWhoseSignalCameAfterTheLastRevokeEndsNormally asserts that a
// Ctrl-C landing during the last revoke, which completes, leaves no token
// unreached: the logout returns its result, not INTERRUPTED.
func TestALogoutWhoseSignalCameAfterTheLastRevokeEndsNormally(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var deletes atomic.Int32
	var armed atomic.Bool
	r := newRevokeDir(t, testrancher.Options{RequestHook: func(req *http.Request) {
		if req.Method == http.MethodDelete && armed.Load() && deletes.Add(1) == 2 {
			cancel()
		}
	}})
	armed.Store(true)
	res, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Len(t, res.Data.(map[string]any)["revoked"], 2)
}

// TestALogoutReportsAgainstAnEntryChangeMadeDuringTheRevokes asserts that
// a change of the entry that lands while the logout revokes is what its
// report is judged by: a token the server failed to revoke is reported
// with a manual retry and a recovery that logs in to the URL the entry had
// before, since no login of the moved or removed entry revokes it; a
// removed entry is not named.
func TestALogoutReportsAgainstAnEntryChangeMadeDuringTheRevokes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(cfg *config.Config)
		wantEntry string
	}{
		{"a URL change", func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.URL, s.LoggedOutAt, s.TokenUserID = "https://prod2.ex", "", ""
			cfg.Servers["prod"] = s
		}, "prod"},
		{"the entry removed", func(cfg *config.Config) { delete(cfg.Servers, "prod") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dir atomic.Pointer[string]
			var edited atomic.Bool
			r := newRevokeDir(t, testrancher.Options{RequestHook: func(req *http.Request) {
				d := dir.Load()
				if req.Method != http.MethodDelete || d == nil || !edited.CompareAndSwap(false, true) {
					return
				}
				cfg, err := config.Load(config.FilePath(*d))
				if err != nil {
					return
				}
				tc.change(cfg)
				_ = cfg.Write()
			}})
			r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
			dir.Store(&r.dir)
			res, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			require.NoError(t, err)
			require.True(t, edited.Load())
			after, err := config.Load(config.FilePath(r.dir))
			require.NoError(t, err)
			e, ok := after.Servers["prod"]
			assert.Equal(t, tc.wantEntry != "", ok, "the change was written")
			if ok {
				assert.Equal(t, "https://prod2.ex", e.URL, "the change was written")
			}
			notRevoked := res.Data.(map[string]any)["notRevoked"].([]token.NotRevokedToken)
			require.Len(t, notRevoked, 1)
			item := notRevoked[0]
			assert.Equal(t, r.cache.Ref.Name, item.Ref.Name)
			assert.Equal(t, "failed", item.Reason)
			assert.Equal(t, tc.wantEntry, item.Entry)
			assert.Equal(t, "manual", item.Retry)
			assert.Equal(t, config.CanonicalServerIdentity(r.url), item.Server)
			require.NotEmpty(t, item.Recovery)
			assert.Contains(t, item.Recovery[0], "--url "+clierr.ShellQuote(r.url))
		})
	}
}

// useKeychainLogin moves the directory's login to the keychain store.
func useKeychainLogin(t *testing.T, r revokeDir) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	cfg.CredentialStore = "keychain"
	require.NoError(t, cfg.Write())
	keychain, err := config.OpenStore("keychain", r.dir)
	require.NoError(t, err)
	require.NoError(t, keychain.SetToken(config.ServerRef{Name: "prod", URL: r.url}, r.login.Token))
}

// failIdentityRemoval makes the keychain login delete fail after the item
// is gone: on the item delete it cancels the command (a Ctrl-C) or takes
// the credential identity lock with a short wait (a timeout), so the
// removal of the identity record fails. The returned func releases it.
func failIdentityRemoval(t *testing.T, dir string, interrupt bool, cancel context.CancelFunc) (release func()) {
	t.Helper()
	t.Cleanup(config.SetLockWaitTimeoutForTest(300 * time.Millisecond))
	fl := flock.New(filepath.Join(dir, "credstore-id.lock"))
	config.SetKeyringObserverForTest(func(op string) {
		switch {
		case op != "delete":
		case interrupt:
			cancel()
		default:
			_ = fl.Lock()
		}
	})
	release = func() {
		config.SetKeyringObserverForTest(nil)
		_ = fl.Unlock()
	}
	t.Cleanup(release)
	return release
}

// TestALogoutWhoseLoginDeleteFailedAfterTheTokenWentCountsItDeleted
// asserts that a keychain login delete that failed after the item was
// gone counts the login as deleted - revoked, reported by its outcome,
// its TokenRef not put back, the keychain in deletedFrom - while the
// failure stays the logout's, with both lists and nothing claimed logged
// out. A Ctrl-C ends it INTERRUPTED with both tokens cancelled and
// recorded on the entry for its next login, and the rerun agrees; an
// identity-lock timeout ends it LOCK_TIMEOUT with both tokens revoked,
// and the rerun finds nothing left.
func TestALogoutWhoseLoginDeleteFailedAfterTheTokenWentCountsItDeleted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
		code      string
	}{{"a Ctrl-C", true, clierr.CodeInterrupted}, {"an identity-lock timeout", false, clierr.CodeLockTimeout}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRevokeDir(t, testrancher.Options{})
			useKeychainLogin(t, r)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release := failIdentityRemoval(t, r.dir, tc.interrupt, cancel)
			_, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			release()

			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.code, ce.Code)
			report, ok := ce.Details.(LogoutAllPartialFailure)
			require.True(t, ok)
			assert.Empty(t, report.LoggedOut, "the logout did not complete")
			assert.Contains(t, strings.Join(report.TextDetails(), "\n"), "run "+clierr.Rancher+" auth logout 'prod' to finish")
			assert.Contains(t, report.DeletedFrom["prod"], "keychain")
			e, ok := mustLoad(t, r.dir).Servers["prod"]
			require.True(t, ok)
			assert.Nil(t, e.TokenRef, "the TokenRef is not put back")
			assert.NotEmpty(t, e.LoggedOutAt)
			warnings := strings.Join(ce.Warnings(), "\n")
			assert.NotContains(t, warnings, "stays in use")
			if tc.interrupt {
				assert.Empty(t, report.Revoked)
				byName := map[string]token.NotRevokedToken{}
				for _, n := range report.NotRevoked {
					byName[n.Ref.Name] = n
				}
				require.Len(t, byName, 2)
				for _, name := range []string{r.cache.Ref.Name, r.login.Ref.Name} {
					assert.Equal(t, "cancelled", byName[name].Reason, name)
					assert.Equal(t, "prod", byName[name].Entry, name)
					assert.Equal(t, "nextLogin", byName[name].Retry, name)
				}
				assert.Contains(t, warnings, "2 token(s) recorded on prod are revoked by the next login of prod")
			} else {
				assert.ElementsMatch(t, []string{r.cache.Ref.Name, r.login.Ref.Name}, revokedNames(report.Revoked))
				assert.Empty(t, report.NotRevoked)
				assert.Contains(t, ce.Message, `failed to log out "prod"`)
			}

			rerun, err := NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			require.NoError(t, err)
			data := rerun.Data.(map[string]any)
			assert.Empty(t, data["revoked"])
			if !tc.interrupt {
				assert.Empty(t, data["notRevoked"])
				return
			}
			for _, n := range data["notRevoked"].([]token.NotRevokedToken) {
				assert.Equal(t, "prod", n.Entry, n.Ref.Name)
				assert.Equal(t, "nextLogin", n.Retry, n.Ref.Name)
			}
			assert.Len(t, data["notRevoked"], 2)
		})
	}
}

// TestALogoutWhoseFileCopyCannotBeDeletedFails asserts that a keychain
// logout whose credentials.yaml cannot be parsed - it still holds a live
// plaintext token - fails although the keychain item is gone: the exit is
// not 0, the login counts as deleted and is revoked with the cache token,
// deletedFrom names the keychain only, nothing is claimed logged out, and
// the plaintext token stays in the file and alive.
func TestALogoutWhoseFileCopyCannotBeDeletedFails(t *testing.T) {
	r := newRevokeDir(t, testrancher.Options{})
	useKeychainLogin(t, r)
	plain, err := token.AcquireLoginCredential(t.Context(), http.DefaultClient,
		token.LoginRequest{Server: r.url, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	creds := config.CredentialsFilePath(r.dir)
	require.NoError(t, os.WriteFile(creds, []byte("{{ not yaml\nprod: "+plain.Token+"\n"), 0o600))

	_, err = NewLogout(r.dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.NotEqual(t, 0, ce.ExitCode())
	assert.Contains(t, ce.Message, `failed to log out "prod"`)
	report, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Empty(t, report.LoggedOut)
	assert.Equal(t, map[string][]string{"prod": {"keychain"}}, report.DeletedFrom)
	assert.ElementsMatch(t, []string{r.cache.Ref.Name, r.login.Ref.Name}, revokedNames(report.Revoked))
	b, rerr := os.ReadFile(creds)
	require.NoError(t, rerr)
	assert.Contains(t, string(b), plain.Token, "the plaintext copy stays")
	_, alive := r.f.TokenOwner(plain.Ref.Name)
	assert.True(t, alive)
}

func mustLoad(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	return cfg
}

func revokedNames(revoked []token.RevokedToken) []string {
	var out []string
	for _, r := range revoked {
		out = append(out, r.Ref.Name)
	}
	return out
}

// TestALogoutWhoseStepWasInterruptedReportsBothLists asserts that a
// Ctrl-C stopping the login delete, with the token still stored, or
// either step of logout --all's final cleanup is reported as INTERRUPTED
// with both lists and the warnings, not a bare cancellation.
func TestALogoutWhoseStepWasInterruptedReportsBothLists(t *testing.T) {
	cancelled := errors.Join(errors.New("locking the credential-helper records"), context.Canceled)
	login := &token.LoginDeleteError{Target: "prod", Err: cancelled}
	for _, tc := range []struct {
		name string
		err  error
		d    *token.Deletion
	}{
		{"the login delete", login, &token.Deletion{LoginErr: login}},
		{"the sweep", &sweepFailure{sweep: cancelled}, &token.Deletion{}},
		{"the cache delete beside a failed sweep", &sweepFailure{cache: cancelled, sweep: errors.New("helper exploded")}, &token.Deletion{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := token.TokenRef{Kind: token.TokenKindExt, Name: "token-a"}
			rep := token.RevocationReport{
				Revoked:    []token.RevokedToken{{Ref: ref, Source: "login"}},
				NotRevoked: []token.NotRevokedToken{{Source: "login", Reason: "unproven", Retry: "manual"}},
			}
			err := logoutCmd{}.localFailure(tc.err, tc.d, rep, "rancher auth logout prod", []string{"a notice"})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeInterrupted, ce.Code)
			details, ok := ce.Details.(LogoutAllPartialFailure)
			require.True(t, ok)
			assert.Equal(t, rep.Revoked, details.Revoked)
			assert.Equal(t, rep.NotRevoked, details.NotRevoked)
			assert.Equal(t, []string{"a notice"}, ce.Warnings())
		})
	}
}
