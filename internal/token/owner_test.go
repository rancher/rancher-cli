package token

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// ownerFixture is a directory and a fixture Rancher for the owner checks
// of calls with --user-id: alice, bob, svc and carol are its users, with
// the ids user-alice and so on, and it answers SelfSubjectReviews with
// the bearer's owner. Its logins answer with the v3 body Rancher sends,
// which names no owner. Every request it receives is kept.
type ownerFixture struct {
	t     *testing.T
	f     *testrancher.Facade
	srv   *httptest.Server
	dir   string
	store *readRecorder

	mu       sync.Mutex
	requests []seenRequest
	// hook, when set, sees every request the fixture Rancher receives.
	hook func(r *http.Request)
}

// readRecorder is a store that records the entries whose stored login was
// read through it.
type readRecorder struct {
	config.CredStore
	mu   sync.Mutex
	read []string
}

func (s *readRecorder) GetToken(ref config.ServerRef) (string, error) {
	s.mu.Lock()
	s.read = append(s.read, ref.Name)
	s.mu.Unlock()
	return s.CredStore.GetToken(ref)
}

func (s *readRecorder) reads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.read...)
}

// seenRequest is one request the fixture Rancher received.
type seenRequest struct{ method, path, bearer string }

func newOwnerFixture(t *testing.T, reuse bool, opts testrancher.Options) *ownerFixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvCredentialStore, "")
	x := &ownerFixture{t: t}
	opts.Users = entryTableUsers()
	opts.SelfSubjectReviews = true
	opts.RequestHook = func(r *http.Request) {
		x.mu.Lock()
		x.requests = append(x.requests, seenRequest{r.Method, r.URL.Path, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")})
		hook := x.hook
		x.mu.Unlock()
		if hook != nil {
			hook(r)
		}
	}
	x.f = newFacadeWithClusters(&opts)
	x.srv = httptest.NewServer(x.f)
	t.Cleanup(x.srv.Close)
	x.dir = t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: reuse, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(x.dir))
	require.NoError(t, cfg.Write())
	x.store = &readRecorder{CredStore: config.NewFileStore(x.dir)}
	return x
}

func (x *ownerFixture) serverID() string { return config.CanonicalServerIdentity(x.srv.URL) }

// pluginEntry is the name of the URL's plugin login entry for a call
// without --user-id.
func (x *ownerFixture) pluginEntry() string { return config.ExecLoginEntryName(x.serverID()) }

// pluginEntryOf is the name of the plugin login entry the plugin creates
// for a call with --user-id userID.
func (x *ownerFixture) pluginEntryOf(userID string) string {
	return config.ExecLoginEntryNameFor(x.serverID(), userID, "")
}

// entry writes a server entry at the fixture's URL recording owner as its
// login's owner, with user's login stored when user is set.
func (x *ownerFixture) entry(name, user, owner string, plugin bool) {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	s := cfg.Servers[name]
	s.URL, s.TokenUserID = x.srv.URL, owner
	if plugin {
		s.Provenance = clierr.ProvenanceExecLogin
	}
	require.NoError(x.t, config.AdvanceCacheBinding(&s))
	cfg.Servers[name] = s
	require.NoError(x.t, cfg.Write())
	if user != "" {
		require.NoError(x.t, x.store.SetToken(config.ServerRef{Name: name, URL: x.srv.URL}, acquireFor(x.t, x.srv, user)))
	}
	x.mu.Lock()
	x.requests = nil
	x.mu.Unlock()
	x.store.mu.Lock()
	x.store.read = nil
	x.store.mu.Unlock()
}

// stored is the login entry name holds, and the entry itself.
func (x *ownerFixture) stored(name string) (string, config.Server) {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	tok, err := x.store.GetToken(config.ServerRef{Name: name, URL: x.srv.URL})
	if errors.Is(err, config.ErrNoToken) {
		tok, err = "", nil
	}
	require.NoError(x.t, err)
	return tok, cfg.Servers[name]
}

// seen is a copy of the requests received so far.
func (x *ownerFixture) seen() []seenRequest {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]seenRequest(nil), x.requests...)
}

// usedBearer reports whether any request was made with bearer.
func (x *ownerFixture) usedBearer(bearer string) bool {
	for _, r := range x.seen() {
		if bearer != "" && r.bearer == bearer {
			return true
		}
	}
	return false
}

// owner reviews counts the SelfSubjectReviews the fixture answered, by path.
func (x *ownerFixture) ownerReviews() []string {
	var out []string
	for _, r := range x.seen() {
		if strings.HasSuffix(r.path, selfSubjectReviewPath) {
			out = append(out, r.path)
		}
	}
	return out
}

// call runs the exec plugin for --name name and --user-id userID against
// the fixture: with a terminal where signIn signs in when asked, or with
// none when signIn is empty. It returns the owner of the token served.
func (x *ownerFixture) call(name, userID, signIn string, notices *bytes.Buffer) (string, error) {
	x.t.Helper()
	return x.callIn("c-test", name, userID, signIn, notices)
}

// callIn is call for --cluster cluster, which may be empty, and --name
// name, which may be empty; neither is passed when empty.
func (x *ownerFixture) callIn(cluster, name, userID, signIn string, notices *bytes.Buffer) (string, error) {
	x.t.Helper()
	if notices == nil {
		notices = &bytes.Buffer{}
	}
	p := ui.New(notices, strings.NewReader(""), false, false)
	if signIn != "" {
		p = ui.New(notices, newLineReader(signIn, "pw-"+signIn), false, true)
	}
	s := map[string]string{"server": x.srv.URL}
	if userID == "" {
		s["auth-provider"] = "localProvider"
	}
	if cluster != "" {
		s["cluster"] = cluster
	}
	if name != "" {
		s["name"] = name
	}
	if userID != "" {
		s["user-id"] = userID
	}
	cmd := NewTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(x.t.Context(), tin{s: s}, p)
	if err != nil {
		return "", err
	}
	cred, ok := res.Data.(*execCredential)
	require.True(x.t, ok)
	owner, found := x.f.BearerOwner(cred.Status.Token)
	require.True(x.t, found, "the served token is one the server issued")
	return owner, nil
}

// cached reports whether a record is cached under the slot of name and
// userID.
func (x *ownerFixture) cached(name, userID string) bool {
	x.t.Helper()
	return x.cachedIn("c-test", name, userID)
}

// cachedIn is cached for the slot of cluster.
func (x *ownerFixture) cachedIn(cluster, name, userID string) bool {
	x.t.Helper()
	_, _, err := x.store.GetCachedCred(config.CacheKeyFor(name, x.serverID(), cluster, userID, ""), anchorFor(x.dir))
	return err == nil
}

// revoked counts the deletes of tokens created after the fixture's last
// entry was written: DELETE requests and logout requests.
func (x *ownerFixture) revoked() int {
	n := 0
	for _, r := range x.seen() {
		if r.method == http.MethodDelete || r.path == LogoutPath {
			n++
		}
	}
	return n
}

// refusal is err's message with every remedy written for the default
// directory.
func refusal(t *testing.T, err error) (*clierr.Error, string) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a typed error, got %v", err)
	return ce, clierr.ExpandRemedies(ce.Message, "")
}

// TestUserIDEntryOwnerRefusesBeforeAnyRequest asserts the entry the --name
// identifies refuses a call with --user-id before any request when its
// login records another owner: a user entry, and a plugin login entry
// holding a login or none. A user entry of another user is no candidate of
// a call that names none, and a user entry with no login keeps its
// recorded owner out of it: the call falls back.
func TestUserIDEntryOwnerRefusesBeforeAnyRequest(t *testing.T) {
	t.Run("identified user entry", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "user-bob", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "server entry 'prod' holds the login of Rancher user 'user-bob', and this exec entry names user 'user-alice' with --user-id; "+
			"pipe an API token of 'user-alice' to: rancher auth login 'prod' --token-stdin; or regenerate the kubeconfig from an entry that holds the login of 'user-alice' with: "+
			"rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-test'; selecting an entry sets the directory's current server, which is unset now", msg)
		assert.Equal(t, "rancher auth login 'prod' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
		assert.Empty(t, x.seen(), "no request is made with the other user's login")
	})
	t.Run("identified user entry, with a current server", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "user-bob", false)
		cfg, err := config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		cfg.CurrentServer = "prod"
		require.NoError(t, cfg.Write())
		_, err = x.call("prod-c-test", "user-alice", "", nil)
		_, msg := refusal(t, err)
		assert.True(t, strings.HasSuffix(msg, "with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-test'; "+
			"selecting an entry switches the directory's current server; to switch back, run: rancher config use-server 'prod'"), "the second route says how to go back to the current server: %s", msg)
	})
	t.Run("a user entry of another user is no candidate of a call that names none", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "user-bob", false)
		bobLogin, _ := x.stored("prod")
		x.forgetReads()
		_, err := x.call("kube-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "interactive login required for auth config local; this call has no terminal to answer it"), msg)
		assert.False(t, x.usedBearer(bobLogin), "no request is made with the other user's login")
		assert.NotContains(t, x.store.reads(), "prod", "and it is not read")
	})
	for _, holds := range []bool{true, false} {
		t.Run("identified plugin login entry", func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			user := ""
			if holds {
				user = "bob"
			}
			x.entry(x.pluginEntry(), user, "user-bob", true)
			before, beforeEntry := x.stored(x.pluginEntry())
			_, err := x.call(x.pluginEntry()+"-c-test", "user-alice", "alice", nil)
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			state := "holds the login of Rancher user 'user-bob'"
			if !holds {
				state = "was last logged in as Rancher user 'user-bob'"
			}
			assert.Equal(t, "server entry '"+x.pluginEntry()+"', the exec plugin's own login entry, "+state+", and this exec entry names user 'user-alice' with --user-id; "+
				"remove '"+x.pluginEntry()+"' with: rancher auth logout '"+x.pluginEntry()+"'; then rancher config delete-server '"+x.pluginEntry()+"'; "+
				"<name> stands for a name of your choice; the next call signs in as 'user-alice' in a terminal; or it uses a login piped to: rancher auth login <name> --url '"+x.serverID()+"' --token-stdin; "+
				"or regenerate the kubeconfig from an entry that holds the login of 'user-alice' with: rancher config use-server <entry>; "+
				"then rancher kubeconfig --cluster 'c-test'; selecting an entry sets the directory's current server, which is unset now", msg)
			assert.Empty(t, ce.Hint, "removing an entry is no hint")
			assert.Empty(t, x.seen(), "no request, and no sign-in")
			after, afterEntry := x.stored(x.pluginEntry())
			assert.Equal(t, before, after, "the other user's login is not written over")
			assert.Equal(t, beforeEntry, afterEntry)
		})
	}
	t.Run("a user entry with no login falls back", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "user-bob", false)
		x.entry(x.pluginEntryOf("user-alice"), "alice", "user-alice", true)
		got, err := x.call("prod-c-test", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got, "the owner a logged-out entry keeps plays no part")
	})
}

// TestUserIDCreatedTokenOwnerIsChecked asserts the token created from a
// stored login is handed kubectl only when the create response names the
// call's user: an entry recording that user or none passes, and is
// cached; another owner or none in the response revokes the token,
// caches nothing and names the login to fix, for the identified entry's
// own login, the plugin login entry it fell back to, the plugin login
// entry the --name identifies, and the entry a call that names none chose
// by its recorded owner, which a failed login can leave beside another
// user's credential.
func TestUserIDCreatedTokenOwnerIsChecked(t *testing.T) {
	for _, recorded := range []string{"user-alice", ""} {
		t.Run("passes, recorded "+recorded, func(t *testing.T) {
			x := newOwnerFixture(t, false, testrancher.Options{})
			x.entry("prod", "alice", recorded, false)
			got, err := x.call("prod-c-test", "user-alice", "", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-alice", got)
			assert.True(t, x.cached("prod-c-test", "user-alice"))
			assert.Empty(t, x.ownerReviews(), "the create response names the owner; no review is asked")
		})
	}
	t.Run("own login of another user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "the stored login of server entry 'prod' belongs to Rancher user 'user-bob', not to 'user-alice', which this exec entry names with --user-id; "+
			"the token created from it was revoked; pipe an API token of 'user-alice' to: rancher auth login 'prod' --token-stdin", msg)
		assert.Equal(t, "rancher auth login 'prod' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
		assert.Equal(t, 1, x.revoked(), "the created token is revoked")
		assert.False(t, x.cached("prod-c-test", "user-alice"), "nothing is cached")
	})
	t.Run("no owner in the response", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{OmitSpecOnCreate: true})
		x.entry("prod", "alice", "user-alice", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.Equal(t, "the token create response for server entry 'prod' named no owner; it cannot be checked against --user-id 'user-alice'; the token was revoked", msg)
		assert.Equal(t, 1, x.revoked())
		assert.False(t, x.cached("prod-c-test", "user-alice"))
	})
	removal := func(x *ownerFixture, entry string) string {
		return "remove '" + entry + "' with: rancher auth logout '" + entry + "'; then rancher config delete-server '" + entry + "'; " +
			"<name> stands for a name of your choice; the next call signs in as 'user-alice' in a terminal; or it uses a login piped to: rancher auth login <name> --url '" + x.serverID() +
			"' --token-stdin"
	}
	t.Run("the fallback's login of another user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		x.entry(x.pluginEntryOf("user-alice"), "bob", "user-alice", true)
		before, beforeEntry := x.stored(x.pluginEntryOf("user-alice"))
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, "the stored login of server entry '"+x.pluginEntryOf("user-alice")+"' belongs to Rancher user 'user-bob', "+
			"not to 'user-alice', which this exec entry names with --user-id; the token created from it was revoked; "+removal(x, x.pluginEntryOf("user-alice")), msg)
		assert.Empty(t, ce.Hint)
		after, afterEntry := x.stored(x.pluginEntryOf("user-alice"))
		assert.Equal(t, before, after, "the plugin login entry is not touched")
		assert.Equal(t, beforeEntry, afterEntry)
		assert.False(t, x.cached("prod-c-test", "user-alice"))
	})
	t.Run("the identified plugin login entry's login of another user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry(x.pluginEntry(), "bob", "", true)
		_, err := x.call(x.pluginEntry()+"-c-test", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, "the stored login of server entry '"+x.pluginEntry()+"' belongs to Rancher user 'user-bob', not to 'user-alice', which this exec entry names with --user-id; "+
			"the token created from it was revoked; "+removal(x, x.pluginEntry()), msg)
		assert.Empty(t, ce.Hint)
		assert.Equal(t, 1, x.revoked())
	})
	t.Run("the chosen plugin login entry's login of another user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry(x.pluginEntryOf("user-alice"), "bob", "user-alice", true)
		_, err := x.call("kube-c-test", "user-alice", "", nil)
		_, msg := refusal(t, err)
		assert.True(t, strings.HasPrefix(msg, "the stored login of server entry '"+x.pluginEntryOf("user-alice")+"' belongs to Rancher user 'user-bob'"), msg)
		assert.False(t, x.cached("kube-c-test", "user-alice"))
	})
	t.Run("the chosen plugin login entry's login of the user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry(x.pluginEntryOf("user-alice"), "alice", "user-alice", true)
		got, err := x.call("kube-c-test", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
	})
}

// TestUserIDFailsClosedWithoutExtTokens asserts a call with --user-id on
// a Rancher without ext.cattle.io is refused rather than handed the stored
// login, whose owner no response names; a call without it keeps the
// stored login fallback.
func TestUserIDFailsClosedWithoutExtTokens(t *testing.T) {
	x := newOwnerFixture(t, false, testrancher.Options{Legacy: true})
	x.entry("prod", "alice", "user-alice", false)
	_, err := x.call("prod-c-test", "user-alice", "", nil)
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, "this Rancher does not serve ext.cattle.io; no token create reports whose login server entry 'prod' holds, and --user-id 'user-alice' cannot be checked; Rancher 2.14 or later is required", msg)
	assert.False(t, x.cached("prod-c-test", "user-alice"))

	got, err := x.call("prod-c-test", "", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", got, "without --user-id the stored login is served as before")
}

// TestUserIDNeverReadsOrWritesAnotherUsersPluginLoginEntry asserts a call
// with --user-id never reads or writes the plugin login entry of another
// user: for a user entry with no login and for a call that names none, it
// signs in, makes its own plugin login entry, checks the owner from the
// create responses and caches under the name and the user id; without a
// terminal it names the login of the entry and nothing about the other
// user's entry. A change of the other user's entry leaves its record
// served, and one of its own ends it.
func TestUserIDNeverReadsOrWritesAnotherUsersPluginLoginEntry(t *testing.T) {
	for _, name := range []string{"prod-c-test", "kube-c-test"} {
		t.Run(name, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			if name == "prod-c-test" {
				x.entry("prod", "", "", false)
			}
			x.entry(x.pluginEntry(), "bob", "user-bob", true)
			pluginLogin, pluginBefore := x.stored(x.pluginEntry())
			readsBefore := len(x.store.reads())

			got, err := x.call(name, "user-carol", "carol", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got)
			assert.NotContains(t, x.store.reads()[readsBefore:], x.pluginEntry(), "the other user's login is not even read")
			assert.Empty(t, x.ownerReviews(), "the create responses name the owner, so no owner request is made")
			assert.False(t, x.usedBearer(pluginLogin), "no request is made with the other user's login")
			after, pluginAfter := x.stored(x.pluginEntry())
			assert.Equal(t, pluginLogin, after, "the other user's entry is not written")
			assert.Equal(t, pluginBefore, pluginAfter)
			own, ownEntry := x.stored(x.pluginEntryOf("user-carol"))
			assert.NotEmpty(t, own, "the call's own plugin login entry is logged in")
			assert.Equal(t, "user-carol", ownEntry.TokenUserID)
			assert.True(t, x.cached(name, "user-carol"), "the token is cached under the name and the user id")

			_, err = x.call(name, "user-carol", "", nil)
			require.NoError(t, err, "the next call is served from the cache")

			x.entry(x.pluginEntry(), "bob", "user-bob", true)
			assert.True(t, x.cached(name, "user-carol"), "another user's entry changing does not end the record")
			cfg, lerr := config.Load(config.FilePath(x.dir))
			require.NoError(t, lerr)
			s := cfg.Servers[x.pluginEntryOf("user-carol")]
			require.NoError(t, config.AdvanceCacheBinding(&s))
			cfg.Servers[x.pluginEntryOf("user-carol")] = s
			require.NoError(t, cfg.Write())
			assert.False(t, x.cached(name, "user-carol"), "a change of the user's own entry does")
		})
	}
	t.Run("without a terminal", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		x.entry(x.pluginEntry(), "bob", "user-bob", true)
		_, err := x.call("prod-c-test", "user-carol", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'user-carol' to: "+
			"rancher auth login 'prod' --url '"+x.serverID()+"' --token-stdin; or run kubectl once in a terminal and sign in as 'user-carol'", msg)
		assert.Equal(t, "rancher auth login 'prod' --url '"+x.serverID()+"' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
	})
}

// TestUserIDSignInOwnerIsChecked asserts the cluster token an interactive
// sign-in creates for a call with --user-id is handed kubectl only when its
// create response names the call's user, with no request to ask; another
// user revokes it and caches nothing. A create response that names no owner
// (the Norman create of a Rancher without ext tokens) is checked with one
// owner request, and a failed request revokes the token too.
func TestUserIDSignInOwnerIsChecked(t *testing.T) {
	t.Run("the user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		got, err := x.call("prod-c-test", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		assert.Empty(t, x.ownerReviews(), "the ext create names the owner")
		assert.True(t, x.cached("prod-c-test", "user-carol"))
	})
	t.Run("another user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "the sign-in was made as Rancher user 'user-carol', and this exec entry names user 'user-alice' with --user-id; the token it created was revoked; sign in as 'user-alice'", msg)
		assert.Empty(t, x.ownerReviews())
		assert.Equal(t, 2, x.revoked(), "the login token's logout, then the cluster token's own delete")
		assert.False(t, x.cached("prod-c-test", "user-alice"))
	})
	t.Run("another user, the token cannot be revoked", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{ClusterDeleteMode: "fail"})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "the token it created could not be revoked (500 Internal Server Error); user 'user-carol' can delete it from a directory signed in as user 'user-carol'; run: rancher delete token ")
		assert.Contains(t, msg, " --apply --yes")
		assert.False(t, x.cached("prod-c-test", "user-alice"))
	})
	t.Run("the create response names no owner and the request fails", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateOmitOwner: true, SelfSubjectReviewStatus: http.StatusInternalServerError})
		_, err := x.call("prod-c-test", "user-carol", "carol", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		assert.True(t, strings.HasPrefix(msg, "checking the signed-in user against --user-id 'user-carol': posting a SelfSubjectReview: an error on the server (\"Internal Server Error\") has prevented the request from succeeding"), msg)
		assert.Contains(t, msg, "the token the sign-in created could not be revoked (logout 401; delete 403 Forbidden); v3/")
		assert.Contains(t, msg, "; an administrator can delete it sooner with: rancher delete management-token ")
		assert.Len(t, x.ownerReviews(), 1)
		assert.Equal(t, 3, x.revoked(), "the login token's logout, then the cluster token's logout and delete, both refused")
		assert.False(t, x.cached("prod-c-test", "user-carol"))
	})
	t.Run("the create response names no owner", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateOmitOwner: true})
		got, err := x.call("prod-c-test", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		assert.Equal(t, []string{"/k8s/clusters/c-test" + selfSubjectReviewPath}, x.ownerReviews(), "one owner request where the response named none, through the call's cluster")
	})
	t.Run("the Norman create response names the owner", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true})
		got, err := x.call("prod-c-test", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		assert.Empty(t, x.ownerReviews(), "no request where the response named the owner")
	})
	t.Run("the Norman create response names another owner", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		_, msg := refusal(t, err)
		assert.True(t, strings.HasPrefix(msg, "the sign-in was made as Rancher user 'user-carol'"), msg)
		assert.Empty(t, x.ownerReviews())
	})
	t.Run("without a terminal", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		_, err := x.call("prod-c-test", "user-carol", "", nil)
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "pipe an API token of Rancher user 'user-carol' to: rancher auth login 'prod' --url")
		assert.Contains(t, msg, "or run kubectl once in a terminal and sign in as 'user-carol', which lasts until the cached token expires")
		assert.NotContains(t, msg, "does not use it")
	})
}

// TestUserIDPluginCreatedLoginChecksBothCreates asserts the plugin-created
// login of a call with --user-id stores the login only when the sign-in is
// the call's user's, and records that user as the owner, in an entry named
// for the server and the user; a sign-in as another user revokes both
// tokens and stores nothing. A plugin login entry the --name identifies is
// the entry it writes.
func TestUserIDPluginCreatedLoginChecksBothCreates(t *testing.T) {
	t.Run("the user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		got, err := x.call("prod-c-test", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		login, s := x.stored(x.pluginEntryOf("user-carol"))
		assert.NotEmpty(t, login, "the entry is named for the server and the user")
		assert.Equal(t, "user-carol", s.TokenUserID)
		_, hostOnly := x.stored(x.pluginEntry())
		assert.Empty(t, hostOnly.URL, "the host-only name is for calls without --user-id")
		assert.Empty(t, x.ownerReviews(), "both creates name the owner")
	})
	t.Run("another user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "the sign-in was made as Rancher user 'user-carol', and this exec entry names user 'user-alice' with --user-id; nothing was stored"), msg)
		login, s := x.stored(x.pluginEntryOf("user-alice"))
		assert.Empty(t, login, "nothing is stored")
		assert.Empty(t, s.URL, "no entry is written")
		assert.GreaterOrEqual(t, x.revoked(), 2, "the session token and the login credential are revoked")
		for _, c := range x.f.CreateRequests() {
			assert.NotEqual(t, "cluster", c.Role, "no cluster token is created from another user's login")
		}
		x.mu.Lock()
		defer x.mu.Unlock()
		for _, r := range x.requests {
			assert.NotEqual(t, testrancher.ClustersCollectionPath, r.path, "the owner refusal comes before any cluster read")
		}
	})
	for _, tc := range []struct {
		name            string
		derived, scoped *string
		want            string
	}{
		{name: "the cluster token of another owner", scoped: new("user-mallory"),
			want: "the sign-in was made as Rancher user 'user-mallory', and this exec entry names user 'user-carol' with --user-id; nothing was stored"},
		{name: "no create names the owner", derived: new(""), scoped: new(""),
			want: "the tokens the sign-in created named no owner; the sign-in cannot be checked against --user-id 'user-carol'; nothing was stored"},
		{name: "only the cluster token names the owner", derived: new("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{DerivedCreateOwner: tc.derived, ScopedCreateOwner: tc.scoped})
			x.entry("prod", "", "", false)
			got, err := x.call("prod-c-test", "user-carol", "carol", nil)
			login, s := x.stored(x.pluginEntryOf("user-carol"))
			if tc.want == "" {
				require.NoError(t, err)
				assert.Equal(t, "user-carol", got)
				assert.NotEmpty(t, login)
				assert.Equal(t, "user-carol", s.TokenUserID, "the owner the check established is recorded")
				return
			}
			_, msg := refusal(t, err)
			assert.True(t, strings.HasPrefix(msg, tc.want), msg)
			assert.Empty(t, login, "nothing is stored")
			assert.GreaterOrEqual(t, x.revoked(), 3, "the session token, the cluster token and the login credential are revoked")
		})
	}
	for _, userID := range []string{"user-carol", ""} {
		t.Run("a plugin login entry the --name identifies, --user-id "+userID, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			named := x.pluginEntry() + "-2"
			x.entry(named, "", userID, true)
			got, err := x.call(named+"-c-test", userID, "carol", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-carol", got)
			login, s := x.stored(named)
			assert.NotEmpty(t, login, "the entry the --name identifies is the one logged in")
			assert.Equal(t, "user-carol", s.TokenUserID)
			_, other := x.stored(x.pluginEntry())
			assert.Empty(t, other.URL, "no other plugin login entry is written")
		})
	}
}

// TestUserIDPluginLoginEntryIsNotReadWithTokenReuseOff asserts a call with
// --user-id whose --name identifies a plugin login entry while token reuse
// is off reads nothing of that entry, whatever owner it records, and signs
// in, checking the owner from the cluster token's create response.
func TestUserIDPluginLoginEntryIsNotReadWithTokenReuseOff(t *testing.T) {
	x := newOwnerFixture(t, false, testrancher.Options{})
	x.entry(x.pluginEntry(), "bob", "user-bob", true)
	login, _ := x.stored(x.pluginEntry())
	readsBefore := len(x.store.reads())
	got, err := x.call(x.pluginEntry()+"-c-test", "user-carol", "carol", nil)
	require.NoError(t, err)
	assert.Equal(t, "user-carol", got, "the call signs in")
	assert.NotContains(t, x.store.reads()[readsBefore:], x.pluginEntry(), "the entry's login is not read")
	assert.False(t, x.usedBearer(login))
	assert.Empty(t, x.ownerReviews(), "the cluster token's create response names the owner")
}
