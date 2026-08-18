package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
func (c *countingStore) DeleteToken(config.ServerRef) error        { return nil }
func (c *countingStore) GetCachedCred(string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (c *countingStore) SetCachedCred(string, string, time.Time, string, string, string) error {
	return nil
}
func (c *countingStore) DeleteCachedCred(string) error                     { return nil }
func (c *countingStore) DeleteAllCachedCreds() error                       { return nil }
func (c *countingStore) ListCachedCreds() ([]config.CachedCredInfo, error) { return nil, nil }

func TestFactoryResolvesEffectiveOnce(t *testing.T) {
	t.Parallel()
	dir := writeTestConfig(t) // temp dir with config.yaml: one server, current context
	store := &countingStore{token: "id:secret"}
	f := newClientFactory(dir, store)
	if _, err := f.Cluster(context.Background(), "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Cluster(context.Background(), "c-abc"); err != nil {
		t.Fatal(err)
	}
	if store.getCalls != 1 {
		t.Errorf("credential read per invocation = %d, want 1", store.getCalls)
	}
}

func TestFactoryRejectsEmptyClusterID(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	_, err := f.Cluster(context.Background(), "")
	var ce *clierr.Error
	if !errors.As(err, &ce) || ce.Code != clierr.CodeValidation {
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
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("c-%d", i%3)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.Cluster(context.Background(), id)
		}()
	}
	wg.Wait() // -race is the assertion
}

// listCountClient serves canned management-cluster objects for List and counts
// the List calls so the memoization test can assert a single hit to the server.
type listCountClient struct {
	items     []unstructured.Unstructured
	listCalls int
}

func (c *listCountClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *listCountClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	c.listCalls++
	return &unstructured.UnstructuredList{Items: c.items}, nil
}
func (c *listCountClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}
func (c *listCountClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *listCountClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *listCountClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func mgmtClusterObj(name, display string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display},
	}}
}

// resolveFactory seeds the factory's "local" client with a fake so ResolveCluster
// lists against it without a real network call.
func resolveFactory(t *testing.T, c engine.ClusterClient) *clientFactory {
	t.Helper()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	f.cache[engine.LocalCluster] = c
	return f
}

func TestResolveClusterByIDNameAndLocal(t *testing.T) {
	t.Parallel()
	f := resolveFactory(t, &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "prod"),
		mgmtClusterObj("c-def", "dev"),
	}})
	for _, tc := range []struct{ in, want string }{
		{"local", "local"},
		{"", "local"},
		{"c-abc", "c-abc"},
		{"prod", "c-abc"},
	} {
		got, err := f.ResolveCluster(context.Background(), tc.in)
		require.NoError(t, err, "ResolveCluster(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ResolveCluster(%q)", tc.in)
	}
}

func TestResolveClusterUnknownIsNotFound(t *testing.T) {
	t.Parallel()
	f := resolveFactory(t, &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "prod"),
		mgmtClusterObj("c-def", "dev"),
	}})
	_, err := f.ResolveCluster(context.Background(), "staging")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	var names []string
	for _, c := range ce.Candidates {
		names = append(names, c.Name)
	}
	assert.ElementsMatch(t, []string{"prod", "dev"}, names, "candidates list every cluster")
}

func TestResolveClusterAmbiguousDisplayName(t *testing.T) {
	t.Parallel()
	// The non-colliding third cluster pins the shared match helper's
	// candidate narrowing THROUGH this call site: the error's candidates
	// must list only the two colliders.
	f := resolveFactory(t, &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "dup"),
		mgmtClusterObj("c-def", "dup"),
		mgmtClusterObj("c-solo", "unique"),
	}})
	_, err := f.ResolveCluster(context.Background(), "dup")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	require.Len(t, ce.Candidates, 2, "candidates must narrow to the colliders")
	ids := []string{ce.Candidates[0].ID, ce.Candidates[1].ID}
	assert.ElementsMatch(t, []string{"c-abc", "c-def"}, ids)
}

func TestResolveClusterIDDisplayCollisionIsAmbiguous(t *testing.T) {
	t.Parallel()
	// A's display name equals B's id: an exact-id match on "foo" (B) must not
	// silently shadow the display match on "foo" (A).
	f := resolveFactory(t, &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "foo"),
		mgmtClusterObj("foo", "bar"),
	}})
	_, err := f.ResolveCluster(context.Background(), "foo")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
}

func TestResolveClusterMemoizes(t *testing.T) {
	t.Parallel()
	client := &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "prod"),
		mgmtClusterObj("c-def", "dev"),
	}}
	f := resolveFactory(t, client)
	for i := 0; i < 2; i++ {
		got, err := f.ResolveCluster(context.Background(), "prod")
		require.NoError(t, err)
		assert.Equal(t, "c-abc", got)
	}
	assert.Equal(t, 1, client.listCalls, "second resolve served from the memo")
}

// TestResolveClusterDistinctNamesShareOneList pins the fan-out fix: the
// name→id memo only helps a REPEATED name (TestResolveClusterMemoizes above);
// resolving several DISTINCT names used to List once per name. They now share
// one cached list for the whole invocation.
func TestResolveClusterDistinctNamesShareOneList(t *testing.T) {
	t.Parallel()
	client := &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "prod"),
		mgmtClusterObj("c-def", "dev"),
		mgmtClusterObj("c-ghi", "staging"),
	}}
	f := resolveFactory(t, client)
	for _, tc := range []struct{ in, want string }{
		{"prod", "c-abc"},
		{"dev", "c-def"},
		{"staging", "c-ghi"},
	} {
		got, err := f.ResolveCluster(context.Background(), tc.in)
		require.NoError(t, err, "ResolveCluster(%q)", tc.in)
		assert.Equal(t, tc.want, got, "ResolveCluster(%q)", tc.in)
	}
	assert.Equal(t, 1, client.listCalls, "three distinct names must still cost one List")
}

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
// signature segment is garbage — jwtTokenID intentionally skips verification.
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
// "" — the backing ext.cattle.io Token cannot match a management-token name.
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
// bearer — the id half is returned.
func TestActiveTokenIDIDSecret(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "mytoken:mysecret"})
	id, err := f.ActiveTokenID()
	require.NoError(t, err)
	assert.Equal(t, "mytoken", id)
}
