package fidelity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/rancher/rancher-cli/internal/token"
)

func jsonMarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder appends a newline; trim it so callers get compact output.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// RealTarget is the recorded oracle: a real Rancher reached through the
// E2E_REAL_* environment scripts/e2e-real exports. Standard and revoked
// bearers are provisioned lazily against the live server and memoized.
type RealTarget struct {
	baseURL string
	client  *http.Client
	admin   string

	mu               sync.Mutex
	standard         string
	standardPassword string
	revoked          string
}

// NewRealTargetFromEnv builds the oracle client from E2E_REAL_URL,
// E2E_REAL_ADMIN_TOKEN, and E2E_REAL_CACERT (inline PEM). The CA is
// mandatory: this harness never skips TLS verification.
func NewRealTargetFromEnv() (*RealTarget, error) {
	base := os.Getenv("E2E_REAL_URL")
	admin := os.Getenv("E2E_REAL_ADMIN_TOKEN")
	ca := os.Getenv("E2E_REAL_CACERT")
	if base == "" || admin == "" || ca == "" {
		return nil, fmt.Errorf("E2E_REAL_URL, E2E_REAL_ADMIN_TOKEN, and E2E_REAL_CACERT must all be set (run via scripts/e2e-real)")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca)) {
		return nil, fmt.Errorf("E2E_REAL_CACERT: no certificates parsed")
	}
	return &RealTarget{
		baseURL: strings.TrimSuffix(base, "/"),
		admin:   admin,
		client: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		},
	}, nil
}

func (t *RealTarget) BaseURL() string      { return t.baseURL }
func (t *RealTarget) Client() *http.Client { return t.client }

func (t *RealTarget) Bearer(ctx context.Context, id Identity) (string, error) {
	switch id {
	case IdentityAdmin:
		return t.admin, nil
	case IdentityStandard:
		return t.standardBearer(ctx)
	case IdentityRevoked:
		return t.revokedBearer(ctx)
	default:
		return "", fmt.Errorf("identity %q is resolved by the executor, not the target", id)
	}
}

const standardUsername = "e2e-fid-standard"

// newStandardPassword mints a fresh random password for the throwaway standard
// user. It is generated per RealTarget instance rather than hardcoded so no
// usable credential for a provisioned user lives in this (public) source.
func newStandardPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating standard-user password: %w", err)
	}
	return "e2e-fid-standard-pw-" + hex.EncodeToString(b[:]), nil
}

// standardBearer provisions the e2e-fid-standard local user (idempotently:
// delete-before-create keeps kept-container reruns clean), binds the
// baseline `user` global role — a bare /v3/users create attaches no global
// role bindings, and a role-less user is not the spec's "standard identity":
// it gets schema-hidden responses (an empty 200 collection, a bodyless 404)
// rather than a baseline user's, so the role realizes that identity — and
// logs in as it. Cleanup relies on Rancher cascading a deleted user's global
// role bindings.
func (t *RealTarget) standardBearer(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.standard != "" {
		return t.standard, nil
	}
	if t.standardPassword == "" {
		pw, err := newStandardPassword()
		if err != nil {
			return "", err
		}
		t.standardPassword = pw
	}
	if err := t.deleteStandardUser(ctx); err != nil {
		return "", err
	}
	create := map[string]any{
		"type":               "user",
		"enabled":            true,
		"mustChangePassword": false,
		"username":           standardUsername,
		"password":           t.standardPassword,
	}
	created, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/users", create, http.StatusCreated)
	if err != nil {
		return "", fmt.Errorf("creating standard user: %w", err)
	}
	userID, _ := created["id"].(string)
	if userID == "" {
		return "", fmt.Errorf("creating standard user: create response has no id")
	}
	grb := map[string]any{
		"type":         "globalRoleBinding",
		"globalRoleId": "user",
		"userId":       userID,
	}
	if _, _, err := t.adminJSON(ctx, http.MethodPost, "/v3/globalrolebindings", grb, http.StatusCreated); err != nil {
		return "", fmt.Errorf("binding baseline user role to standard user: %w", err)
	}
	bearer, err := t.login(ctx, standardUsername, t.standardPassword)
	if err != nil {
		return "", err
	}
	t.standard = bearer
	return bearer, nil
}

// revokedBearer mints a derived token as admin, then revokes it by the name
// embedded in the ext bearer.
func (t *RealTarget) revokedBearer(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.revoked != "" {
		return t.revoked, nil
	}
	body, _, err := t.adminJSON(ctx, http.MethodPost, token.MintTokensPath, map[string]any{"spec": map[string]any{}}, http.StatusCreated)
	if err != nil {
		return "", fmt.Errorf("minting token to revoke: %w", err)
	}
	bearer := extBearerFrom(body)
	name, ok := NameFromExtBearer(bearer)
	if !ok {
		return "", fmt.Errorf("mint response bearer is not ext-shaped (credential value withheld)")
	}
	if _, _, err := t.adminJSON(ctx, http.MethodDelete, token.MintTokensPath+"/"+name, nil, http.StatusOK); err != nil {
		return "", fmt.Errorf("revoking token %s: %w", name, err)
	}
	t.revoked = bearer
	return bearer, nil
}

// deleteStandardUser removes any prior e2e-fid-standard user by username
// lookup. An absent user (empty data array) is clean; every HTTP failure,
// malformed 200 shape, or rejected delete is surfaced — a silently-kept
// stale user would break kept-run recovery at the subsequent create.
func (t *RealTarget) deleteStandardUser(ctx context.Context) error {
	body, status, err := t.adminJSON(ctx, http.MethodGet, "/v3/users?username="+standardUsername, nil, 0)
	if err != nil {
		return fmt.Errorf("looking up standard user: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("looking up standard user: status %d", status)
	}
	data, ok := body["data"].([]any)
	if !ok {
		return fmt.Errorf("looking up standard user: 200 response has no data array")
	}
	for _, el := range data {
		m, _ := el.(map[string]any)
		id, _ := m["id"].(string)
		if id == "" {
			return fmt.Errorf("looking up standard user: collection entry without id")
		}
		_, dstatus, derr := t.adminJSON(ctx, http.MethodDelete, "/v3/users/"+id, nil, 0)
		if derr != nil {
			return fmt.Errorf("deleting stale standard user %s: %w", id, derr)
		}
		switch dstatus {
		case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		default:
			return fmt.Errorf("deleting stale standard user %s: status %d", id, dstatus)
		}
	}
	return nil
}

// login posts local-provider credentials and returns the bearer, accepting
// both the Norman ({token}) and ext ({status.bearerToken}) response shapes —
// the same tolerance internal/token's parseLoginResponse implements.
func (t *RealTarget) login(ctx context.Context, user, pass string) (string, error) {
	payload := map[string]any{"type": "localProvider", "username": user, "password": pass}
	body, status, err := t.plainJSON(ctx, http.MethodPost, "/v1-public/login", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("login as %s: status %d", user, status)
	}
	if bearer := extBearerFrom(body); bearer != "" {
		return bearer, nil
	}
	if tok, _ := body["token"].(string); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("login as %s: no token in response", user)
}

func extBearerFrom(body map[string]any) string {
	st, _ := body["status"].(map[string]any)
	bearer, _ := st["bearerToken"].(string)
	return bearer
}

// adminJSON sends an admin-authenticated JSON request. wantStatus 0 skips
// the status check.
func (t *RealTarget) adminJSON(ctx context.Context, method, path string, payload map[string]any, wantStatus int) (map[string]any, int, error) {
	return t.doJSON(ctx, method, path, payload, t.admin, wantStatus)
}

func (t *RealTarget) plainJSON(ctx context.Context, method, path string, payload map[string]any) (map[string]any, int, error) {
	return t.doJSON(ctx, method, path, payload, "", 0)
}

func (t *RealTarget) doJSON(ctx context.Context, method, path string, payload map[string]any, bearer string, wantStatus int) (map[string]any, int, error) {
	var marshaled []byte
	if payload != nil {
		var err error
		marshaled, err = json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("encoding %s %s: %w", method, path, err)
		}
	}
	build := func() (*http.Request, error) {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(marshaled)
		}
		req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, body)
		if err != nil {
			return nil, fmt.Errorf("building %s %s: %w", method, path, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		return req, nil
	}
	resp, err := doResilient(t.client, build)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading %s %s response: %w", method, path, err)
	}
	if wantStatus != 0 && resp.StatusCode != wantStatus {
		return nil, resp.StatusCode, fmt.Errorf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantStatus, redactBody(raw))
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return decoded, resp.StatusCode, nil
}

// redactBody scrubs credential fields from a response body before it is
// embedded in an error message; non-JSON bodies are summarized, never echoed
// — an unexpected-status response can still carry a live bearer.
func redactBody(raw []byte) string {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Sprintf("<non-JSON body, %d bytes>", len(raw))
	}
	Normalize(decoded, DefaultNormalizeRules())
	out, err := jsonMarshalNoEscape(decoded)
	if err != nil {
		return fmt.Sprintf("<unrenderable body, %d bytes>", len(raw))
	}
	return string(out)
}
