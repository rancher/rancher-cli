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
// escape impossible whatever the inputs. erase uses the metadata endpoint -
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

// httpError builds the error for a non-2xx Vault response: the verb, the
// status, Vault's own error reason from the response body (the {"errors":
// [...]} envelope, or raw text for a non-JSON body), and - on 403 only -
// a parenthetical policy hint naming the capabilities the verb needs. The
// body must lead: a 403 can be an invalid/expired token just as well as a
// missing policy capability, and Vault's body says which (live-exercise
// S-12: a canned policy hint alone misdirects token problems).
func httpError(verb string, code int, body []byte, policyHint string) error {
	msg := fmt.Sprintf("vault %s: HTTP %d", verb, code)
	if reason := vaultErrors(body); reason != "" {
		msg += ": " + reason
	}
	if code == http.StatusForbidden && policyHint != "" {
		msg += " (" + policyHint + ")"
	}
	return fmt.Errorf("%s", msg)
}

// vaultErrors extracts Vault's error reasons from a response body, joined
// and whitespace-collapsed to one line; a non-JSON body falls back to its
// trimmed raw text.
func vaultErrors(body []byte) string {
	var e struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(body, &e); err == nil {
		// Keep only entries that still say something after whitespace
		// collapse: a redacting proxy can blank any subset of the array,
		// and joining blanks first would fabricate a bare ";" reason that
		// blocks the raw-body fallback below.
		var parts []string
		for _, msg := range e.Errors {
			if f := strings.Join(strings.Fields(msg), " "); f != "" {
				parts = append(parts, f)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return strings.Join(strings.Fields(string(body)), " ")
}

// policyHint names the capability a 403 for a VALID token points at - one
// template for every verb, so a wording change lands once.
func (h *vaultHandler) policyHint(capability, kind string) string {
	return fmt.Sprintf("if the token is valid, the policy needs %s on %s/%s/%s/*", capability, h.mount, kind, h.path)
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
		return nil, httpError("get", code, body, h.policyHint("read", "data"))
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
	code, body, err := h.do(http.MethodPost, h.url("data", serverURL, name), map[string]any{
		"data": map[string]string{"token": token, "serverURL": serverURL, "name": name},
	})
	if err != nil {
		return err
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		return httpError("store", code, body, h.policyHint("create+update+read", "data"))
	}
	return nil
}

func (h *vaultHandler) Erase(serverURL, name string) (credhelper.EraseOutcome, error) {
	// Vault's metadata DELETE is idempotent and does not say whether
	// anything existed, so ProbeErase answers through h.Get - presence here
	// can never diverge from what a get would say (a soft-deleted latest
	// version, an empty token, or a mismatched identity all read as absent
	// in both). A policy without read on the data path (a delete-only
	// service account) gets a denied read in Vault's audit log and an
	// "unknown" outcome - never a failure. Best-effort under concurrent
	// stores; see ProbeErase's doc.
	return credhelper.ProbeErase(
		func() (*credhelper.Credentials, error) { return h.Get(serverURL, name) },
		func() error {
			code, body, err := h.do(http.MethodDelete, h.url("metadata", serverURL, name), nil)
			if err != nil {
				return err
			}
			// 404 = already gone = success (idempotent).
			if code != http.StatusNoContent && code != http.StatusOK && code != http.StatusNotFound {
				return httpError("erase", code, body, h.policyHint("delete", "metadata"))
			}
			return nil
		})
}
