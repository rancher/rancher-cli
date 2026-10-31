package token

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

var revocationUsers = []testrancher.User{
	{Name: "alice", Password: "pw", UserID: "user-alice"},
	{Name: "bob", Password: "pw", UserID: "user-bob"},
}

// revFixture is a config directory with a file store and one fixture
// Rancher, the server every entry names unless a test adds another.
type revFixture struct {
	t     *testing.T
	f     *testrancher.Facade
	srv   *httptest.Server
	url   string
	dir   string
	store config.CredStore
}

func newRevFixture(t *testing.T, opts testrancher.Options) *revFixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	opts.Users = revocationUsers
	f := testrancher.New(opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	x := &revFixture{t: t, f: f, srv: srv, url: srv.URL, dir: t.TempDir()}
	x.store = config.NewFileStore(x.dir)
	cfg := &config.Config{Servers: map[string]config.Server{}, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(x.dir))
	require.NoError(t, cfg.Write())
	return x
}

// token creates a live ext token of user on the fixture Rancher.
func (x *revFixture) token(user string) LoginCredential {
	x.t.Helper()
	return tokenOn(x.t, x.srv, user)
}

func tokenOn(t *testing.T, srv *httptest.Server, user string) LoginCredential {
	t.Helper()
	cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader(user, "pw"), false, true))
	require.NoError(t, err)
	require.NotNil(t, cred.Ref)
	return cred
}

// edit loads config.yaml, applies change and writes it back.
func (x *revFixture) edit(change func(cfg *config.Config)) {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	change(cfg)
	require.NoError(x.t, cfg.Write())
}

func (x *revFixture) load() *config.Config {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	return cfg
}

// entry writes a server entry with a fresh cache binding id.
func (x *revFixture) entry(name string, s config.Server) {
	x.t.Helper()
	if s.URL == "" {
		s.URL = x.url
	}
	x.edit(func(cfg *config.Config) {
		require.NoError(x.t, config.AdvanceCacheBinding(&s))
		cfg.Servers[name] = s
	})
}

// login gives entry a login of user the CLI created and named, as auth
// login stores it, and returns it.
func (x *revFixture) login(name, user string) LoginCredential {
	x.t.Helper()
	cred := x.token(user)
	x.entry(name, config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: cred.Ref, TokenUserID: cred.UserID, CredentialExpiresAt: cred.ExpiresAt})
	require.NoError(x.t, x.store.SetToken(config.ServerRef{Name: name, URL: x.url}, cred.Token))
	return cred
}

// cache writes a cache record of key under b holding token, naming it
// when named is set.
func (x *revFixture) cache(key string, cred LoginCredential, b config.CacheBinding, named bool) {
	x.t.Helper()
	if named {
		b = b.WithCreatedToken(cred.Ref, cred.UserID)
	}
	require.NoError(x.t, x.store.SetCachedCred(key, cred.Token, time.Now().Add(time.Hour), key, config.CanonicalServerIdentity(x.url), "c-1", b))
}

// own is the binding of a record made from entry's own credential.
func (x *revFixture) own(entry string) config.CacheBinding {
	return config.CacheBindingFor(config.CanonicalDir(x.dir), x.load(), entry)
}

// serverWide is the binding of a record of a call that identified no
// entry for the fixture's server.
func (x *revFixture) serverWide() config.CacheBinding {
	return config.CacheBindingForServer(config.CanonicalDir(x.dir), x.load(), x.url, false)
}

// cacheKeys lists the keys the cache holds.
func (x *revFixture) cacheKeys() []string {
	x.t.Helper()
	creds, err := x.store.ListCachedCreds()
	require.NoError(x.t, err)
	var keys []string
	for _, c := range creds {
		keys = append(keys, c.Key)
	}
	return keys
}

func (x *revFixture) alive(cred LoginCredential) bool {
	_, ok := x.f.TokenOwner(cred.Ref.Name)
	return ok
}

// deleteLogin is the commands' login delete: every backend.
func (x *revFixture) deleteLogin(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
	return config.DeleteLoginToken(ctx, x.dir, ref, "file")
}

// req is a request for cmd over targets with the production login delete.
func (x *revFixture) req(cmd DeleteCommand, targets ...string) LocalDelete {
	return LocalDelete{Dir: x.dir, Command: cmd, Targets: targets, Cache: x.store, DeleteLogin: x.deleteLogin, Rerun: clierr.Rancher + " auth logout"}
}

// localDelete runs the local delete under the config lock, as a command does.
func (x *revFixture) localDelete(req LocalDelete) (*Deletion, error) {
	x.t.Helper()
	var d *Deletion
	var derr error
	require.NoError(x.t, config.WithLock(x.t.Context(), x.dir, func() error {
		cfg, err := config.Load(config.FilePath(x.dir))
		if err != nil {
			return err
		}
		d, derr = DeleteAndRecord(x.t.Context(), cfg, req)
		return nil
	}))
	return d, derr
}

// run is the local delete, the revokes and the record write after them.
func (x *revFixture) run(req LocalDelete) (*Deletion, RevocationReport, error) {
	x.t.Helper()
	d, err := x.localDelete(req)
	if d == nil {
		return nil, RevocationReport{}, err
	}
	d.RevokeTokens(x.t.Context())
	require.Empty(x.t, d.Settle(x.t.Context()))
	return d, d.Report(), err
}

// notRevoked finds the item for the token named name.
func notRevoked(t *testing.T, rep RevocationReport, name string) NotRevokedToken {
	t.Helper()
	for _, n := range rep.NotRevoked {
		if n.Ref != nil && n.Ref.Name == name {
			return n
		}
	}
	require.Failf(t, "not listed", "%s is not in notRevoked: %+v", name, rep.NotRevoked)
	return NotRevokedToken{}
}

func revokedNames(rep RevocationReport) []string {
	var out []string
	for _, r := range rep.Revoked {
		out = append(out, r.Ref.Name)
	}
	return out
}

// configLockHeld reports whether dir's config lock is held by anyone.
func configLockHeld(dir string) bool {
	fl := flock.New(config.LockPath(dir))
	ok, err := fl.TryLock()
	if err != nil || !ok {
		return true
	}
	_ = fl.Unlock()
	return false
}
