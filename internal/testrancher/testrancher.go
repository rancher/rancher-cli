// Package testrancher fakes just enough of Rancher's public auth surface
// (/v1-public/*) and its ext.cattle.io/v1 Token store's imperative
// mint/revoke behavior to drive the CLI's login and get-token flows
// end-to-end without a real Rancher. It is test-only by convention: no
// production package may import it.
package testrancher

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	authProvidersPath = "/v1-public/authproviders"
	loginPath         = "/v1-public/login"

	// mintTokensPath mirrors token.MintTokensPath's literal value. It is
	// duplicated rather than imported so this test double stays decoupled
	// from the production package it fakes.
	mintTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

	defaultMintTTL = 15 * time.Minute
)

// Options configures a Facade instance. Every façade instance is meant to be
// per-script (FA-3): its counters and token store are not safe to share
// across concurrent scripts.
type Options struct {
	// Fallthrough handles every request the façade does not own: the
	// existing bearer-checked router in front of envtest.
	Fallthrough http.Handler
	// Legacy makes the mint branch of the token POST 404 (ErrExtGroupAbsent),
	// while CR-create fallthrough is unaffected.
	Legacy bool
	// NamePrefix names minted tokens ("<prefix>-<n>"); defaults to
	// "fac-token". Minted secrets always use "fac-secret-<n>".
	NamePrefix string
	// LoginUser/LoginPass are the fixed local-provider credentials; default
	// "e2e-user"/"e2e-pass".
	LoginUser string
	LoginPass string
	// EnableIdP turns on the azureADProvider entry and the /idp/* device-code
	// stub.
	EnableIdP bool
	// IdPPendingPolls is how many /idp/token polls return
	// authorization_pending before a token is issued; defaults to 2.
	IdPPendingPolls int
	// SeedToken, when set, is accepted as a valid bearer for the mint
	// endpoint alongside façade-issued tokens — the static seed token every
	// existing e2e script already authenticates with (FA-1: "the mint
	// requires a valid façade-issued (or seed) bearer").
	SeedToken string
}

// record is one façade-issued token.
type record struct {
	name            string
	secret          string
	resourceVersion string
	clusterName     string
	expiresAt       time.Time
}

func (r *record) bearer() string { return "ext/" + r.name + ":" + r.secret }

// Facade is a fake Rancher auth surface. It implements http.Handler.
type Facade struct {
	opts Options
	idp  *idpStub

	mu      sync.Mutex
	counter int
	tokens  map[string]*record
}

// New builds a Facade from opts, applying defaults for the fields FA-1
// leaves optional.
func New(opts Options) *Facade {
	if opts.NamePrefix == "" {
		opts.NamePrefix = "fac-token"
	}
	if opts.LoginUser == "" {
		opts.LoginUser = "e2e-user"
	}
	if opts.LoginPass == "" {
		opts.LoginPass = "e2e-pass"
	}
	f := &Facade{opts: opts, tokens: map[string]*record{}}
	if opts.EnableIdP {
		f.idp = newIdPStub(opts.IdPPendingPolls)
	}
	return f
}

// ValidBearer reports whether token is a façade-issued, unrevoked, unexpired
// bearer (instance clock). Revoked tokens are removed from the store, so
// "unrevoked" and "present" are the same condition here.
func (f *Facade) ValidBearer(token string) bool {
	if token == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.tokens {
		if rec.bearer() == token {
			return time.Now().Before(rec.expiresAt)
		}
	}
	return false
}

// ServeHTTP dispatches auth paths to the façade's own handlers; everything
// else falls through to opts.Fallthrough (FA-0: layer, don't grow).
func (f *Facade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == authProvidersPath:
		f.handleAuthProviders(w, r)
	case r.Method == http.MethodPost && r.URL.Path == loginPath:
		f.handleLogin(w, r)
	case r.Method == http.MethodPost && r.URL.Path == mintTokensPath:
		f.handleTokensPOST(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, mintTokensPath+"/"):
		f.handleNamedGet(w, r, strings.TrimPrefix(r.URL.Path, mintTokensPath+"/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, mintTokensPath+"/"):
		f.handleDelete(w, r, strings.TrimPrefix(r.URL.Path, mintTokensPath+"/"))
	case f.idp != nil && r.Method == http.MethodPost && r.URL.Path == "/idp/devicecode":
		f.idp.handleDeviceCode(w, r)
	case f.idp != nil && r.Method == http.MethodPost && r.URL.Path == "/idp/token":
		f.idp.handleToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/v3-public"):
		http.NotFound(w, r)
	default:
		f.fallthroughTo(w, r)
	}
}

func (f *Facade) fallthroughTo(w http.ResponseWriter, r *http.Request) {
	if f.opts.Fallthrough == nil {
		http.NotFound(w, r)
		return
	}
	f.opts.Fallthrough.ServeHTTP(w, r)
}

func (f *Facade) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{{"type": "localProvider"}}
	if f.opts.EnableIdP {
		base := "https://" + r.Host
		data = append(data, map[string]any{
			"type":          "azureADProvider",
			"redirectUrl":   "",
			"clientId":      "facade-client",
			"scopes":        []string{"openid"},
			"authUrl":       base + "/idp/authorize",
			"deviceAuthUrl": base + "/idp/devicecode",
			"tokenUrl":      base + "/idp/token",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (f *Facade) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req struct {
		Type    string `json:"type"`
		User    string `json:"username"`
		Pass    string `json:"password"`
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	switch req.Type {
	case "localProvider":
		if req.User != f.opts.LoginUser || req.Pass != f.opts.LoginPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	case "azureADProvider":
		if f.idp == nil || !f.idp.validIDToken(req.IDToken) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	default:
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rec := f.mintToken("", 0)
	writeExtToken(w, http.StatusCreated, rec)
}

func (f *Facade) handleTokensPOST(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	// A full CR (create resource -f) carries metadata.name; a mint request
	// (MintDerivedToken) is a bare {"spec": ...} body. Restore the body
	// before any fallthrough so the real handler can read it again.
	var probe struct {
		Metadata *struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Metadata != nil && probe.Metadata.Name != "" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fallthroughTo(w, r)
		return
	}

	if f.opts.Legacy {
		http.NotFound(w, r)
		return
	}
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var mintReq struct {
		Spec struct {
			TTL         int64  `json:"ttl"`
			ClusterName string `json:"clusterName"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(body, &mintReq)
	rec := f.mintToken(mintReq.Spec.ClusterName, mintReq.Spec.TTL)
	writeExtToken(w, http.StatusCreated, rec)
}

func (f *Facade) handleNamedGet(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	rec, ok := f.tokens[name]
	f.mu.Unlock()
	if !ok {
		f.fallthroughTo(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   map[string]any{"name": rec.name, "resourceVersion": rec.resourceVersion},
		"spec":       map[string]any{"clusterName": rec.clusterName},
		"status":     map[string]any{"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339)},
	})
}

type deleteOptions struct {
	DryRun        []string `json:"dryRun"`
	Preconditions *struct {
		ResourceVersion *string `json:"resourceVersion"`
	} `json:"preconditions"`
}

func (f *Facade) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	rec, ok := f.tokens[name]
	f.mu.Unlock()
	if !ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fallthroughTo(w, r)
		return
	}

	var opts deleteOptions
	if len(body) > 0 {
		_ = json.Unmarshal(body, &opts)
	}
	if opts.Preconditions != nil && opts.Preconditions.ResourceVersion != nil &&
		*opts.Preconditions.ResourceVersion != rec.resourceVersion {
		w.WriteHeader(http.StatusConflict)
		return
	}

	dryRun := false
	for _, v := range opts.DryRun {
		if v == "All" {
			dryRun = true
		}
	}
	if dryRun {
		writeJSON(w, http.StatusOK, tokenCRBody(rec))
		return
	}

	f.mu.Lock()
	delete(f.tokens, name)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, tokenCRBody(rec))
}

func (f *Facade) authorizedBearer(r *http.Request) bool {
	tok := bearerFromHeader(r)
	if tok == "" {
		return false
	}
	if f.opts.SeedToken != "" && tok == f.opts.SeedToken {
		return true
	}
	return f.ValidBearer(tok)
}

func bearerFromHeader(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimPrefix(h, prefix)
}

// mintToken issues the next deterministic name/secret pair from the
// instance-local counter, shared by /v1-public/login (the session token) and
// the mint endpoint (the derived token) — the same mechanism that makes a
// provider login consume two counter slots.
func (f *Facade) mintToken(clusterName string, ttlMs int64) *record {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counter++
	n := f.counter
	rec := &record{
		name:            fmt.Sprintf("%s-%d", f.opts.NamePrefix, n),
		secret:          fmt.Sprintf("fac-secret-%d", n),
		resourceVersion: "1",
		clusterName:     clusterName,
	}
	if ttlMs > 0 {
		rec.expiresAt = time.Now().Add(time.Duration(ttlMs) * time.Millisecond)
	} else {
		rec.expiresAt = time.Now().Add(defaultMintTTL)
	}
	f.tokens[rec.name] = rec
	return rec
}

func tokenCRBody(rec *record) map[string]any {
	return map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   map[string]any{"name": rec.name, "resourceVersion": rec.resourceVersion},
		"spec":       map[string]any{"clusterName": rec.clusterName},
		"status":     map[string]any{"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339)},
	}
}

func writeExtToken(w http.ResponseWriter, status int, rec *record) {
	writeJSON(w, status, map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"status": map[string]any{
			"bearerToken": rec.bearer(),
			"expiresAt":   rec.expiresAt.UTC().Format(time.RFC3339),
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
