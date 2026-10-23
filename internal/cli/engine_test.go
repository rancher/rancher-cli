package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestConfig writes a minimal config.yaml (one server, current server)
// into a temp dir and returns the dir.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{"acme": {URL: "https://rancher.test"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// countingStore is a fake config.CredStore that counts GetToken calls so tests
// can assert credentials are read exactly once per invocation.
type countingStore struct {
	token    string
	getCalls int
}

func (c *countingStore) GetToken(config.ServerRef) (string, error) { c.getCalls++; return c.token, nil }
func (c *countingStore) SetToken(config.ServerRef, string) error   { return nil }
func (c *countingStore) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	return config.CommitOutcome{Backend: clierr.BackendFile, State: config.CommitCommitted, PriorSource: clierr.PriorNone}, nil
}
func (c *countingStore) DeleteToken(config.ServerRef) error { return nil }
func (c *countingStore) GetCachedCred(string, *config.CacheAnchor) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (c *countingStore) GetCachedCredDetail(string, *config.CacheAnchor) (config.CachedCredDetail, error) {
	return config.CachedCredDetail{}, config.ErrNoToken
}
func (c *countingStore) MarkCachedCredReady(string, string, string) (bool, error) { return false, nil }
func (c *countingStore) SetCachedCred(string, string, time.Time, string, string, string, config.CacheBinding) error {
	return nil
}
func (c *countingStore) DeleteAllCachedCreds() error                             { return nil }
func (c *countingStore) ListCachedCreds() ([]config.CachedCredInfo, error)       { return nil, nil }
func (c *countingStore) HoldCachedCreds(func([]config.CachedRecord) error) error { return nil }
func (c *countingStore) DeleteCachedCredsIfUnchanged(map[string]string) ([]string, []string, error) {
	return nil, nil, nil
}
func (c *countingStore) ResolvedBackend() (string, bool) { return "", true }

func TestFactoryResolvesEffectiveOnce(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t) // temp dir with config.yaml: one server, current context
	store := &countingStore{token: "id:secret"}
	f := newClientFactory(dir, store)
	if _, err := f.Cluster(t.Context(), "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Cluster(t.Context(), "c-abc"); err != nil {
		t.Fatal(err)
	}
	if store.getCalls != 1 {
		t.Errorf("credential read per invocation = %d, want 1", store.getCalls)
	}
}

// TestFactoryServerInfoFetchesOnce asserts the shared invocation-scoped probe:
// ServerInfo hits /rancherversion at most once per clientFactory (per
// invocation) no matter how many times it is called, and a second,
// independent clientFactory (a second invocation) fetches again - no
// cross-invocation caching.
func TestFactoryServerInfoFetchesOnce(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.15.0","RancherPrime":"true"}`))
	}))
	defer srv.Close()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{"acme": {URL: srv.URL, CA: string(certPEM)}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := &countingStore{token: "id:secret"}

	f := newClientFactory(dir, store)
	info1, err := f.ServerInfo(t.Context())
	require.NoError(t, err)
	info2, err := f.ServerInfo(t.Context())
	require.NoError(t, err)
	assert.Equal(t, info1, info2)
	assert.Equal(t, 1, hits, "ServerInfo must fetch /rancherversion at most once per invocation")

	// A second clientFactory stands in for a second invocation: no
	// cross-invocation cache.
	f2 := newClientFactory(dir, store)
	_, err = f2.ServerInfo(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, hits, "a new invocation must fetch again - no cross-invocation caching")
}

func TestFactoryRejectsEmptyClusterID(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	_, err := f.Cluster(t.Context(), "")
	if ce, ok := errors.AsType[*clierr.Error](err); !ok || ce.Code != clierr.CodeValidation {
		t.Errorf("empty cluster id: got %v, want VALIDATION error", err)
	}
}

func TestFactoryIdentity(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	server, err := f.Identity()
	if err != nil || server == "" {
		t.Fatalf("Identity() = %q, %v; want the configured server name", server, err)
	}
}

func TestFactoryConcurrentAccess(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	var wg sync.WaitGroup
	for i := range 8 {
		id := fmt.Sprintf("c-%d", i%3)
		wg.Go(func() {
			_, _ = f.Cluster(t.Context(), id)
		})
	}
	wg.Wait() // -race is the assertion
}

// steveClustersRoot is an engine.RootClient that serves a steve clusters
// collection in one page, answers a GET by id from byID (404 otherwise),
// and records every path.
type steveClustersRoot struct {
	items []map[string]any
	byID  map[string]int // status per cluster id; 200 serves the object
	mu    sync.Mutex
	paths []string
}

func steveClusterItem(id, display string) map[string]any {
	return map[string]any{"id": id, "type": "management.cattle.io.cluster",
		"metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

func (r *steveClustersRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	const collection = "/v1/management.cattle.io.clusters"
	if path == collection {
		return json.Marshal(map[string]any{"data": r.items, "count": len(r.items), "revision": "1"})
	}
	id, ok := strings.CutPrefix(path, collection+"/")
	status := r.byID[id]
	switch {
	case ok && status == http.StatusOK:
		return json.Marshal(steveClusterItem(id, ""))
	case status == 0:
		status = http.StatusNotFound
	}
	return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("rancher root GET %s: %d", path, status)).
		WithCause(clierr.ResponseError(http.MethodGet, status, nil))
}

func (r *steveClustersRoot) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

// resolveFactory seeds the factory's root client with a fake so
// ResolveCluster reads steve without a real network call.
func resolveFactory(t *testing.T, root *steveClustersRoot) *clientFactory {
	t.Helper()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	f.rootOnce.Do(func() { f.root = root })
	return f
}

func TestResolveClusterByIDNameAndLocal(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod"), steveClusterItem("c-def", "dev")}}
	f := resolveFactory(t, root)
	for _, tc := range []struct{ in, want string }{
		{"local", "local"},
		{"", "local"},
		{"c-abc", "c-abc"},
		{"prod", "c-abc"},
	} {
		got, err := f.ResolveCluster(t.Context(), tc.in)
		require.NoError(t, err, "ResolveCluster(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ResolveCluster(%q)", tc.in)
	}
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters"}, root.requests(), "local and \"\" send no request")
	assert.Empty(t, f.DrainNotices())
}

// TestResolveClusterUnknownIsNotFound asserts the NOT_FOUND text, hint and
// candidates, after the one GET by id a cluster-shaped miss sends.
func TestResolveClusterUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod"), steveClusterItem("c-def", "dev")}}
	f := resolveFactory(t, root)
	_, err := f.ResolveCluster(t.Context(), "staging")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster not found: 'staging'; it is not among the clusters you can read; pass a cluster id or display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "prod"}, {ID: "c-def", Name: "dev"}}, ce.Candidates)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters", "/v1/management.cattle.io.clusters/staging"}, root.requests())
}

// TestResolveClusterMissResolvesThroughTheGetByID asserts a cluster steve's
// cache has not listed yet resolves through one GET by id.
func TestResolveClusterMissResolvesThroughTheGetByID(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod")}, byID: map[string]int{"c-new": http.StatusOK}}
	f := resolveFactory(t, root)
	got, err := f.ResolveCluster(t.Context(), "c-new")
	require.NoError(t, err)
	assert.Equal(t, "c-new", got)
}

func TestResolveClusterAmbiguousDisplayName(t *testing.T) {
	t.Parallel()
	// The non-colliding third cluster exercises the shared match helper's
	// candidate narrowing THROUGH this call site: the error's candidates
	// must list only the two colliders.
	f := resolveFactory(t, &steveClustersRoot{items: []map[string]any{
		steveClusterItem("c-abc", "dup"), steveClusterItem("c-def", "dup"), steveClusterItem("c-solo", "unique"),
	}})
	_, err := f.ResolveCluster(t.Context(), "dup")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []clierr.Candidate{{ID: "c-abc", Name: "dup"}, {ID: "c-def", Name: "dup"}}, ce.Candidates)
}

// TestResolveClusterIDWinsWithOneNotice asserts an id that is also another
// cluster's display name resolves to the id, and the factory hands the
// warning to the pipeline once, however often the value resolves.
func TestResolveClusterIDWinsWithOneNotice(t *testing.T) {
	t.Parallel()
	f := resolveFactory(t, &steveClustersRoot{items: []map[string]any{
		steveClusterItem("c-abc", "foo"), steveClusterItem("foo", "bar"),
	}})
	for range 2 {
		got, err := f.ResolveCluster(t.Context(), "foo")
		require.NoError(t, err)
		assert.Equal(t, "foo", got)
	}
	assert.Equal(t, []string{"--cluster 'foo' is the id of cluster 'foo' and the display name of cluster 'c-abc'; the command uses the one with that id. To use the other one, pass --cluster 'c-abc'"},
		f.DrainNotices())
	assert.Empty(t, f.DrainNotices(), "drained once")
}

func TestResolveClusterMemoizes(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "prod"), steveClusterItem("c-def", "dev")}}
	f := resolveFactory(t, root)
	for range 2 {
		got, err := f.ResolveCluster(t.Context(), "prod")
		require.NoError(t, err)
		assert.Equal(t, "c-abc", got)
	}
	assert.Len(t, root.requests(), 1, "second resolve served from the memo")
}

// TestResolveClusterDistinctNamesShareOneList asserts the name→id memo only
// helps a REPEATED name (TestResolveClusterMemoizes above); resolving
// several DISTINCT names shares one collection read for the whole
// invocation.
func TestResolveClusterDistinctNamesShareOneList(t *testing.T) {
	t.Parallel()
	root := &steveClustersRoot{items: []map[string]any{
		steveClusterItem("c-abc", "prod"), steveClusterItem("c-def", "dev"), steveClusterItem("c-ghi", "staging"),
	}}
	f := resolveFactory(t, root)
	for _, tc := range []struct{ in, want string }{
		{"prod", "c-abc"},
		{"dev", "c-def"},
		{"staging", "c-ghi"},
	} {
		got, err := f.ResolveCluster(t.Context(), tc.in)
		require.NoError(t, err, "ResolveCluster(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ResolveCluster(%q)", tc.in)
	}
	assert.Len(t, root.requests(), 1, "three distinct names must still cost one collection read")
}

// TestNoticeSourcesDrainsInOrder asserts the factory's notices come ahead
// of the next source's.
func TestNoticeSourcesDrainsInOrder(t *testing.T) {
	t.Parallel()
	f := resolveFactory(t, &steveClustersRoot{items: []map[string]any{steveClusterItem("c-abc", "foo"), steveClusterItem("foo", "")}})
	_, err := f.ResolveCluster(t.Context(), "foo")
	require.NoError(t, err)
	got := noticeSources{f, fixedNotices{"from the store"}}.DrainNotices()
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "--cluster 'foo' is the id of cluster 'foo'")
	assert.Equal(t, "from the store", got[1])
}

type fixedNotices []string

func (n fixedNotices) DrainNotices() []string { return n }

func TestRegistryBindingLookup(t *testing.T) {
	t.Parallel()
	// The registry runs on the composed resolver: generated bindings from the
	// kind table plus the bespoke provider map plus the derived create ops.
	res, err := composeBindings()
	require.NoError(t, err)
	reg := runtime.NewRegistry(res)

	b := reg.Binding("get cluster") // owned by the kind table
	assert.Equal(t, safety.ContractInternal, b.Contract)
	assert.Equal(t, safety.Beta, b.Stability)

	b2 := reg.Binding("create project") // derived from the project kind row
	assert.Equal(t, safety.ContractPublic, b2.Contract)
	assert.Equal(t, safety.Stable, b2.Stability)

	bU := reg.Binding("nope")
	assert.Empty(t, bU.Contract)
	assert.Empty(t, bU.Stability)
}

// makeJWT builds a minimal JWT string (header.payload.sig) for tests. The
// signature segment is garbage - jwtTokenID intentionally skips verification.
func makeJWT(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	p, _ := json.Marshal(payload)
	payloadEnc := base64.RawURLEncoding.EncodeToString(p)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fakesig"))
	return header + "." + payloadEnc + "." + sig
}

// TestJWTTokenIDIDSecret verifies the id:secret path returns the id half.
func TestJWTTokenIDIDSecret(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "mytoken", jwtTokenID("mytoken:secret"))
}

// TestJWTTokenIDV3JWT verifies that a Rancher JWT with token_kind=v3 returns
// the backing token name from the "token" claim.
func TestJWTTokenIDV3JWT(t *testing.T) {
	t.Parallel()
	tok := makeJWT(map[string]any{"token": "local-abc-123", "token_kind": "v3"})
	assert.Equal(t, "local-abc-123", jwtTokenID(tok))
}

// TestJWTTokenIDExtJWT verifies that a Rancher JWT with token_kind=ext returns
// "" - the backing ext.cattle.io Token cannot match a management-token name.
func TestJWTTokenIDExtJWT(t *testing.T) {
	t.Parallel()
	tok := makeJWT(map[string]any{"token": "ext-abc-123", "token_kind": "ext"})
	assert.Equal(t, "", jwtTokenID(tok))
}

// TestJWTTokenIDMalformedJWT verifies that a malformed bearer (wrong segment
// count or non-base64 payload) returns "" without panicking.
func TestJWTTokenIDMalformedJWT(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", jwtTokenID("not.a.valid.jwt.at.all"))
	assert.Equal(t, "", jwtTokenID("only.two"))
	assert.Equal(t, "", jwtTokenID("bad.!!!.sig"))
}

// TestJWTTokenIDEmpty verifies that an empty bearer returns "".
func TestJWTTokenIDEmpty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", jwtTokenID(""))
}

// TestActiveTokenIDV3JWT verifies the full factory path: a JWT bearer with
// token_kind=v3 in the credential store yields the backing token name.
func TestActiveTokenIDV3JWT(t *testing.T) {
	t.Parallel()
	tok := makeJWT(map[string]any{"token": "session-token-xyz", "token_kind": "v3"})
	f := newClientFactory(writeTestConfig(t), &countingStore{token: tok})
	id, err := f.ActiveTokenID()
	require.NoError(t, err)
	assert.Equal(t, "session-token-xyz", id)
}

// TestActiveTokenIDIDSecret verifies the full factory path for an id:secret
// bearer - the id half is returned.
func TestActiveTokenIDIDSecret(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "mytoken:mysecret"})
	id, err := f.ActiveTokenID()
	require.NoError(t, err)
	assert.Equal(t, "mytoken", id)
}
