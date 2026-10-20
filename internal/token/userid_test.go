package token

import (
	"bytes"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// countingStore records the cache keys a call reads, so a test can tell
// which slots the call looked at and how often.
type countingStore struct {
	config.CredStore
	mu    sync.Mutex
	reads []string
}

func (s *countingStore) GetCachedCred(key string, anchor *config.CacheAnchor) (string, time.Time, error) {
	s.mu.Lock()
	s.reads = append(s.reads, key)
	s.mu.Unlock()
	return s.CredStore.GetCachedCred(key, anchor)
}

func (s *countingStore) GetCachedCredDetail(key string, anchor *config.CacheAnchor) (config.CachedCredDetail, error) {
	s.mu.Lock()
	s.reads = append(s.reads, key)
	s.mu.Unlock()
	return s.CredStore.GetCachedCredDetail(key, anchor)
}

func (s *countingStore) cacheReads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reads...)
}

// failingRancher is a server that answers every request with 500, for
// calls a test expects to stop before any request, or to fail if one is
// made.
func failingRancher(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGetTokenUserIDIsCheckedBeforeAnyCacheRead asserts --user-id typed
// empty and a value that is not a Rancher user id are refused before the
// call reads anything, even with a cached token waiting under the key the
// call would read.
func TestGetTokenUserIDIsCheckedBeforeAnyCacheRead(t *testing.T) {
	srv := failingRancher(t)
	dir := t.TempDir()
	store := &countingStore{CredStore: config.NewFileStore(dir)}
	serverID := config.CanonicalServerIdentity(srv.URL)
	for _, u := range []string{"", "u-a"} {
		require.NoError(t, seedCacheFor(t, dir, store.CredStore, config.CacheKeyFor("prod", serverID, "c-1", u, ""), "warm-tok", time.Now().Add(time.Hour), "prod", serverID, "c-1", u))
	}
	for _, tc := range []struct {
		name, userID, wantMsg string
	}{
		{"prod", "", "--user-id is empty; pass a value or omit --user-id"},
		{"prod", "U-A", "--user-id 'U-A' is not a Rancher user id (a lowercase name such as u-abc12)"},
		{"prod", "u-a/b", "--user-id 'u-a/b' is not a Rancher user id (a lowercase name such as u-abc12)"},
		{"prod", "u a", "--user-id 'u a' is not a Rancher user id (a lowercase name such as u-abc12)"},
	} {
		t.Run(tc.name+"/"+tc.userID, func(t *testing.T) {
			cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
			ops := map[string]int{}
			restore := config.SetIdentityObserverForTest(func(op string) { ops[op]++ })
			t.Cleanup(restore)
			before := len(store.cacheReads())
			s := map[string]string{"server": srv.URL, "cluster": "c-1", "user-id": tc.userID}
			if tc.name != "" {
				s["name"] = tc.name
			}
			_, err := cmd.Execute(t.Context(), tin{s: s}, ui.Discard())
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.wantMsg, clierr.ExpandRemedies(ce.Message, ""))
			assert.Len(t, store.cacheReads(), before, "no cache slot is read")
			assert.Empty(t, ops, "and nothing else is read: no stamp, no config")
		})
	}
}

// TestGetTokenClusterIsRequiredUnlessUserIDIsGiven asserts the refusals of
// --cluster, before the call reads anything, even with a cached token
// waiting under the key the call would read: a missing one without
// --user-id, which asks for a token for Rancher itself only when it is
// given, and a typed empty one with or without it.
func TestGetTokenClusterIsRequiredUnlessUserIDIsGiven(t *testing.T) {
	srv := failingRancher(t)
	dir := t.TempDir()
	store := &countingStore{CredStore: config.NewFileStore(dir)}
	serverID := config.CanonicalServerIdentity(srv.URL)
	require.NoError(t, seedCache(t, dir, store.CredStore, config.CacheKeyFor("prod", serverID, "", "", ""), "warm-tok", time.Now().Add(time.Hour), "prod", serverID, ""))
	require.NoError(t, seedCacheFor(t, dir, store.CredStore, config.CacheKeyFor("", serverID, "", "u-a", ""), "warm-user-tok", time.Now().Add(time.Hour), "", serverID, "", "u-a"))
	for _, tc := range []struct {
		name  string
		flags map[string]string
		want  string
	}{
		{"no --cluster, no --user-id", map[string]string{"name": "prod"}, "--cluster is required"},
		{"no --cluster, no --user-id, no --name", map[string]string{}, "--cluster is required"},
		{"--cluster=, no --user-id", map[string]string{"name": "prod", "cluster": ""}, "--cluster is empty; pass a value or omit --cluster"},
		{"--cluster= with --user-id", map[string]string{"name": "prod", "cluster": "", "user-id": "u-a"}, "--cluster is empty; pass a value or omit --cluster"},
		{"--cluster= with --user-id, no --name", map[string]string{"cluster": "", "user-id": "u-a"}, "--cluster is empty; pass a value or omit --cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
			ops := map[string]int{}
			restore := config.SetIdentityObserverForTest(func(op string) { ops[op]++ })
			t.Cleanup(restore)
			before := len(store.cacheReads())
			s := map[string]string{"server": srv.URL}
			maps.Copy(s, tc.flags)
			_, err := cmd.Execute(t.Context(), tin{s: s}, ui.Discard())
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Len(t, store.cacheReads(), before, "no cache slot is read")
			assert.Empty(t, ops, "and nothing else is read: no stamp, no config")
		})
	}
}

// TestGetTokenRefusesUserIDPrefixedName asserts a --name in the namespace
// of the --user-id cache keys is refused, with or without --user-id, by
// the flag path before it reads the cache and by runWithParams for a
// caller that bypasses the flags.
func TestGetTokenRefusesUserIDPrefixedName(t *testing.T) {
	const want = "--name values starting with 'user-id:' are reserved for the exec plugin's cache slots of calls with --user-id; use a different name"
	srv := failingRancher(t)
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity(srv.URL)
	// The slot a call with --name user-id:u-a/prod and no --user-id would
	// read is the slot of --name prod with --user-id u-a.
	key := config.CacheKeyFor("prod", serverID, "c-1", "u-a", "")
	require.Equal(t, key, config.CacheKey("user-id:u-a/prod", serverID, "c-1"))
	require.NoError(t, seedCache(t, dir, store, key, "warm-tok", time.Now().Add(time.Hour), "prod", serverID, "c-1"))

	for _, s := range []map[string]string{
		{"server": srv.URL, "cluster": "c-1", "name": "user-id:u-a/prod"},
		{"server": srv.URL, "cluster": "c-1", "name": "user-id:u-a/prod", "user-id": "u-a"},
	} {
		cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: s}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, "the warm slot is never served")
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, want, ce.Message)
	}

	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, err := cmd.runWithParams(t.Context(), runParams{Server: srv.URL, Cluster: "c-1", Name: "user-id:x"}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, want, ce.Message)
}

// TestUserIDKeyIsOneKeyOnBothPaths asserts the flag fast path and the
// resolved path build the same key for every input, a user id included,
// so a call that misses reads its slot once.
func TestUserIDKeyIsOneKeyOnBothPaths(t *testing.T) {
	for _, tc := range []struct{ name, server, cluster, userID string }{
		{"prod", "https://r.ex", "c-1", ""},
		{"prod", "https://r.ex", "c-1", "u-a"},
		{"prod-c-1", "r.ex:8443", "c-1", "user-xyz"},
	} {
		got, ok := computeCacheProbeKey(tc.name, tc.server, tc.cluster, tc.userID, "")
		require.True(t, ok)
		assert.Equal(t, config.CacheKeyFor(tc.name, config.CanonicalServerIdentity(applyDefaultScheme(tc.server)), tc.cluster, tc.userID, ""), got)
	}

	r := newRecordFixture(t, false)
	r.login("prod", "", "alice", false)
	store := &countingStore{CredStore: r.store}
	cmd := NewTokenCommand(r.dir, store, anchorFor(r.dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), tin{s: map[string]string{
		"server": r.srv.URL, "cluster": "c-test", "name": "prod", "user-id": "user-alice",
	}}, ui.Discard())
	require.NoError(t, err)
	want := config.CacheKeyFor("prod", config.CanonicalServerIdentity(r.srv.URL), "c-test", "user-alice", "")
	assert.Equal(t, []string{want}, store.cacheReads(), "one read, of the slot of the name and the user id")
}

// callWithUserID runs the exec plugin for --name name and, when userID is
// set, --user-id userID, with no terminal, and returns the token served.
func (r *recordFixture) callWithUserID(name, userID string) (string, error) {
	r.t.Helper()
	s := map[string]string{"server": r.srv.URL, "cluster": "c-test", "name": name}
	if userID != "" {
		s["user-id"] = userID
	} else {
		s["auth-provider"] = "localProvider"
	}
	cmd := NewTokenCommand(r.dir, r.store, anchorFor(r.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(r.t.Context(), tin{s: s}, ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false))
	if err != nil {
		return "", err
	}
	cred, ok := res.Data.(*execCredential)
	require.True(r.t, ok)
	return cred.Status.Token, nil
}

// TestUserIDSlotsAreSeparateOnBothPaths asserts a call with --user-id
// never serves the record of the same --name without it, and a call
// without --user-id never serves a record of a call with it: on the flag
// fast path and on the resolved path a kubeconfig with a direct context
// takes, where kubectl names its cluster entry. Each call writes its own
// record, and the resolved path serves the record of its own slot.
func TestUserIDSlotsAreSeparateOnBothPaths(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag fast path", true: "resolved path"}[resolved], func(t *testing.T) {
			r := newRecordFixture(t, false)
			if resolved {
				t.Setenv(execInfoEnv, execInfoFor(t, r.srv.URL+"/k8s/clusters/c-test", nil))
			}
			r.login("prod", "", "alice", false)
			serverID := config.CanonicalServerIdentity(r.srv.URL)
			plainKey := config.CacheKeyFor("prod", serverID, "c-test", "", "")
			userKey := config.CacheKeyFor("prod", serverID, "c-test", "user-alice", "")
			require.NoError(t, seedCache(t, r.dir, r.store, plainKey, "plain-tok", time.Now().Add(time.Hour), "prod", serverID, "c-test"))

			first, err := r.callWithUserID("prod", "user-alice")
			require.NoError(t, err)
			assert.NotEqual(t, "plain-tok", first, "the record without the user id is not read")
			cached, _, err := r.store.GetCachedCred(userKey, anchorFor(r.dir))
			require.NoError(t, err)
			assert.Equal(t, first, cached, "the call wrote its own slot")

			second, err := r.callWithUserID("prod", "user-alice")
			require.NoError(t, err)
			assert.Equal(t, first, second, "the slot of the name and the user id is served")

			plain, err := r.callWithUserID("prod", "")
			require.NoError(t, err)
			assert.Equal(t, "plain-tok", plain, "a call without --user-id reads only its own slot")

			_, _, derr := r.store.DeleteCachedCredsIfUnchanged(map[string]string{plainKey: "plain-tok"})
			require.NoError(t, derr)
			again, err := r.callWithUserID("prod", "")
			require.NoError(t, err)
			assert.NotEqual(t, first, again, "a call without --user-id never serves the user id's slot")
		})
	}
}
