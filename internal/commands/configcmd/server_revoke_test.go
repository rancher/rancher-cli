package configcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
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
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

// linesReader hands out one line per Read, as a person types them.
type linesReader struct{ lines []string }

func (r *linesReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.lines[0]+"\n")
	r.lines = r.lines[1:]
	return n, nil
}

// serverRevokeDir is a directory whose entry prod, the current server,
// holds a login of e2e-user the CLI created and named on the fixture
// Rancher, with one cache record of prod's own credential naming its
// token; dev, a second entry for the same server, records the same owner.
type serverRevokeDir struct {
	dir   string
	url   string
	f     *testrancher.Facade
	login token.LoginCredential
	cache token.LoginCredential
	armed atomic.Pointer[func()]
}

func newServerRevokeDir(t *testing.T) *serverRevokeDir {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	r := &serverRevokeDir{dir: t.TempDir()}
	r.f = testrancher.New(testrancher.Options{RequestHook: func(req *http.Request) {
		if req.Method != http.MethodDelete {
			return
		}
		if fn := r.armed.Swap(nil); fn != nil {
			(*fn)()
		}
	}})
	srv := httptest.NewServer(r.f)
	t.Cleanup(srv.Close)
	r.url = srv.URL
	create := func() token.LoginCredential {
		cred, err := token.AcquireLoginCredential(t.Context(), srv.Client(),
			token.LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
			ui.New(&bytes.Buffer{}, &linesReader{lines: []string{"e2e-user", "e2e-pass"}}, false, true))
		require.NoError(t, err)
		return cred
	}
	r.login, r.cache = create(), create()
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod": {URL: srv.URL, Provenance: clierr.ProvenanceDerived, TokenRef: r.login.Ref, TokenUserID: r.login.UserID, CacheBindingID: "p1"},
		"dev":  {URL: srv.URL, TokenUserID: r.login.UserID, CacheBindingID: "d1"},
	}}
	cfg.SetPath(config.FilePath(r.dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(r.dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, r.login.Token))
	b := config.CacheBindingFor(config.CanonicalDir(r.dir), cfg, "prod").WithCreatedToken(r.cache.Ref, r.cache.UserID)
	require.NoError(t, store.SetCachedCred("prod-c-1", r.cache.Token, time.Now().Add(time.Hour), "prod", config.CanonicalServerIdentity(srv.URL), "c-1", b))
	return r
}

// during runs fn once, when the first revoke reaches the server: after the
// local delete's lock span, before the span that removes the entry. The
// fixture decides what "reaches" means: newServerRevokeDir fires on the
// first DELETE request, newTLSServerRevokeDir on the first connection,
// since a revoke that fails its handshake sends no request.
func (r *serverRevokeDir) during(fn func()) { r.armed.Store(&fn) }

func (r *serverRevokeDir) edit(t *testing.T, change func(cfg *config.Config)) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	change(cfg)
	require.NoError(t, cfg.Write())
}

func (r *serverRevokeDir) deleteServer(t *testing.T) (map[string]any, error) {
	t.Helper()
	res, err := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	if err != nil {
		return nil, err
	}
	return res.Data.(map[string]any), nil
}

func (r *serverRevokeDir) entry(t *testing.T) (config.Server, bool) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	s, ok := cfg.Servers["prod"]
	return s, ok
}

// TestDeleteServerRevokesThenRemovesTheEntry asserts the main path: the
// cache token and the login are revoked, then the entry is removed and the
// current-server pointer cleared.
func TestDeleteServerRevokesThenRemovesTheEntry(t *testing.T) {
	r := newServerRevokeDir(t)
	data, err := r.deleteServer(t)
	require.NoError(t, err)
	revoked := data["revoked"].([]token.RevokedToken)
	require.Len(t, revoked, 2)
	assert.Equal(t, r.cache.Ref.Name, revoked[0].Ref.Name)
	assert.Equal(t, r.login.Ref.Name, revoked[1].Ref.Name)
	assert.Equal(t, []token.NotRevokedToken{}, data["notRevoked"])
	assert.Equal(t, true, data["currentServerCleared"])
	_, ok := r.entry(t)
	assert.False(t, ok)
	_, alive := r.f.TokenOwner(r.login.Ref.Name)
	assert.False(t, alive)
}

// TestDeleteServerLastSpanChecksTheStamp asserts the span that removes the
// entry after the revokes: a login meanwhile (pasted or not) and a URL
// change clear the stamp and make it refuse with CONFLICT, keeping the
// entry, with both lists in the details; a concurrent logout's new stamp
// still lets it remove the entry; an entry already removed is a success.
func TestDeleteServerLastSpanChecksTheStamp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(s *config.Server, cfg *config.Config)
		conflict bool
		gone     bool
	}{
		{"a login meanwhile", func(s *config.Server, _ *config.Config) {
			s.LoggedOutAt, s.TokenRef = "", &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"}
		}, true, false},
		{"a pasted login meanwhile", func(s *config.Server, _ *config.Config) {
			s.LoggedOutAt, s.Provenance = "", clierr.ProvenanceUserSupplied
		}, true, false},
		{"a URL change meanwhile", func(s *config.Server, _ *config.Config) {
			*s = config.Server{URL: "https://moved.ex", CacheBindingID: s.CacheBindingID}
		}, true, false},
		{"a concurrent logout's stamp", func(s *config.Server, _ *config.Config) { s.LoggedOutAt = "2030-01-01T00:00:00Z" }, false, true},
		{"the entry already removed", func(_ *config.Server, cfg *config.Config) { delete(cfg.Servers, "prod") }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newServerRevokeDir(t)
			r.during(func() {
				r.edit(t, func(cfg *config.Config) {
					s := cfg.Servers["prod"]
					tc.change(&s, cfg)
					if _, ok := cfg.Servers["prod"]; ok {
						cfg.Servers["prod"] = s
					}
				})
			})
			data, err := r.deleteServer(t)
			_, present := r.entry(t)
			if tc.conflict {
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				require.ErrorContains(t, err, "changed since this command started")
				details, ok := ce.Details.(token.RevocationReport)
				require.True(t, ok)
				assert.Len(t, details.Revoked, 2)
				assert.True(t, present, "the entry is kept")
				return
			}
			require.NoError(t, err)
			assert.Len(t, data["revoked"], 2)
			assert.False(t, present)
		})
	}
}

// TestDeleteServerKeepsTheRetryOfRecordsOnOtherEntries asserts that a
// server-wide record is placed on another entry for the server before the
// target, and keeps nextLogin there when its revoke fails, while a token
// left on the removed entry is manual.
func TestDeleteServerKeepsTheRetryOfRecordsOnOtherEntries(t *testing.T) {
	r := newServerRevokeDir(t)
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	wide := config.CacheBindingForServer(config.CanonicalDir(r.dir), cfg, r.url, false).WithCreatedToken(r.cache.Ref, r.cache.UserID)
	store := config.NewFileStore(r.dir)
	_, _, err = store.DeleteCachedCredsIfUnchanged(map[string]string{"prod-c-1": r.cache.Token})
	require.NoError(t, err)
	require.NoError(t, store.SetCachedCred("wide", r.cache.Token, time.Now().Add(time.Hour), "kube", config.CanonicalServerIdentity(r.url), "c-1", wide))
	r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
	r.f.SetDeleteMode(r.login.Ref.Name, "fail")
	data, err := r.deleteServer(t)
	require.NoError(t, err)
	byName := map[string]token.NotRevokedToken{}
	for _, n := range data["notRevoked"].([]token.NotRevokedToken) {
		byName[n.Ref.Name] = n
	}
	assert.Equal(t, "dev", byName[r.cache.Ref.Name].Entry)
	assert.Equal(t, "nextLogin", byName[r.cache.Ref.Name].Retry)
	assert.Empty(t, byName[r.login.Ref.Name].Entry)
	assert.Equal(t, "manual", byName[r.login.Ref.Name].Retry)
}

// TestDeleteServerRevokesNothingUnderTheConfigLock asserts no request runs
// while the config lock is held.
func TestDeleteServerRevokesNothingUnderTheConfigLock(t *testing.T) {
	r := newServerRevokeDir(t)
	var violations atomic.Int32
	var check func()
	check = func() {
		fl := flock.New(config.LockPath(r.dir))
		if ok, err := fl.TryLock(); err != nil || !ok {
			violations.Add(1)
		} else {
			_ = fl.Unlock()
		}
		r.during(check)
	}
	r.during(check)
	_, err := r.deleteServer(t)
	require.NoError(t, err)
	assert.Equal(t, int32(0), violations.Load())
}

// TestUpdateServerRecordsTheOldServersTokensAndSendsNothing asserts a URL
// change: nothing is sent to either address; prod's login, cache token and
// superseded record stay recorded on prod with the old server, otherServer
// and manual; config status lists them with the old server's recovery; and
// the next retirement of prod at its new address leaves them alone.
func TestUpdateServerRecordsTheOldServersTokensAndSendsNothing(t *testing.T) {
	r := newServerRevokeDir(t)
	r.edit(t, func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-older"}, UserID: r.login.UserID, SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	before := len(r.f.DeleteRequests())
	res, err := newUpdateServerCmd(r.dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://moved.example.com"}, Pos: []string{"prod"}}, nil)
	require.NoError(t, err)
	assert.Len(t, r.f.DeleteRequests(), before, "nothing is sent")
	data := res.Data.(map[string]any)
	assert.NotContains(t, data, "revoked")
	old := config.CanonicalServerIdentity(r.url)
	nr := data["notRevoked"].([]token.NotRevokedToken)
	require.Len(t, nr, 3)
	for _, n := range nr {
		assert.Equal(t, "otherServer", n.Reason, n.Ref.Name)
		assert.Equal(t, "prod", n.Entry, n.Ref.Name)
		assert.Equal(t, "manual", n.Retry, n.Ref.Name)
		assert.Equal(t, old, n.Server, n.Ref.Name)
	}
	s, ok := r.entry(t)
	require.True(t, ok)
	assert.Equal(t, "https://moved.example.com", s.URL)
	require.Len(t, s.SupersededTokens, 3)
	for _, rec := range s.SupersededTokens {
		assert.Equal(t, old, rec.Server, rec.Ref.Name)
	}

	status := execConfigInfo(t, r.dir)["servers"].(map[string]any)["prod"].(map[string]any)["supersededTokens"].([]map[string]any)
	require.Len(t, status, 3)
	for _, rec := range status {
		assert.Equal(t, "otherServer", rec["state"])
		assert.Contains(t, rec["recovery"].([]string)[0], "--url '"+old+"'")
	}

	rep := token.RetireSuperseded(t.Context(), r.dir, "prod", "https://moved.example.com", r.cache, s.SupersededTokens)
	assert.Empty(t, rep.Retired)
	assert.Len(t, r.f.DeleteRequests(), before, "the new address's login retires none of them")
}

// TestUpdateServerLeavesTokensOnAnotherEntryForItsLogin asserts that a
// server-wide record whose server another entry still names is recorded
// there, notAttempted, with nextLogin when that entry records its owner.
func TestUpdateServerLeavesTokensOnAnotherEntryForItsLogin(t *testing.T) {
	r := newServerRevokeDir(t)
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	wide := config.CacheBindingForServer(config.CanonicalDir(r.dir), cfg, r.url, false).WithCreatedToken(r.cache.Ref, r.cache.UserID)
	store := config.NewFileStore(r.dir)
	_, _, err = store.DeleteCachedCredsIfUnchanged(map[string]string{"prod-c-1": r.cache.Token})
	require.NoError(t, err)
	require.NoError(t, store.SetCachedCred("wide", r.cache.Token, time.Now().Add(time.Hour), "kube", config.CanonicalServerIdentity(r.url), "c-1", wide))
	res, err := newUpdateServerCmd(r.dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://moved.example.com"}, Pos: []string{"prod"}}, nil)
	require.NoError(t, err)
	var wideItem token.NotRevokedToken
	for _, n := range res.Data.(map[string]any)["notRevoked"].([]token.NotRevokedToken) {
		if n.Ref.Name == r.cache.Ref.Name {
			wideItem = n
		}
	}
	assert.Equal(t, "notAttempted", wideItem.Reason)
	assert.Equal(t, "dev", wideItem.Entry)
	assert.Equal(t, "nextLogin", wideItem.Retry)
}

// TestServerCommandsThatCannotTakeTheCacheLockDeleteNothing asserts that a
// token-cache lock delete-server or update-server's URL change cannot take
// fails it with the lock's own error before anything is written, deleted
// or revoked.
func TestServerCommandsThatCannotTakeTheCacheLockDeleteNothing(t *testing.T) {
	for name, run := range map[string]func(t *testing.T, r *serverRevokeDir) error{
		"delete-server": func(t *testing.T, r *serverRevokeDir) error {
			t.Helper()
			_, err := r.deleteServer(t)
			return err
		},
		"update-server --url": func(t *testing.T, r *serverRevokeDir) error {
			t.Helper()
			_, err := newUpdateServerCmd(r.dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://moved.example.com"}, Pos: []string{"prod"}}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newServerRevokeDir(t)
			before, err := config.Load(config.FilePath(r.dir))
			require.NoError(t, err)
			deletes := len(r.f.DeleteRequests())
			t.Cleanup(config.SetCacheLockWaitForTest(200 * time.Millisecond))
			fl := flock.New(filepath.Join(r.dir, "token-cache.lock"))
			require.NoError(t, fl.Lock())
			t.Cleanup(func() { _ = fl.Unlock() })

			err = run(t, r)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
			assert.Len(t, r.f.DeleteRequests(), deletes, "nothing is revoked")
			after, err := config.Load(config.FilePath(r.dir))
			require.NoError(t, err)
			assert.Equal(t, before.Servers, after.Servers, "nothing is written")
			tok, terr := config.NewFileStore(r.dir).GetToken(config.ServerRef{Name: "prod", URL: r.url})
			require.NoError(t, terr)
			assert.Equal(t, r.login.Token, tok)
			creds, lerr := config.NewFileStore(r.dir).ListCachedCreds()
			require.NoError(t, lerr)
			assert.Len(t, creds, 1, "the cache record stays")
		})
	}
}

// TestDeleteServerInterruptedDuringTheRevokesKeepsTheEntry asserts that a
// Ctrl-C during the revokes ends config delete-server with INTERRUPTED and
// both lists in the details, without its last span: the entry stays for a
// rerun to remove. The tokens left on the entry are manual with their
// recovery, as the rerun the message names reports them when it removes
// the entry, and the warnings carry that recovery.
func TestDeleteServerInterruptedDuringTheRevokesKeepsTheEntry(t *testing.T) {
	r := newServerRevokeDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.during(cancel)
	_, err := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeInterrupted, ce.Code)
	require.ErrorContains(t, err, "config delete-server 'prod' again")
	details := decodeRevocationDetails(t, ce.Details)
	assert.Len(t, details.Revoked, 1)
	require.Len(t, details.NotRevoked, 1)
	left := details.NotRevoked[0]
	assert.Equal(t, "cancelled", left.Reason)
	assert.Equal(t, "manual", left.Retry)
	assert.Empty(t, left.Entry)
	require.NotEmpty(t, left.Recovery)
	assert.Contains(t, strings.Join(ce.Warnings(), "\n"), "(cancelled); revoke it with:")
	_, present := r.entry(t)
	assert.True(t, present, "the entry is left in place")

	data, err := r.deleteServer(t)
	require.NoError(t, err)
	_, present = r.entry(t)
	assert.False(t, present, "the rerun removes the entry")
	raw, err := json.Marshal(data["notRevoked"])
	require.NoError(t, err)
	var again []token.NotRevokedToken
	require.NoError(t, json.Unmarshal(raw, &again))
	require.Len(t, again, 1)
	assert.Equal(t, left.Ref.Name, again[0].Ref.Name, "the rerun reports the same token")
	assert.Equal(t, "manual", again[0].Retry)
	assert.NotEmpty(t, again[0].Recovery)
}

// decodeRevocationDetails decodes a delete-server error's details as the
// JSON envelope carries them.
func decodeRevocationDetails(t *testing.T, details any) token.RevocationReport {
	t.Helper()
	raw, err := json.Marshal(details)
	require.NoError(t, err)
	var rep token.RevocationReport
	require.NoError(t, json.Unmarshal(raw, &rep))
	return rep
}

// TestDeleteServerWhoseSignalCameAfterTheLastRevokeRemovesTheEntry asserts
// that a Ctrl-C landing during the last revoke, which completes, leaves no
// token unreached: config delete-server removes the entry and returns its
// result.
func TestDeleteServerWhoseSignalCameAfterTheLastRevokeRemovesTheEntry(t *testing.T) {
	r := newServerRevokeDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.during(func() { r.during(cancel) })
	res, err := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Len(t, res.Data.(map[string]any)["revoked"], 2)
	_, present := r.entry(t)
	assert.False(t, present)
}

// TestDeleteServerSurfacesTheCacheLoosePermissionNotice asserts that the
// cache store's notice about a group- or world-readable token-cache.yaml
// reaches the command's warnings.
func TestDeleteServerSurfacesTheCacheLoosePermissionNotice(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	r := newServerRevokeDir(t)
	require.NoError(t, os.Chmod(filepath.Join(r.dir, "token-cache.yaml"), 0o644))
	res, err := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "group/world-readable")
}

// TestDeleteServerReportsEntryAndRetryOnEachPath asserts where delete-server
// reports a token recorded on its entry, decoded from the envelope: with no
// entry and a manual retry when the entry is removed or already gone, and
// when an interrupt leaves it with no login for the named rerun to remove;
// with the entry and the owner rule's retry when a failed login delete
// restores the login, a login in between makes the last span refuse, or
// the removal could not be written.
func TestDeleteServerReportsEntryAndRetryOnEachPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, r *serverRevokeDir, cmd *configDeleteServerCmd, cancel context.CancelFunc)
		code       string
		token      func(r *serverRevokeDir) string
		wantEntry  string
		wantRetry  string
		wantReason string
		// oldServer asks that the recovery log in to the URL the entry had
		// when the command started.
		oldServer bool
	}{
		{name: "the entry removed", code: "",
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "", wantRetry: "manual", wantReason: "failed"},
		{name: "interrupted", code: clierr.CodeInterrupted,
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, cancel context.CancelFunc) {
				t.Helper()
				r.during(cancel)
			},
			token: func(r *serverRevokeDir) string { return r.login.Ref.Name }, wantEntry: "", wantRetry: "manual", wantReason: "cancelled"},
		{name: "a failed login delete", code: clierr.CodeGeneric,
			setup: func(t *testing.T, r *serverRevokeDir, cmd *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, error) {
					return nil, nil, errors.New("the keychain refused")
				}
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "nextLogin", wantReason: "failed"},
		{name: "interrupted in the login delete, the token still stored", code: clierr.CodeInterrupted,
			setup: func(t *testing.T, _ *serverRevokeDir, cmd *configDeleteServerCmd, cancel context.CancelFunc) {
				t.Helper()
				cmd.delete = func(ctx context.Context, _ string, _ config.ServerRef, _ string) ([]string, []string, error) {
					cancel()
					return nil, nil, ctx.Err()
				}
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "nextLogin", wantReason: "cancelled"},
		{name: "a failed delete after the login token went", code: clierr.CodeGeneric,
			setup: func(t *testing.T, r *serverRevokeDir, cmd *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				cmd.delete = func(ctx context.Context, dir string, ref config.ServerRef, policy string) ([]string, []string, error) {
					from, ns, err := config.DeleteLoginToken(ctx, dir, ref, policy)
					require.NoError(t, err)
					return from, ns, errors.New("removing the credential identity record: lock wait timed out")
				}
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "nextLogin", wantReason: "failed"},
		{name: "the entry already gone", code: "",
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				r.during(func() { r.edit(t, func(cfg *config.Config) { delete(cfg.Servers, "prod") }) })
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "", wantRetry: "manual", wantReason: "failed"},
		{name: "the removal not written", code: clierr.CodeGeneric,
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				if goruntime.GOOS == "windows" {
					t.Skip("a read-only directory mode does not stop a write on Windows")
				}
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				t.Cleanup(func() { _ = os.Chmod(r.dir, 0o700) })
				r.during(func() { require.NoError(t, os.Chmod(r.dir, 0o500)) })
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "nextLogin", wantReason: "failed"},
		{name: "a login in between", code: clierr.CodeConflict,
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				r.during(func() {
					r.edit(t, func(cfg *config.Config) {
						s := cfg.Servers["prod"]
						s.LoggedOutAt, s.TokenRef = "", &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"}
						cfg.Servers["prod"] = s
					})
				})
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "nextLogin", wantReason: "failed"},
		{name: "a URL change in between", code: clierr.CodeConflict,
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				r.during(func() {
					_, err := newUpdateServerCmd(r.dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://prod2.ex"}, Pos: []string{"prod"}}, nil)
					require.NoError(t, err)
				})
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "manual", wantReason: "failed", oldServer: true},
		{name: "a URL change after the record write", code: clierr.CodeConflict,
			setup: func(t *testing.T, r *serverRevokeDir, _ *configDeleteServerCmd, _ context.CancelFunc) {
				t.Helper()
				r.f.SetDeleteMode(r.cache.Ref.Name, "fail")
				var armed atomic.Bool
				r.during(func() { armed.Store(true) })
				// The first config write after the revokes is the one that
				// records their results; the change lands right after it,
				// so only the span that removes the entry sees it.
				t.Cleanup(config.SetAfterWriteHookForTest(func(what string) {
					if what != "config.yaml" || !armed.CompareAndSwap(true, false) {
						return
					}
					r.edit(t, func(cfg *config.Config) {
						s := cfg.Servers["prod"]
						s.URL, s.LoggedOutAt, s.TokenUserID = "https://prod2.ex", "", ""
						cfg.Servers["prod"] = s
					})
				}))
			},
			token: func(r *serverRevokeDir) string { return r.cache.Ref.Name }, wantEntry: "prod", wantRetry: "manual", wantReason: "failed", oldServer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newServerRevokeDir(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd := NewConfigDeleteServer(r.dir).(*configDeleteServerCmd)
			tc.setup(t, r, cmd, cancel)
			res, err := cmd.Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			var items []token.NotRevokedToken
			if tc.code == "" {
				require.NoError(t, err)
				raw, merr := json.Marshal(res.Data.(map[string]any)["notRevoked"])
				require.NoError(t, merr)
				require.NoError(t, json.Unmarshal(raw, &items))
			} else {
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce)
				assert.Equal(t, tc.code, ce.Code)
				items = decodeRevocationDetails(t, ce.Details).NotRevoked
			}
			want := tc.token(r)
			var got *token.NotRevokedToken
			for i := range items {
				if items[i].Ref != nil && items[i].Ref.Name == want {
					got = &items[i]
				}
			}
			require.NotNil(t, got, "%s is listed: %+v", want, items)
			assert.Equal(t, tc.wantReason, got.Reason)
			assert.Equal(t, tc.wantEntry, got.Entry)
			assert.Equal(t, tc.wantRetry, got.Retry)
			if tc.wantRetry == "manual" {
				assert.NotEmpty(t, got.Recovery)
			}
			if tc.oldServer {
				require.NotEmpty(t, got.Recovery)
				assert.Contains(t, got.Recovery[0], "--url "+clierr.ShellQuote(r.url))
				assert.Equal(t, config.CanonicalServerIdentity(r.url), got.Server)
			}
		})
	}
}

// TestDeleteServerWhoseLoginDeleteFailedAfterTheTokenWentCountsItDeleted
// asserts that a keychain login delete that failed after the item was
// gone - a Ctrl-C or an identity-lock timeout while the identity record
// is removed - counts the login as deleted while the failure stays the
// command's, the entry kept with no TokenRef, and the rerun agrees with
// the report. After a Ctrl-C the command ends INTERRUPTED with both tokens
// cancelled, reported with no entry and a manual retry since the rerun the
// message names removes the entry, and the rerun does so and reports them
// the same way. After a timeout it ends LOCK_TIMEOUT with both revoked,
// and the rerun removes the entry with nothing left to report.
func TestDeleteServerWhoseLoginDeleteFailedAfterTheTokenWentCountsItDeleted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
		code      string
	}{{"a Ctrl-C", true, clierr.CodeInterrupted}, {"an identity-lock timeout", false, clierr.CodeLockTimeout}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newServerRevokeDir(t)
			r.edit(t, func(cfg *config.Config) { cfg.CredentialStore = "keychain" })
			keychain, err := config.OpenStore("keychain", r.dir)
			require.NoError(t, err)
			require.NoError(t, keychain.SetToken(config.ServerRef{Name: "prod", URL: r.url}, r.login.Token))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			t.Cleanup(config.SetLockWaitTimeoutForTest(300 * time.Millisecond))
			fl := flock.New(filepath.Join(r.dir, "credstore-id.lock"))
			config.SetKeyringObserverForTest(func(op string) {
				switch {
				case op != "delete":
				case tc.interrupt:
					cancel()
				default:
					_ = fl.Lock()
				}
			})
			release := func() {
				config.SetKeyringObserverForTest(nil)
				_ = fl.Unlock()
			}
			t.Cleanup(release)

			_, err = NewConfigDeleteServer(r.dir).(*configDeleteServerCmd).Execute(ctx, cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
			release()
			names := []string{r.cache.Ref.Name, r.login.Ref.Name}
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.code, ce.Code)
			assert.NotContains(t, strings.Join(ce.Warnings(), "\n"), "stays in use")
			first := decodeRevocationDetails(t, ce.Details)
			e, ok := r.entry(t)
			require.True(t, ok, "the entry stays")
			assert.Nil(t, e.TokenRef, "the TokenRef is not put back")
			if !tc.interrupt {
				assert.Contains(t, ce.Message, `failed to delete credentials for "prod"`)
				var got []string
				for _, rv := range first.Revoked {
					got = append(got, rv.Ref.Name)
				}
				assert.ElementsMatch(t, names, got)
				assert.Empty(t, first.NotRevoked)
				data, err := r.deleteServer(t)
				require.NoError(t, err, "the rerun removes the entry")
				assert.Empty(t, data["revoked"])
				assert.Empty(t, data["notRevoked"])
				_, ok = r.entry(t)
				assert.False(t, ok)
				return
			}
			assert.Contains(t, ce.Message, "stopped while deleting the credentials of 'prod'")
			assert.Empty(t, first.Revoked)
			check := func(items []token.NotRevokedToken, reason string) {
				t.Helper()
				byName := map[string]token.NotRevokedToken{}
				for _, n := range items {
					byName[n.Ref.Name] = n
				}
				require.Len(t, byName, 2)
				for _, name := range names {
					assert.Equal(t, reason, byName[name].Reason, name)
					assert.Empty(t, byName[name].Entry, name)
					assert.Equal(t, "manual", byName[name].Retry, name)
					assert.NotEmpty(t, byName[name].Recovery, name)
				}
			}
			check(first.NotRevoked, "cancelled")

			data, err := r.deleteServer(t)
			require.NoError(t, err)
			assert.Empty(t, data["revoked"])
			check(data["notRevoked"].([]token.NotRevokedToken), "noBearer")
			_, ok = r.entry(t)
			assert.False(t, ok, "the rerun removes the entry")
		})
	}
}

// TestDeleteServerListsTheBackendsInNameOrder asserts that the backends a
// login was deleted from are listed sorted by name, whatever order the
// delete reported them in.
func TestDeleteServerListsTheBackendsInNameOrder(t *testing.T) {
	r := newServerRevokeDir(t)
	r.edit(t, func(cfg *config.Config) { cfg.CredentialStore = "keychain" })
	keychain, err := config.OpenStore("keychain", r.dir)
	require.NoError(t, err)
	require.NoError(t, keychain.SetToken(config.ServerRef{Name: "prod", URL: r.url}, r.login.Token))
	data, err := r.deleteServer(t)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"backend": {"file", "keychain"}}, data["deletedFrom"])
}
