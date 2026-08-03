package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/credhelper"
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
	return newTestHandlerFor(t, fv.handler(t))
}

// newTestHandlerFor is the one place a test vaultHandler is built over an
// httptest server; every fixture (fakeVault, errBodyVault, routeVault)
// funnels through it.
func newTestHandlerFor(t *testing.T, h http.Handler) *vaultHandler {
	srv := httptest.NewServer(h)
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

	outcome, eerr := h.Erase(u, n)
	require.NoError(t, eerr)
	assert.Equal(t, credhelper.EraseDeleted, outcome, "erase of a stored credential reports a deletion")
	creds, err = h.Get(u, n)
	require.NoError(t, err)
	assert.Nil(t, creds, "erased credential reads as not-found")
	// erase idempotent on missing — and it says so
	outcome, eerr = h.Erase(u, n)
	require.NoError(t, eerr)
	assert.Equal(t, credhelper.EraseNothingStored, outcome, "erase of a missing credential reports a no-op")
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

// errBodyVault returns the given status and a Vault-shaped error body for
// every request, mimicking Vault's real {"errors":[...]} envelope.
func errBodyVault(t *testing.T, status int, errs ...string) *vaultHandler {
	return newTestHandlerFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
	}))
}

// TestVaultErrorsSurfaceResponseBody pins the live-exercise S-12 finding:
// the helper must surface Vault's own error reason (e.g. "invalid token"),
// not substitute one canned policy hint for every non-2xx. The policy hint
// stays, but only on 403 and only as a parenthetical after the real cause.
func TestVaultErrorsSurfaceResponseBody(t *testing.T) {
	t.Run("store 403 invalid token names the real cause", func(t *testing.T) {
		h := errBodyVault(t, http.StatusForbidden, "2 errors occurred:\n\t* permission denied\n\t* invalid token\n\n")
		err := h.Store("https://r.ex", "prod", "tok")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid token", "Vault's own reason must surface")
		assert.Contains(t, err.Error(), "HTTP 403")
		assert.Contains(t, err.Error(), "create+update+read", "policy hint stays on 403")
	})

	t.Run("store non-403 carries no policy hint", func(t *testing.T) {
		h := errBodyVault(t, http.StatusInternalServerError, "internal error")
		err := h.Store("https://r.ex", "prod", "tok")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 500")
		assert.Contains(t, err.Error(), "internal error")
		assert.NotContains(t, err.Error(), "policy", "a 500 is not a policy problem")
	})

	t.Run("erase 403 surfaces cause with delete hint", func(t *testing.T) {
		h := errBodyVault(t, http.StatusForbidden, "permission denied")
		_, err := h.Erase("https://r.ex", "prod")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
		assert.Contains(t, err.Error(), "delete", "erase's policy hint names the delete capability")
	})

	t.Run("get non-200 surfaces cause", func(t *testing.T) {
		h := errBodyVault(t, http.StatusForbidden, "permission denied")
		_, err := h.Get("https://r.ex", "prod")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission denied")
	})

	t.Run("non-JSON body falls back to trimmed raw text", func(t *testing.T) {
		h := newTestHandlerFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>upstream broke</html>\n"))
		}))
		err := h.Store("https://r.ex", "prod", "tok")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upstream broke")
	})

	t.Run("whitespace-only errors array falls back to the raw body", func(t *testing.T) {
		// One, two, and three blank entries: 2+ is the round-5 repro, where
		// joining blanks first fabricated a bare ";" reason that blocked
		// the raw-body fallback.
		for _, errs := range [][]string{{"   "}, {"", ""}, {"", "", ""}} {
			h := errBodyVault(t, http.StatusForbidden, errs...)
			err := h.Store("https://r.ex", "prod", "tok")
			require.Error(t, err, "entries=%q", errs)
			assert.Contains(t, err.Error(), "errors", "the raw body must surface when the errors array is blank (entries=%q)", errs)
			assert.NotContains(t, err.Error(), "403: ;", "a fabricated bare-';' reason must never appear (entries=%q)", errs)
		}
	})

	t.Run("blank entries are dropped, real ones kept", func(t *testing.T) {
		h := errBodyVault(t, http.StatusForbidden, "", "invalid token", "  ")
		err := h.Store("https://r.ex", "prod", "tok")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 403: invalid token", "the surviving real entry must be the reason, unpolluted by blanks")
	})
}

// routeVault serves per-route status/body pairs so mixed-capability policies
// (e.g. delete allowed, read denied) can be modeled — unlike errBodyVault,
// which fails every request identically.
func routeVault(t *testing.T, routes map[string]func(w http.ResponseWriter)) *vaultHandler {
	return newTestHandlerFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/secret/"), "/", 2)[0]
		route, ok := routes[key]
		require.True(t, ok, "unexpected request %s %s", r.Method, r.URL.Path)
		route(w)
	}))
}

// TestVaultEraseOutcomeClassification pins round-2 findings 3 and 4: the
// erase's presence read shares Get's semantics (a 200 whose data is
// null/empty reads as absent, exactly as a get would), and a denied read
// under a delete-only policy yields an UNKNOWN outcome with a successful
// erase — never a failure, never a fabricated deletion.
func TestVaultEraseOutcomeClassification(t *testing.T) {
	t.Run("delete-only policy: probe denied, erase succeeds, outcome unknown", func(t *testing.T) {
		h := routeVault(t, map[string]func(http.ResponseWriter){
			"GET data": func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			},
			"DELETE metadata": func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		})
		outcome, err := h.Erase("https://r.ex", "prod")
		require.NoError(t, err, "a denied presence read must never fail the erase")
		assert.Equal(t, credhelper.EraseUnknown, outcome)
	})

	t.Run("soft-deleted latest version: 200 with null data reads as nothing stored", func(t *testing.T) {
		h := routeVault(t, map[string]func(http.ResponseWriter){
			"GET data": func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"data":null}}`))
			},
			"DELETE metadata": func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		})
		outcome, err := h.Erase("https://r.ex", "prod")
		require.NoError(t, err)
		assert.Equal(t, credhelper.EraseNothingStored, outcome,
			"a value Get would report absent must not be reported as a deletion")
	})
}

// TestVaultGetEmptyDataIsNotFound pins the Get side of the same class: a
// 200 response with null/empty data (a soft-deleted latest version) is a
// clean not-found, not an error and not a credential.
func TestVaultGetEmptyDataIsNotFound(t *testing.T) {
	h := routeVault(t, map[string]func(http.ResponseWriter){
		"GET data": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"data":null}}`))
		},
	})
	creds, err := h.Get("https://r.ex", "prod")
	require.NoError(t, err)
	assert.Nil(t, creds)
}
