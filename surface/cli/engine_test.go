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
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
func (c *countingStore) SetCachedCred(string, string, time.Time) error     { return nil }
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

func TestRegistryBindingLookup(t *testing.T) {
	reg := runtime.NewRegistry(provider.NewResolver())
	b := reg.Binding("get cluster")
	assert.Equal(t, safety.TierCRD, b.Tier)
	assert.Equal(t, safety.Beta, b.Stability)

	b2 := reg.Binding("create project")
	assert.Equal(t, safety.TierPublic, b2.Tier)
	assert.Equal(t, safety.Stable, b2.Stability)

	bU := reg.Binding("nope")
	assert.Empty(t, bU.Tier)
	assert.Empty(t, bU.Stability)
}
