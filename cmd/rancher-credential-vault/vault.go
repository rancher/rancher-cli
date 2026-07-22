package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rancher/rancher-cli/credhelper"
)

// vaultHandler stores one secret per credential identity in KV v2 at
// <mount>/data/<path>/<sha256hex(serverURL+"\n"+name)>. The CLI sends
// canonical serverURLs (the wire contract); hex-only key segments make path
// escape impossible whatever the inputs. erase uses the metadata endpoint —
// the data endpoint only soft-deletes the latest version, which would
// violate the protocol's erase contract.
//
// Required Vault policy: create, update, read on <mount>/data/<path>/*;
// delete on <mount>/metadata/<path>/* (update matters: without it the first
// login works and every re-login fails permission-denied).
type vaultHandler struct {
	addr, token, mount, path string
	client                   *http.Client
}

func newVaultHandlerFromEnv() (*vaultHandler, error) {
	addr := os.Getenv("VAULT_ADDR")
	if addr == "" {
		return nil, fmt.Errorf("VAULT_ADDR is not set")
	}
	token := os.Getenv("VAULT_TOKEN")
	if token == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			if b, rerr := os.ReadFile(filepath.Join(home, ".vault-token")); rerr == nil {
				token = strings.TrimSpace(string(b))
			}
		}
	}
	if token == "" {
		return nil, fmt.Errorf("no Vault token: set VAULT_TOKEN or create ~/.vault-token")
	}
	mount := envOr("RANCHER_VAULT_MOUNT", "secret")
	path := envOr("RANCHER_VAULT_PATH", "rancher-cli")
	return &vaultHandler{addr: strings.TrimRight(addr, "/"), token: token, mount: mount, path: path, client: http.DefaultClient}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func identityHash(serverURL, name string) string {
	sum := sha256.Sum256([]byte(serverURL + "\n" + name))
	return hex.EncodeToString(sum[:])
}

func (h *vaultHandler) url(kind, serverURL, name string) string {
	return fmt.Sprintf("%s/v1/%s/%s/%s/%s", h.addr, h.mount, kind, h.path, identityHash(serverURL, name))
}

func (h *vaultHandler) do(method, url string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Vault-Token", h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

func (h *vaultHandler) Get(serverURL, name string) (*credhelper.Credentials, error) {
	code, body, err := h.do(http.MethodGet, h.url("data", serverURL, name), nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("vault get: HTTP %d", code)
	}
	var envelope struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("vault get: decoding response: %w", err)
	}
	d := envelope.Data.Data
	// Defense in depth against hash-scheme drift: the stored identity must
	// match the request's; a mismatch reads as not-found, never a leak.
	if d["serverURL"] != serverURL || d["name"] != name {
		return nil, nil
	}
	if d["token"] == "" {
		return nil, nil
	}
	return &credhelper.Credentials{Token: d["token"]}, nil
}

func (h *vaultHandler) Store(serverURL, name, token string) error {
	code, _, err := h.do(http.MethodPost, h.url("data", serverURL, name), map[string]any{
		"data": map[string]string{"token": token, "serverURL": serverURL, "name": name},
	})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		return fmt.Errorf("vault store: HTTP %d (policy needs create+update+read on %s/data/%s/*)", code, h.mount, h.path)
	}
	return nil
}

func (h *vaultHandler) Erase(serverURL, name string) error {
	code, _, err := h.do(http.MethodDelete, h.url("metadata", serverURL, name), nil)
	if err != nil {
		return err
	}
	// 404 = already gone = success (idempotent).
	if code != http.StatusNoContent && code != http.StatusOK && code != http.StatusNotFound {
		return fmt.Errorf("vault erase: HTTP %d (policy needs delete on %s/metadata/%s/*)", code, h.mount, h.path)
	}
	return nil
}
