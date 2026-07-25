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

	"github.com/rancher/rancher-cli/internal/token"
)

const (
	authProvidersPath = "/v1-public/authproviders"
	loginPath         = "/v1-public/login"

	// mintTokensPath is production's own constant, not a duplicated literal:
	// the architecture conformance edge only forbids a production package
	// importing testrancher, not the reverse, so this is the one place the
	// path lives.
	mintTokensPath = token.MintTokensPath

	defaultMintTTL = 15 * time.Minute

	// CRTCollectionPath is the Steve /v1 collection route for
	// clusterregistrationtokens (Task 5, spec D-4). It must match
	// internal/engine/provisioning's own crtCollectionPath — duplicated there
	// rather than imported, since production code cannot import this test-only
	// package. internal/testrancher/fidelity consumes THIS constant (it may
	// import its parent), so the only remaining intentional duplicate is
	// production's copy, not a third one on the test side.
	CRTCollectionPath = "/v1/management.cattle.io.clusterregistrationtokens"

	// rke2ReleasePath/k3sReleasePath are the channel-server release routes
	// (Task 6, spec D-7). Must match internal/engine/provisioning's own
	// rke2ReleasePath/k3sReleasePath constants — duplicated here for the
	// same reason as crtCollectionPath above.
	rke2ReleasePath = "/v1-rke2-release/release"
	k3sReleasePath  = "/v1-k3s-release/release"
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
	// CRTUnformatted makes the clusterregistrationtoken Steve route serve
	// its fixture objects with the seven command/manifest fields still
	// carrying the k8s proxy's own literal "{token}" placeholder — i.e. as
	// if Steve's splicing formatter never ran (rancher
	// clusterregistrationtoken.go:24-72). Exercises `get
	// clusterregistrationtoken`'s placeholder guard end-to-end; false
	// (the default) serves fully spliced fixtures.
	CRTUnformatted bool
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
func (f *Facade) ValidBearer(bearer string) bool {
	if bearer == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.tokens {
		if rec.bearer() == bearer {
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
	case r.Method == http.MethodGet && r.URL.Path == CRTCollectionPath:
		f.handleCRTCollection(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, CRTCollectionPath+"/"):
		f.handleCRTSingle(w, r, strings.TrimPrefix(r.URL.Path, CRTCollectionPath+"/"))
	case r.Method == http.MethodGet && r.URL.Path == rke2ReleasePath:
		f.handleRelease(w, rke2ReleaseFixture)
	case r.Method == http.MethodGet && r.URL.Path == k3sReleasePath:
		f.handleRelease(w, k3sReleaseFixture)
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
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	rec, ok := f.tokens[name]
	f.mu.Unlock()
	if !ok {
		f.fallthroughTo(w, r)
		return
	}
	writeJSON(w, http.StatusOK, tokenCRBody(rec))
}

type deleteOptions struct {
	DryRun        []string `json:"dryRun"`
	Preconditions *struct {
		ResourceVersion *string `json:"resourceVersion"`
	} `json:"preconditions"`
}

func (f *Facade) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
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

// crtFixture is one façade-served clusterregistrationtoken. Token is the
// deterministic real value spliced into the seven command/manifest fields,
// mirroring rancher's own steve formatter (clusterregistrationtoken.go:
// 24-72, the ReplaceAll at :69). ClusterName mirrors the real CRD's
// spec.clusterName, which always equals the CRT's own management namespace.
type crtFixture struct {
	Namespace string
	Name      string
	Token     string
}

// crtFixtures spans TWO namespaces (Task 5, spec D-4 review R3-3's
// namespace-filter test fixture set): "c-fixture" is the namespace
// e2e/scripts/registration-token.txtar addresses; "c-other" exists purely
// so a bare `get clusterregistrationtoken` (no --namespace) proves more
// than one namespace is present, and so a --namespace c-fixture read never
// leaks c-other's token.
var crtFixtures = []crtFixture{
	{Namespace: "c-fixture", Name: "default-token", Token: "e2e-crt-real-token-c-fixture"},
	{Namespace: "c-other", Name: "default-token", Token: "e2e-crt-real-token-c-other"},
}

// crtStatus builds a clusterregistrationtoken status block. unformatted
// serves the k8s proxy's own literal "{token}" placeholder in the seven
// command/manifest fields instead of splicing in token — as if Steve's
// formatter never ran — for CRTUnformatted's placeholder-guard path.
// status.token is never the placeholder itself: it is the splice SOURCE the
// real formatter reads to perform the ReplaceAll.
func crtStatus(token string, unformatted bool) map[string]any {
	val := token
	if unformatted {
		val = "{token}"
	}
	return map[string]any{
		"command":                    "curl --insecure -sfL https://rancher.example/v3/import/" + val + ".yaml | kubectl apply -f -",
		"insecureCommand":            "curl -sfL https://rancher.example/v3/import/" + val + ".yaml | kubectl apply -f -",
		"nodeCommand":                "sudo docker run -d --privileged --net=host --name=cattle-agent rancher/rancher-agent --server https://rancher.example --token " + val,
		"insecureNodeCommand":        "sudo docker run -d --name=cattle-agent rancher/rancher-agent --token " + val,
		"windowsNodeCommand":         "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + val,
		"insecureWindowsNodeCommand": "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + val,
		"manifestUrl":                "https://rancher.example/v3/import/" + val + ".yaml",
		"token":                      token,
		"expiresAt":                  "2099-01-01T00:00:00Z",
	}
}

func crtBody(fx crtFixture, unformatted bool) map[string]any {
	return map[string]any{
		"id":       fx.Namespace + "/" + fx.Name,
		"metadata": map[string]any{"namespace": fx.Namespace, "name": fx.Name},
		"spec":     map[string]any{"clusterName": fx.Namespace},
		"status":   crtStatus(fx.Token, unformatted),
	}
}

// handleCRTCollection serves the Steve /v1 collection envelope for every
// fixture in crtFixtures — the caller (NewGetCRT) filters by namespace
// locally, so the façade always returns the full set regardless of query
// string.
func (f *Facade) handleCRTCollection(w http.ResponseWriter, r *http.Request) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	data := make([]map[string]any, 0, len(crtFixtures))
	for _, fx := range crtFixtures {
		data = append(data, crtBody(fx, f.opts.CRTUnformatted))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// handleCRTSingle serves one fixture at its namespaced single-object path
// ("<ns>/<name>", already trimmed of the collection prefix); an unknown
// namespace/name pair 404s.
func (f *Facade) handleCRTSingle(w http.ResponseWriter, r *http.Request, rest string) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ns, name, ok := strings.Cut(rest, "/")
	if ok {
		for _, fx := range crtFixtures {
			if fx.Namespace == ns && fx.Name == name {
				writeJSON(w, http.StatusOK, crtBody(fx, f.opts.CRTUnformatted))
				return
			}
		}
	}
	http.NotFound(w, r)
}

// rke2ReleaseFixture/k3sReleaseFixture are the two deterministic versions
// each channel-server route serves (Task 6, spec D-7): raw
// "vMAJOR.MINOR.PATCH+<distro>rN"-shaped strings, exactly the form
// internal/engine/provisioning's normalizeVersion strips down to
// MAJOR.MINOR.PATCH before any supported-range comparison.
var (
	rke2ReleaseFixture = []string{"v1.30.5+rke2r1", "v1.30.6+rke2r1"}
	k3sReleaseFixture  = []string{"v1.30.5+k3s1", "v1.30.6+k3s1"}
)

// handleRelease serves a channel-server release collection envelope
// ({"data":[{"version": "..."}, ...]}) — the "data" key is pinned to match
// internal/engine/provisioning's fetchReleaseVersions decode (channelserver
// model/config.go:22-30's own collection key; the acceptance pass against a
// real server corrects this fixture if the real shape differs). Unlike the
// façade's other routes, this one is deliberately UNAUTHENTICATED: rancher's
// routes.go:139-140 registers it as a bare mux route server-side, so no
// authorizedBearer check gates it here — RootClient still sends credentials
// to it (spec D-10's one-policy rule), but the fixture must accept the
// request regardless, exactly like the real server would.
func (f *Facade) handleRelease(w http.ResponseWriter, versions []string) {
	data := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		data = append(data, map[string]any{"version": v})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
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
	enc := json.NewEncoder(w)
	// This JSON is a test fixture read by the CLI, not embedded in HTML — keep
	// `<`, `>`, `&` literal, matching internal/output/render.go's convention.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
