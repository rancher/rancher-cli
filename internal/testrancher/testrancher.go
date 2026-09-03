// Package testrancher fakes just enough of Rancher's public auth surface
// (/v1-public/*) and its ext.cattle.io/v1 Token store's imperative
// mint/revoke behavior to drive the CLI's login and get-token flows
// end-to-end without a real Rancher. It is test-only by convention: no
// production package may import it.
package testrancher

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	authProvidersPath = "/v1-public/authproviders"
	loginPath         = "/v1-public/login"

	// mintTokensPath is the ext token collection behind the local-cluster
	// proxy. It repeats production's token.MintTokensPath as a literal
	// because the token package's own tests exercise this façade, and an
	// import of token here would make that a cycle; a test asserts the two
	// stay equal.
	mintTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

	// v3TokensPath is the deprecated management.cattle.io/v3 token
	// collection behind the local-cluster proxy — where a session token
	// Rancher created through /v1-public/login lives, and therefore the
	// route that revokes it.
	v3TokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"

	// relayPathPrefix is the authtokens collection: the CLI polls
	// <prefix>/<requestID> for the encrypted bearer the browser sign-in
	// deposited.
	relayPathPrefix = "/v1-public/authtokens/"

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
	// IdPURL, when set, is the external origin (scheme://host[:port]) the
	// azureADProvider document names for its endpoints, and the façade stops
	// serving /idp/* itself; the caller serves IdPHandler there. Empty keeps
	// the stub on the façade's own origin.
	IdPURL string
	// SeedToken, when set, is accepted as a valid bearer for the mint
	// endpoint alongside façade-issued tokens — the static seed token every
	// existing e2e script already authenticates with (FA-1: "the mint
	// requires a valid façade-issued (or seed) bearer").
	SeedToken string

	// DropLoginResponse creates the token and then closes the connection
	// without writing: the client never learns what the server did.
	DropLoginResponse bool
	// MalformedLoginResponse answers a created login with a 201 whose body
	// establishes no token shape.
	MalformedLoginResponse bool
	// LoginNameOnly answers with the kind's shape carrying the resource name
	// but no bearer.
	LoginNameOnly bool
	// LoginNoIdentity answers with the kind's recognized shape carrying a
	// bearer but no resource name.
	LoginNoIdentity bool
	// LoginBearerOverride replaces the bearer in the login response with this
	// string, for a token the client cannot use or parse.
	LoginBearerOverride string
	// LoginStatus forces a status on the login POST. A deterministic 4xx
	// (400, 401, 403, 404, 409, 422) is answered before any record exists;
	// any other status is answered after, modelling a create the client never
	// learned of.
	LoginStatus int
	// LoginShape forces the login response shape and the created record's
	// kind ("ext" or "v3") regardless of the requested responseType. Empty
	// follows the response type: json is v3, kubeconfig is ext.
	LoginShape string

	// DropMintResponse creates the derived token and then closes the
	// connection without writing.
	DropMintResponse bool
	// MalformedMintResponse answers a created mint with a 201 whose body
	// establishes no token shape.
	MalformedMintResponse bool
	// MintBearerOverride replaces the bearer in the mint response with this
	// string.
	MintBearerOverride string
	// MintStatus forces a status on the mint POST, with LoginStatus's rule
	// about when the record is created.
	MintStatus int

	// SessionDeleteMode and DerivedDeleteMode fail the revoke of a located
	// record by its role, on either route: "" answers normally, "gone"
	// answers 404, "fail" answers 500.
	SessionDeleteMode string
	DerivedDeleteMode string

	// EnableSAMLRelay serves the authtokens collection the browser sign-in
	// polls.
	// Nothing is served for a request id until DepositRelay.
	EnableSAMLRelay bool
}

// record is one façade-issued token.
type record struct {
	name            string
	secret          string
	resourceVersion string
	clusterName     string
	description     string
	// kind is the resource the façade pretends to have created, "ext" or
	// "v3". It is fixed when the record is created, whatever shape the
	// client is later shown, because a server creates one definite kind.
	kind string
	// role is what the operation created the token for: "session" for the
	// login handler and the SAML authtokens flow, "derived" for the mint
	// endpoint. The
	// delete fault modes select on it.
	role      string
	expiresAt time.Time
}

// bearer renders the record's bearer in its kind's shape: an ext token
// carries the ext prefix, a v3 token is <name>:<secret>.
func (r *record) bearer() string {
	if r.kind == "v3" {
		return r.name + ":" + r.secret
	}
	return "ext/" + r.name + ":" + r.secret
}

// RelayRecord is one request the authtokens path received.
type RelayRecord struct {
	Method string
	Path   string
}

// DeleteRecord is one recorded DELETE against either token store, dry-run
// and refused attempts included, so a test can prove that the preview and
// the execution of one delete carried the same precondition.
type DeleteRecord struct {
	Kind         string // "ext" or "v3"
	Name         string
	Bearer       string
	DryRun       bool   // the body's dryRun contained "All"
	Precondition string // the body's preconditions.resourceVersion, or ""
}

// Facade is a fake Rancher auth surface. It implements http.Handler.
type Facade struct {
	opts Options
	idp  *idpStub

	mu           sync.Mutex
	counter      int
	tokens       map[string]*record
	descriptions []string
	deletes      []DeleteRecord
	relayReqs    []RelayRecord
	relay        map[string]string // request id → encrypted base64 bearer
}

// LoginDescriptions returns the descriptions login requests carried, in
// order.
func (f *Facade) LoginDescriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.descriptions)
}

// DeleteRequests returns every recorded DELETE attempt, in order.
func (f *Facade) DeleteRequests() []DeleteRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deletes)
}

// RelayRequests returns every request the authtokens path received, in
// order.
func (f *Facade) RelayRequests() []RelayRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.relayReqs)
}

// TokenCount is how many tokens the façade holds.
func (f *Facade) TokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

// TokensWithDescriptionContaining returns the resource names of the records
// of kind ("ext" or "v3") whose description contains sub.
func (f *Facade) TokensWithDescriptionContaining(kind, sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, rec := range f.tokens {
		if rec.kind == kind && strings.Contains(rec.description, sub) {
			names = append(names, rec.name)
		}
	}
	slices.Sort(names)
	return names
}

// BearersOfKind returns the bearers of every record of kind, sorted.
func (f *Facade) BearersOfKind(kind string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bearers []string
	for _, rec := range f.tokens {
		if rec.kind == kind {
			bearers = append(bearers, rec.bearer())
		}
	}
	slices.Sort(bearers)
	return bearers
}

// DepositRelay creates a v3 session token and deposits it for requestID,
// encrypted to publicKeyB64 the way the browser sign-in does.
func (f *Facade) DepositRelay(requestID, publicKeyB64 string) {
	rec := f.mintToken("", 0)
	f.mu.Lock()
	rec.kind = "v3"
	rec.role = "session"
	f.mu.Unlock()
	f.DepositRelayRaw(requestID, encryptToPublicKey(publicKeyB64, rec.bearer()))
}

// DepositRelayRaw deposits b64 for requestID as it is, for a payload the
// client cannot decrypt.
func (f *Facade) DepositRelayRaw(requestID, b64 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relay[requestID] = b64
}

// New builds a Facade from opts, applying defaults for the fields FA-1
// leaves optional.
func New(opts Options) *Facade {
	if opts.LoginUser == "" {
		opts.LoginUser = "e2e-user"
	}
	if opts.LoginPass == "" {
		opts.LoginPass = "e2e-pass"
	}
	f := &Facade{opts: opts, tokens: map[string]*record{}, relay: map[string]string{}}
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
	case r.Method == http.MethodGet && r.URL.Path == mintTokensPath:
		f.handleExtCollection(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, mintTokensPath+"/"):
		f.handleNamedGet(w, r, strings.TrimPrefix(r.URL.Path, mintTokensPath+"/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, mintTokensPath+"/"):
		f.handleDelete(w, r, "ext", strings.TrimPrefix(r.URL.Path, mintTokensPath+"/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, v3TokensPath+"/"):
		f.handleDelete(w, r, "v3", strings.TrimPrefix(r.URL.Path, v3TokensPath+"/"))
	case f.opts.EnableSAMLRelay && strings.HasPrefix(r.URL.Path, relayPathPrefix):
		f.handleRelay(w, r, strings.TrimPrefix(r.URL.Path, relayPathPrefix))
	case r.Method == http.MethodGet && r.URL.Path == CRTCollectionPath:
		f.handleCRTCollection(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, CRTCollectionPath+"/"):
		f.handleCRTSingle(w, r, strings.TrimPrefix(r.URL.Path, CRTCollectionPath+"/"))
	case r.Method == http.MethodGet && r.URL.Path == rke2ReleasePath:
		f.handleRelease(w, rke2ReleaseFixture)
	case r.Method == http.MethodGet && r.URL.Path == k3sReleasePath:
		f.handleRelease(w, k3sReleaseFixture)
	case f.idp != nil && f.opts.IdPURL == "" && r.Method == http.MethodPost && r.URL.Path == "/idp/devicecode":
		f.idp.handleDeviceCode(w, r)
	case f.idp != nil && f.opts.IdPURL == "" && r.Method == http.MethodPost && r.URL.Path == "/idp/token":
		f.idp.handleToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/v3-public"):
		http.NotFound(w, r)
	default:
		f.fallthroughTo(w, r)
	}
}

// IdPHandler serves the device-code identity-provider stub for a caller
// that hosts it on its own origin (Options.IdPURL). It answers POST
// /idp/devicecode and POST /idp/token; anything else is 404, and every
// route is 404 when the façade was built without EnableIdP.
func (f *Facade) IdPHandler() http.Handler {
	if f.idp == nil {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /idp/devicecode", f.idp.handleDeviceCode)
	mux.HandleFunc("POST /idp/token", f.idp.handleToken)
	return mux
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
		base := f.opts.IdPURL
		if base == "" {
			base = "https://" + r.Host
		}
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
		Type         string `json:"type"`
		User         string `json:"username"`
		Pass         string `json:"password"`
		IDToken      string `json:"id_token"`
		ResponseType string `json:"responseType"`
		Description  string `json:"description"`
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
	// A deterministic 4xx proves the server created nothing, so it is
	// answered before any record exists.
	if deterministic4xx(f.opts.LoginStatus) {
		w.WriteHeader(f.opts.LoginStatus)
		return
	}

	rec := f.mintToken("", 0)
	f.mu.Lock()
	rec.kind = loginKind(f.opts.LoginShape, req.ResponseType)
	rec.role = "session"
	rec.description = req.Description
	if req.Description != "" {
		f.descriptions = append(f.descriptions, req.Description)
	}
	f.mu.Unlock()

	name, bearer := rec.name, rec.bearer()
	if f.opts.LoginBearerOverride != "" {
		bearer = f.opts.LoginBearerOverride
	}
	if f.opts.LoginNameOnly {
		bearer = ""
	}
	if f.opts.LoginNoIdentity {
		name = ""
	}

	switch {
	case f.opts.LoginStatus != 0:
		// The record exists and the client learns nothing about it.
		w.WriteHeader(f.opts.LoginStatus)
	case f.opts.DropLoginResponse:
		dropConnection(w)
	case f.opts.MalformedLoginResponse:
		writeJSON(w, http.StatusCreated, map[string]any{"broken": true})
	case rec.kind == "v3":
		writeV3Token(w, http.StatusCreated, rec, name, bearer)
	default:
		writeExtTokenAs(w, http.StatusCreated, rec, name, bearer)
	}
}

// loginKind is the kind of resource a login creates: the forced shape when
// a test sets one, else the shape the response type asks for.
func loginKind(shape, responseType string) string {
	switch {
	case shape != "":
		return shape
	case responseType == "json":
		return "v3"
	default:
		return "ext"
	}
}

// deterministic4xx reports whether a status proves the request had no
// effect on the server.
func deterministic4xx(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// dropConnection closes the connection with no response, so the client
// cannot tell whether the request took effect.
func dropConnection(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
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

	if deterministic4xx(f.opts.MintStatus) {
		w.WriteHeader(f.opts.MintStatus)
		return
	}

	var mintReq struct {
		Spec struct {
			TTL         int64  `json:"ttl"`
			ClusterName string `json:"clusterName"`
			Description string `json:"description"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(body, &mintReq)
	rec := f.mintToken(mintReq.Spec.ClusterName, mintReq.Spec.TTL)
	f.mu.Lock()
	rec.kind = "ext"
	rec.role = "derived"
	rec.description = mintReq.Spec.Description
	f.mu.Unlock()

	switch {
	case f.opts.MintStatus != 0:
		w.WriteHeader(f.opts.MintStatus)
	case f.opts.DropMintResponse:
		dropConnection(w)
	case f.opts.MalformedMintResponse:
		writeJSON(w, http.StatusCreated, map[string]any{"broken": true})
	default:
		bearer := rec.bearer()
		if f.opts.MintBearerOverride != "" {
			bearer = f.opts.MintBearerOverride
		}
		writeExtTokenAs(w, http.StatusCreated, rec, rec.name, bearer)
	}
}

// handleExtCollection lists every ext record as a TokenList. With a
// fallthrough, the records the real apiserver holds are listed first and
// the façade's own appended, so a listing shows both.
func (f *Facade) handleExtCollection(w http.ResponseWriter, r *http.Request) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	items := []any{}
	if f.opts.Fallthrough != nil {
		rec := httptest.NewRecorder()
		f.opts.Fallthrough.ServeHTTP(rec, r)
		var upstream struct {
			Items []any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &upstream); err == nil {
			items = append(items, upstream.Items...)
		}
	}
	f.mu.Lock()
	names := make([]string, 0, len(f.tokens))
	for name, tok := range f.tokens {
		if tok.kind == "ext" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		items = append(items, tokenCRBody(f.tokens[name]))
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "TokenList",
		"items":      items,
	})
}

// handleRelay serves the authtokens collection: 404 until a token is
// deposited for the request id, then the encrypted bearer, and a DELETE
// removes it. Every request is recorded before it is answered.
func (f *Facade) handleRelay(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	f.relayReqs = append(f.relayReqs, RelayRecord{Method: r.Method, Path: r.URL.Path})
	payload, ok := f.relay[id]
	if r.Method == http.MethodDelete {
		delete(f.relay, id)
	}
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusOK)
	case !ok:
		http.NotFound(w, r)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"token":     payload,
			"expiresAt": time.Now().Add(defaultMintTTL).UTC().Format(time.RFC3339),
		})
	}
}

// encryptToPublicKey encrypts plaintext to the base64-encoded JSON RSA
// public key the browser sign-in publishes, the way Rancher deposits an
// auth token. A key the client cannot parse yields an empty payload,
// which the client reads as nothing deposited yet.
func encryptToPublicKey(publicKeyB64, plaintext string) string {
	raw, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return ""
	}
	var pub rsa.PublicKey
	if err := json.Unmarshal(raw, &pub); err != nil {
		return ""
	}
	cipher, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &pub, []byte(plaintext), nil)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(cipher)
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

// handleDelete serves a DELETE against either token store. The attempt is
// recorded before anything can reject it, so a refused, dry-run, or absent
// target is recorded too, and a test can prove that the preview and the
// execution of one delete carried the same precondition.
func (f *Facade) handleDelete(w http.ResponseWriter, r *http.Request, kind, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var opts deleteOptions
	if len(body) > 0 {
		_ = json.Unmarshal(body, &opts)
	}
	precondition := ""
	if opts.Preconditions != nil && opts.Preconditions.ResourceVersion != nil {
		precondition = *opts.Preconditions.ResourceVersion
	}
	dryRun := slices.Contains(opts.DryRun, "All")

	f.mu.Lock()
	f.deletes = append(f.deletes, DeleteRecord{
		Kind:         kind,
		Name:         name,
		Bearer:       bearerFromHeader(r),
		DryRun:       dryRun,
		Precondition: precondition,
	})
	f.mu.Unlock()

	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	rec, ok := f.tokens[name]
	if ok && rec.kind != kind {
		ok = false
	}
	f.mu.Unlock()
	if !ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fallthroughTo(w, r)
		return
	}

	switch f.deleteMode(rec) {
	case "gone":
		http.NotFound(w, r)
		return
	case "fail":
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if precondition != "" && precondition != rec.resourceVersion {
		w.WriteHeader(http.StatusConflict)
		return
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

// deleteMode is the fault mode configured for the located record's role,
// so an ext-shaped session token fails where the derived token succeeds.
func (f *Facade) deleteMode(rec *record) string {
	switch rec.role {
	case "session":
		return f.opts.SessionDeleteMode
	case "derived":
		return f.opts.DerivedDeleteMode
	}
	return ""
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

// crtFixtures spans multiple namespaces (Task 5, spec D-4 review R3-3's
// namespace-filter test fixture set): "c-fixture" is the namespace
// e2e/scripts/registration-token.txtar addresses; "c-other" exists purely
// so a bare `get clusterregistrationtoken` (no --namespace) proves more
// than one namespace is present, and so a --namespace c-fixture read never
// leaks c-other's token.
var crtFixtures = []crtFixture{
	{Namespace: "c-fixture", Name: "default-token", Token: "e2e-crt-real-token-c-fixture"},
	{Namespace: "c-other", Name: "default-token", Token: "e2e-crt-real-token-c-other"},
	{Namespace: "c-import", Name: "default-token", Token: "e2e-crt-real-token-c-import"},
}

// crtStatus builds a clusterregistrationtoken status block with token
// spliced into the seven command/manifest fields, as the real formatter
// serves them. status.token is the splice SOURCE the real formatter reads
// to perform the ReplaceAll.
func crtStatus(token string) map[string]any {
	return map[string]any{
		"command":                    "curl --insecure -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"insecureCommand":            "curl -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"nodeCommand":                "sudo docker run -d --privileged --net=host --name=cattle-agent rancher/rancher-agent --server https://rancher.example --token " + token,
		"insecureNodeCommand":        "sudo docker run -d --name=cattle-agent rancher/rancher-agent --token " + token,
		"windowsNodeCommand":         "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"insecureWindowsNodeCommand": "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"manifestUrl":                "https://rancher.example/v3/import/" + token + ".yaml",
		"token":                      token,
		"expiresAt":                  "2099-01-01T00:00:00Z",
	}
}

func crtBody(fx crtFixture) map[string]any {
	return map[string]any{
		"id":       fx.Namespace + "/" + fx.Name,
		"metadata": map[string]any{"namespace": fx.Namespace, "name": fx.Name},
		"spec":     map[string]any{"clusterName": fx.Namespace},
		"status":   crtStatus(fx.Token),
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
		data = append(data, crtBody(fx))
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
				writeJSON(w, http.StatusOK, crtBody(fx))
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
		name:            fmt.Sprintf("fac-token-%d", n),
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
	spec := map[string]any{"clusterName": rec.clusterName}
	if rec.description != "" {
		spec["description"] = rec.description
	}
	return map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   map[string]any{"name": rec.name, "resourceVersion": rec.resourceVersion},
		"spec":       spec,
		"status":     map[string]any{"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339)},
	}
}

// writeExtTokenAs writes the ext Token object with the given identity: the
// resource's name in metadata and its bearer in status. Either may be
// empty, which withholds that field, for a response the client cannot
// fully identify.
func writeExtTokenAs(w http.ResponseWriter, status int, rec *record, name, bearer string) {
	statusBlock := map[string]any{"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339)}
	if bearer != "" {
		statusBlock["bearerToken"] = bearer
	}
	metadata := map[string]any{"resourceVersion": rec.resourceVersion}
	if name != "" {
		metadata["name"] = name
	}
	writeJSON(w, status, map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   metadata,
		"status":     statusBlock,
	})
}

// writeV3Token writes the Norman token shape a responseType json login
// returns: the bearer under "token" and the resource name under "id".
// Either may be empty, which withholds that field.
func writeV3Token(w http.ResponseWriter, status int, rec *record, name, bearer string) {
	body := map[string]any{
		"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339),
		"baseType":  "token",
		"type":      "token",
	}
	if name != "" {
		body["id"] = name
	}
	if bearer != "" {
		body["token"] = bearer
	}
	writeJSON(w, status, body)
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
