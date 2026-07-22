package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVault speaks just enough KV v2: /v1/secret/data/<key> GET/POST and
// /v1/secret/metadata/<key> DELETE, recording exact request paths.
type fakeVault struct {
	mu      sync.Mutex
	secrets map[string]map[string]string
	paths   []string
	// requireUpdateCapability simulates a policy without update: second POST
	// to an existing key returns 403 when true.
	denySecondWrite bool
}

func newFakeVault() *fakeVault { return &fakeVault{secrets: map[string]map[string]string{}} }

func (f *fakeVault) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		require.Equal(t, "test-token", r.Header.Get("X-Vault-Token"))
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.Method == http.MethodGet:
			key := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
			data, ok := f.secrets[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": data}})
		case strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.Method == http.MethodPost:
			key := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
			if _, exists := f.secrets[key]; exists && f.denySecondWrite {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var body struct {
				Data map[string]string `json:"data"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.secrets[key] = body.Data
		case strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/") && r.Method == http.MethodDelete:
			key := strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")
			delete(f.secrets, key) // metadata delete removes everything; 204 even when absent
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

func newTestHandler(t *testing.T, fv *fakeVault) *vaultHandler {
	srv := httptest.NewServer(fv.handler(t))
	t.Cleanup(srv.Close)
	return &vaultHandler{addr: srv.URL, token: "test-token", mount: "secret", path: "rancher-cli", client: srv.Client()}
}

func TestVaultStoreGetErase(t *testing.T) {
	t.Parallel()
	fv := newFakeVault()
	h := newTestHandler(t, fv)
	const u, n = "https://rancher.example.com", "prod"

	require.NoError(t, h.Store(u, n, "id:secret"))
	creds, err := h.Get(u, n)
	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.Equal(t, "id:secret", creds.Token)

	require.NoError(t, h.Erase(u, n))
	creds, err = h.Get(u, n)
	require.NoError(t, err)
	assert.Nil(t, creds, "erased credential reads as not-found")
	// erase idempotent on missing
	require.NoError(t, h.Erase(u, n))
	// exact endpoint: metadata delete, never data soft-delete
	joined := strings.Join(fv.paths, "\n")
	assert.Contains(t, joined, "DELETE /v1/secret/metadata/rancher-cli/")
	assert.NotContains(t, joined, "DELETE /v1/secret/data/")
}

func TestVaultCrossServerSameNameIsolation(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeVault())
	require.NoError(t, h.Store("https://a.example.com", "prod", "token-a"))
	require.NoError(t, h.Store("https://b.example.com", "prod", "token-b"))
	ca, err := h.Get("https://a.example.com", "prod")
	require.NoError(t, err)
	require.NotNil(t, ca)
	assert.Equal(t, "token-a", ca.Token, "same name on two servers must not collide")
}

func TestVaultPathHostileIdentity(t *testing.T) {
	t.Parallel()
	fv := newFakeVault()
	h := newTestHandler(t, fv)
	require.NoError(t, h.Store("https://x.example.com", "../escape/attempt", "tok"))
	for _, p := range fv.paths {
		assert.NotContains(t, p, "..", "hashed keys never carry hostile segments")
	}
}

func TestVaultGetServerURLMismatchIsNotFound(t *testing.T) {
	t.Parallel()
	fv := newFakeVault()
	h := newTestHandler(t, fv)
	require.NoError(t, h.Store("https://a.example.com", "prod", "tok"))
	// Corrupt the stored serverURL field to simulate hash-scheme drift.
	fv.mu.Lock()
	for k, v := range fv.secrets {
		v["serverURL"] = "https://evil.example.com"
		fv.secrets[k] = v
	}
	fv.mu.Unlock()
	creds, err := h.Get("https://a.example.com", "prod")
	require.NoError(t, err)
	assert.Nil(t, creds, "stored serverURL mismatch reads as not-found")
}

func TestVaultStoreTwiceSameIdentity(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, newFakeVault()) // policy fixture allows update
	require.NoError(t, h.Store("https://a.example.com", "prod", "tok-1"))
	require.NoError(t, h.Store("https://a.example.com", "prod", "tok-2"), "re-login/rotation is an update, not a create")
	c, err := h.Get("https://a.example.com", "prod")
	require.NoError(t, err)
	assert.Equal(t, "tok-2", c.Token)
}
