package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// writeTestConfig writes a minimal config.yaml (one server, current context)
// into a temp dir and returns the dir.
func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "local"}},
		Servers:        map[string]config.Server{"acme": {URL: "https://rancher.test"}},
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

func (c *countingStore) GetToken(string) (string, error) { c.getCalls++; return c.token, nil }
func (c *countingStore) SetToken(string, string) error   { return nil }
func (c *countingStore) DeleteToken(string) error        { return nil }
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

func TestFactoryIdentityAndContextCluster(t *testing.T) {
	t.Parallel()
	f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})
	server, err := f.Identity()
	if err != nil || server == "" {
		t.Fatalf("Identity() = %q, %v; want the configured server name", server, err)
	}
	if _, err := f.ContextCluster(); err != nil {
		t.Fatal(err)
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
	f := resolveFactory(t, &listCountClient{items: []unstructured.Unstructured{
		mgmtClusterObj("c-abc", "dup"),
		mgmtClusterObj("c-def", "dup"),
	}})
	_, err := f.ResolveCluster(context.Background(), "dup")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
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
	// kind table plus the bespoke provider map.
	res, err := runtime.ComposeResolvers(provider.NewResolver(), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(res)

	b := reg.Binding("get cluster") // owned by the kind table
	assert.Equal(t, safety.TierCRD, b.Tier)
	assert.Equal(t, safety.Beta, b.Stability)

	b2 := reg.Binding("create project") // owned by the bespoke resolver
	assert.Equal(t, safety.TierPublic, b2.Tier)
	assert.Equal(t, safety.Stable, b2.Stability)

	bU := reg.Binding("nope")
	assert.Empty(t, bU.Tier)
	assert.Empty(t, bU.Stability)
}
