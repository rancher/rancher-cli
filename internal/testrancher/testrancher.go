// Package testrancher fakes just enough of Rancher's public auth surface
// (/v1-public/*) and its ext.cattle.io/v1 Token store's imperative
// create/revoke behavior to drive the CLI's login and get-token flows
// end-to-end without a real Rancher. It is test-only by convention: no
// production package may import it.
package testrancher

import (
	"bytes"
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	authProvidersPath = "/v1-public/authproviders"
	loginPath         = "/v1-public/login"

	// extTokensPath is the ext token collection behind the local-cluster
	// proxy. It repeats production's token.ExtTokensPath as a literal
	// because the token package's own tests exercise this facade, and an
	// import of token here would make that a cycle; a test asserts the two
	// stay equal.
	extTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

	// v3TokensPath is the deprecated management.cattle.io/v3 token
	// collection behind the local-cluster proxy - where a session token
	// Rancher created through /v1-public/login lives, and therefore the
	// route that revokes it.
	v3TokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"

	// normanTokensPath is the Norman v3 token create route.
	normanTokensPath = "/v3/tokens"

	// authTokensPathPrefix is the authtokens collection: the CLI polls
	// <prefix>/<requestID> for the encrypted bearer the browser sign-in
	// deposited.
	authTokensPathPrefix = "/v1-public/authtokens/"

	defaultDerivedTTL = 15 * time.Minute

	// CRTCollectionPath is the Steve /v1 collection route for
	// clusterregistrationtokens. It must match
	// internal/engine/provisioning's own crtCollectionPath - duplicated there
	// rather than imported, since production code cannot import this test-only
	// package. internal/testrancher/fidelity consumes THIS constant (it may
	// import its parent), so the only remaining intentional duplicate is
	// production's copy, not a third one on the test side.
	CRTCollectionPath = "/v1/management.cattle.io.clusterregistrationtokens"
	// ClustersCollectionPath is the steve collection of management Clusters
	// the CLI reads for the clusters a caller can see. The facade serves it
	// only when Options.ClusterVisibility says what each bearer sees.
	ClustersCollectionPath = "/v1/management.cattle.io.clusters"
	// ProjectsCollectionPath is the steve collection of management Projects
	// on every cluster; ProjectsCollectionPath/<cluster> is the collection of
	// one cluster's projects.
	ProjectsCollectionPath = "/v1/management.cattle.io.projects"
	// LocalProjectsPath is the Kubernetes path of the local cluster's
	// management Projects behind the local-cluster proxy; the facade answers
	// a GET by id under it for its built-in projects.
	LocalProjectsPath = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/local/projects"

	// rke2ReleasePath/k3sReleasePath are the channel-server release routes.
	// Must match internal/engine/provisioning's own
	// rke2ReleasePath/k3sReleasePath constants - duplicated here for the
	// same reason as crtCollectionPath above.
	rke2ReleasePath = "/v1-rke2-release/release"
	k3sReleasePath  = "/v1-k3s-release/release"

	// KubeconfigsCollectionPath is the ext Kubeconfig collection behind the
	// local-cluster proxy. `create kubeconfig` posts an unnamed object here
	// (the store ignores metadata.name/generateName, like Token); a POST
	// carrying its own metadata.name (`create resource -f`) falls through to
	// envtest's real kubeconfigs.ext.cattle.io CRD instead. Exported for
	// internal/testrancher/fidelity, which may import its parent.
	KubeconfigsCollectionPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/kubeconfigs"

	// PasswordChangeRequestsCollectionPath is the ext PasswordChangeRequest
	// collection behind the local-cluster proxy: create-only, no CRD
	// registered for it in test/e2e/testdata/crds (a plain CRD would reject the
	// unnamed create the real command sends, or persist an object no
	// controller ever confirms), so the facade owns the route outright.
	PasswordChangeRequestsCollectionPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/passwordchangerequests"
)

// User is a named user the facade authenticates.
type User struct {
	Name     string
	Password string
	UserID   string
}

// Options configures a Facade instance. Every facade instance is meant to be
// per-script: its counters and token store are not safe to share
// across concurrent scripts.
type Options struct {
	// Fallthrough handles every request the facade does not own: the
	// existing bearer-checked router in front of envtest.
	Fallthrough http.Handler
	// Legacy makes the derived token create branch of the token POST 404 (ErrExtGroupAbsent),
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
	// azureADProvider document names for its endpoints, and the facade stops
	// serving /idp/* itself; the caller serves IdPHandler there. Empty keeps
	// the stub on the facade's own origin.
	IdPURL string
	// SeedToken, when set, is accepted as a valid bearer for the derived token
	// create endpoint alongside facade-issued tokens - the static seed token
	// every existing e2e script already authenticates with (the derived token
	// create requires a valid facade-issued or seed bearer).
	SeedToken string
	// ExtraProviders are provider types listed alongside localProvider, for
	// a flow the facade does not itself serve (the SAML authtokens flow
	// completes in the browser).
	ExtraProviders []string
	// Users are the named users the facade authenticates; defaults to one
	// user with LoginUser/LoginPass and UserID user-e2e.
	Users []User
	// AdminUserIDs are owners allowed to delete any token on the v3 path.
	AdminUserIDs []string
	// LegacyLoginBodyUserID adds userId to the v3 login body, as pre-ext
	// servers do.
	LegacyLoginBodyUserID bool
	// OmitSpecOnCreate withholds spec on ext token create responses.
	OmitSpecOnCreate bool
	// ScopedCreateStatus forces the status of a cluster-scoped ext create.
	ScopedCreateStatus int
	// ScopedCreateClusterName overrides the echoed spec.clusterName on a
	// scoped create; an empty string omits it.
	ScopedCreateClusterName *string
	// V3TokenCreate serves POST /v3/tokens, the Norman cluster-token
	// create.
	V3TokenCreate bool
	// V3CreateStatus forces a status on the Norman v3 token create.
	V3CreateStatus int
	// V3CreateClusterID overrides the echoed clusterId on a Norman v3
	// create; an empty string omits it.
	V3CreateClusterID *string
	// V3DeleteForbiddenForNonAdmin makes DELETE on the v3 path by a
	// non-admin owner answer 403.
	V3DeleteForbiddenForNonAdmin bool
	// RequestHook runs on every request the facade handles, before
	// dispatch.
	RequestHook func(r *http.Request)
	// ClusterVisibility says which management Clusters a bearer sees on the
	// steve clusters collection; a bearer it does not know is answered 401.
	// nil leaves the collection unserved (404).
	ClusterVisibility func(bearer string) ([]ClusterFixture, bool)
	// ClustersPageSize pages every steve collection the facade serves
	// (clusters, projects, a cluster's namespaces) with steve's continue
	// token (an offset); 0 serves one page.
	ClustersPageSize int
	// ProjectVisibility says which management Projects a bearer sees on a
	// steve projects collection: cluster is "" for the collection of every
	// cluster and the cluster id for ProjectsCollectionPath/<cluster>, where
	// the facade keeps only the projects on that cluster. ok=false answers
	// 401 (a bearer it does not know); an error answers 500. nil serves the
	// built-in projects (the local cluster's Default and System) to every
	// authorized bearer, and their GET by id under LocalProjectsPath.
	ProjectVisibility func(ctx context.Context, bearer, cluster string) (projects []ProjectFixture, ok bool, err error)
	// NamespaceVisibility says which namespaces a bearer sees on one
	// cluster's steve namespace collection (/k8s/clusters/<id>/v1/namespaces);
	// a bearer it does not know is answered 401. nil leaves the collection
	// unserved (404).
	NamespaceVisibility func(bearer, cluster string) ([]NamespaceFixture, bool)

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
	// any other status is answered after. A 2xx other than 201 carries the
	// normal token body (a create the client can read but must refuse);
	// any other status carries no body, modelling a create the client
	// never learned of.
	LoginStatus int
	// LoginShape forces the login response shape and the created record's
	// kind ("ext" or "v3") regardless of the requested responseType. Empty
	// follows the response type: json is v3, kubeconfig is ext.
	LoginShape string

	// DropDerivedCreateResponse creates the derived token and then closes the
	// connection without writing.
	DropDerivedCreateResponse bool
	// DropDerivedCreateResponseOnce does the same for the first create only
	// and answers every later one normally, so one process can fail a login
	// and then complete the next.
	DropDerivedCreateResponseOnce bool
	// MalformedDerivedCreateResponse answers a created derived token with a 201 whose body
	// establishes no token shape.
	MalformedDerivedCreateResponse bool
	// DerivedBearerOverride replaces the bearer in the derived token create response with this
	// string.
	DerivedBearerOverride string
	// DerivedCreateStatus forces a status on the derived token create POST, with LoginStatus's rule
	// about when the record is created.
	DerivedCreateStatus int
	// DerivedCreateHook runs when a derived token create POST arrives, before the record is created
	// or answered. A hook that cancels the client's context must then block
	// on the request context so the answer cannot race the cancellation.
	DerivedCreateHook func(r *http.Request)

	// SessionDeleteMode and DerivedDeleteMode fail the revoke of a located
	// record by its role, on either route: "" answers normally, "gone"
	// answers 404, "fail" answers 500.
	SessionDeleteMode string
	DerivedDeleteMode string

	// EnableSAMLAuthTokens serves the authtokens collection the browser sign-in
	// polls.
	// Nothing is served for a request id until DepositAuthToken.
	EnableSAMLAuthTokens bool
}

// record is one facade-issued token.
type record struct {
	name            string
	secret          string
	resourceVersion string
	clusterName     string
	description     string
	userID          string
	// kind is the resource the facade pretends to have created, "ext" or
	// "v3". It is fixed when the record is created, whatever shape the
	// client is later shown, because a server creates one definite kind.
	kind string
	// role is what the operation created the token for: "session" for the
	// login handler and the SAML authtokens flow, "derived" for the derived token create
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

// AuthTokensRequest is one request the authtokens path received.
type AuthTokensRequest struct {
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

// CreateRecord is one token create request the facade answered.
type CreateRecord struct {
	Kind        string
	Role        string
	Name        string
	Bearer      string
	ClusterName string
	Description string
	OwnerUserID string
}

// Facade is a fake Rancher auth surface. It implements http.Handler.
type Facade struct {
	opts   Options
	idp    *idpStub
	admins map[string]bool

	mu                sync.Mutex
	counter           int
	kubeconfigCounter int
	tokens            map[string]*record
	deleteModes       map[string]string
	descriptions      []string
	deletes           []DeleteRecord
	creates           []CreateRecord
	authTokensReqs    []AuthTokensRequest
	authTokens        map[string]string // request id → encrypted base64 bearer
	logins            int
	derivedCreates    int
	dropped           bool
}

// LoginDescriptions returns the descriptions the token-creating requests of
// a login carried - the login POST and the derived token create POST - in order.
func (f *Facade) LoginDescriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.descriptions)
}

// LoginCount is how many login POSTs the facade answered.
func (f *Facade) LoginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

// DerivedCreateRequests is how many derived token create POSTs the facade received, whatever it
// answered.
func (f *Facade) DerivedCreateRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.derivedCreates
}

// DeleteRequests returns every recorded DELETE attempt, in order.
func (f *Facade) DeleteRequests() []DeleteRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deletes)
}

// AuthTokensRequests returns every request the authtokens path received, in
// order.
func (f *Facade) AuthTokensRequests() []AuthTokensRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.authTokensReqs)
}

// TokenCount is how many tokens the facade holds.
func (f *Facade) TokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokens)
}

// CreateRequests returns every token create the facade answered, in order:
// login POSTs (role session), derived creates, cluster-scoped creates, and
// Norman v3 creates, with the bearer that made each one.
func (f *Facade) CreateRequests() []CreateRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.creates)
}

// recordCreate appends one create record under the lock.
func (f *Facade) recordCreate(c CreateRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, c)
}

// TokenOwner reports the owner of a live facade token by name.
func (f *Facade) TokenOwner(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.tokens[name]
	if !ok {
		return "", false
	}
	return rec.userID, true
}

// BearerOwner resolves a bearer to its token's owner; the seed token is
// owned by user-seed.
func (f *Facade) BearerOwner(bearer string) (string, bool) {
	if bearer == "" {
		return "", false
	}
	if f.opts.SeedToken != "" && bearer == f.opts.SeedToken {
		return "user-seed", true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rec := range f.tokens {
		if rec.bearer() == bearer {
			return rec.userID, true
		}
	}
	return "", false
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

// DepositAuthToken creates a v3 session token and deposits it for requestID,
// encrypted to publicKeyB64 the way the browser sign-in does.
func (f *Facade) DepositAuthToken(requestID, publicKeyB64 string) {
	rec := f.newToken("", 0)
	f.mu.Lock()
	rec.kind = "v3"
	rec.role = "session"
	f.mu.Unlock()
	f.DepositAuthTokenRaw(requestID, encryptToPublicKey(publicKeyB64, rec.bearer()))
}

// DepositAuthTokenPlaintext deposits plaintext for requestID, encrypted to
// publicKeyB64 in place of a real bearer, for a payload the client can
// decrypt but cannot read as a token.
func (f *Facade) DepositAuthTokenPlaintext(requestID, publicKeyB64, plaintext string) {
	f.DepositAuthTokenRaw(requestID, encryptToPublicKey(publicKeyB64, plaintext))
}

// DepositAuthTokenRaw deposits b64 for requestID as it is, for a payload the
// client cannot decrypt.
func (f *Facade) DepositAuthTokenRaw(requestID, b64 string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authTokens[requestID] = b64
}

// New builds a Facade from opts, applying defaults for the optional
// fields.
func New(opts Options) *Facade {
	if opts.LoginUser == "" {
		opts.LoginUser = "e2e-user"
	}
	if opts.LoginPass == "" {
		opts.LoginPass = "e2e-pass"
	}
	if len(opts.Users) == 0 {
		opts.Users = []User{{Name: opts.LoginUser, Password: opts.LoginPass, UserID: "user-e2e"}}
	}
	admins := make(map[string]bool)
	for _, id := range opts.AdminUserIDs {
		admins[id] = true
	}
	admins["user-seed"] = true
	f := &Facade{opts: opts, admins: admins, tokens: map[string]*record{}, authTokens: map[string]string{}}
	if opts.EnableIdP {
		f.idp = newIdPStub(opts.IdPPendingPolls)
	}
	return f
}

// ValidBearer reports whether token is a facade-issued, unrevoked, unexpired
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

// ServeHTTP dispatches auth paths to the facade's own handlers; everything else
// falls through to opts.Fallthrough (the facade is a layer in front of the
// existing router; it does not grow into a full fake).
func (f *Facade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.opts.RequestHook != nil {
		f.opts.RequestHook(r)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == normanTokensPath:
		f.handleNormanTokensPOST(w, r)
	case r.Method == http.MethodGet && r.URL.Path == authProvidersPath:
		f.handleAuthProviders(w, r)
	case r.Method == http.MethodPost && r.URL.Path == loginPath:
		f.handleLogin(w, r)
	case r.Method == http.MethodPost && r.URL.Path == extTokensPath:
		f.handleTokensPOST(w, r)
	case r.Method == http.MethodGet && r.URL.Path == extTokensPath:
		f.handleExtCollection(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, extTokensPath+"/"):
		f.handleNamedGet(w, r, strings.TrimPrefix(r.URL.Path, extTokensPath+"/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, extTokensPath+"/"):
		f.handleDelete(w, r, "ext", strings.TrimPrefix(r.URL.Path, extTokensPath+"/"))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, v3TokensPath+"/"):
		f.handleDelete(w, r, "v3", strings.TrimPrefix(r.URL.Path, v3TokensPath+"/"))
	case f.opts.EnableSAMLAuthTokens && strings.HasPrefix(r.URL.Path, authTokensPathPrefix):
		f.handleAuthTokens(w, r, strings.TrimPrefix(r.URL.Path, authTokensPathPrefix))
	case r.Method == http.MethodPost && r.URL.Path == KubeconfigsCollectionPath:
		f.handleKubeconfigsPOST(w, r)
	case r.Method == http.MethodPost && r.URL.Path == PasswordChangeRequestsCollectionPath:
		f.handlePasswordChangeRequestsPOST(w, r)
	case r.Method == http.MethodGet && r.URL.Path == CRTCollectionPath:
		f.handleCRTCollection(w, r)
	case f.opts.ClusterVisibility != nil && r.Method == http.MethodGet && r.URL.Path == ClustersCollectionPath:
		f.handleClustersCollection(w, r)
	case r.Method == http.MethodGet && isProjectsCollection(r.URL.Path):
		f.handleProjectsCollection(w, r, projectsCollectionCluster(r.URL.Path))
	case f.opts.ProjectVisibility == nil && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, LocalProjectsPath+"/"):
		f.handleLocalProjectGet(w, r, strings.TrimPrefix(r.URL.Path, LocalProjectsPath+"/"))
	case f.opts.NamespaceVisibility != nil && r.Method == http.MethodGet && steveNamespacesCluster(r.URL.Path) != "":
		f.handleNamespacesCollection(w, r, steveNamespacesCluster(r.URL.Path))
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
// route is 404 when the facade was built without EnableIdP.
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
	data := []map[string]any{{"id": "local", "type": "localProvider"}}
	if f.opts.EnableIdP {
		base := f.opts.IdPURL
		if base == "" {
			base = "https://" + r.Host
		}
		data = append(data, map[string]any{
			"id":            "azuread",
			"type":          "azureADProvider",
			"redirectUrl":   "",
			"clientId":      "facade-client",
			"scopes":        []string{"openid"},
			"authUrl":       base + "/idp/authorize",
			"deviceAuthUrl": base + "/idp/devicecode",
			"tokenUrl":      base + "/idp/token",
		})
	}
	for _, p := range f.opts.ExtraProviders {
		data = append(data, map[string]any{"id": strings.ToLower(strings.TrimSuffix(p, "Provider")), "type": p})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (f *Facade) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.logins++
	f.mu.Unlock()
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
	var userID string
	switch req.Type {
	case "localProvider":
		var found bool
		for _, u := range f.opts.Users {
			if u.Name == req.User && u.Password == req.Pass {
				userID = u.UserID
				found = true
				break
			}
		}
		if !found {
			f.recordCreate(CreateRecord{Kind: loginKind(f.opts.LoginShape, req.ResponseType), Role: "session"})
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	case "azureADProvider":
		if f.idp == nil || !f.idp.validIDToken(req.IDToken) {
			f.recordCreate(CreateRecord{Kind: loginKind(f.opts.LoginShape, req.ResponseType), Role: "session"})
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		userID = f.opts.Users[0].UserID
	default:
		f.recordCreate(CreateRecord{Kind: loginKind(f.opts.LoginShape, req.ResponseType), Role: "session"})
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// A deterministic 4xx proves the server created nothing, so it is
	// answered before any record exists.
	if deterministic4xx(f.opts.LoginStatus) {
		f.recordCreate(CreateRecord{Kind: loginKind(f.opts.LoginShape, req.ResponseType), Role: "session"})
		w.WriteHeader(f.opts.LoginStatus)
		return
	}

	rec := f.newToken("", 0)
	f.mu.Lock()
	rec.kind = loginKind(f.opts.LoginShape, req.ResponseType)
	rec.role = "session"
	rec.description = req.Description
	rec.userID = userID
	if req.Description != "" {
		f.descriptions = append(f.descriptions, req.Description)
	}
	f.creates = append(f.creates, CreateRecord{Kind: rec.kind, Role: "session", Name: rec.name, Bearer: "", Description: req.Description, OwnerUserID: rec.userID})
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

	// A forced 2xx other than 201 answers with the normal body under that
	// status: the token exists and the client can read it, but the status
	// is not the one a create promises. Any other forced status answers
	// with no body: the record exists and the client learns nothing.
	status := http.StatusCreated
	if f.opts.LoginStatus != 0 {
		status = f.opts.LoginStatus
	}
	switch {
	case f.opts.LoginStatus != 0 && (status < 200 || status > 299):
		w.WriteHeader(status)
	case f.opts.DropLoginResponse:
		dropConnection(w)
	case f.opts.MalformedLoginResponse:
		writeJSON(w, status, map[string]any{"broken": true})
	case rec.kind == "v3":
		writeV3Token(w, status, rec, name, bearer, f.opts.LegacyLoginBodyUserID)
	default:
		writeExtTokenAs(w, status, rec, name, bearer, !f.opts.OmitSpecOnCreate)
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

// namedBodyFallthrough reads r's body and, when it carries its own
// metadata.name (a full CR - `create resource -f`), restores the body and
// falls through to the real envtest store unchanged, answering w itself and
// reporting handled=true; an unnamed body (an imperative create-only
// resource's own shape) is left unhandled, body returned for the caller to
// decode. Shared by the handlers whose resource also has a CRD behind the
// fallthrough (tokens, kubeconfigs) - passwordchangerequests has none, so
// its handler reads its own body directly instead of calling this.
func (f *Facade) namedBodyFallthrough(w http.ResponseWriter, r *http.Request) (body []byte, handled bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return nil, true
	}
	var probe struct {
		Metadata *struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &probe)
	if probe.Metadata != nil && probe.Metadata.Name != "" {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.fallthroughTo(w, r)
		return nil, true
	}
	return body, false
}

func (f *Facade) handleTokensPOST(w http.ResponseWriter, r *http.Request) {
	body, handled := f.namedBodyFallthrough(w, r)
	if handled {
		return
	}

	f.mu.Lock()
	f.derivedCreates++
	f.mu.Unlock()

	if f.opts.Legacy {
		http.NotFound(w, r)
		return
	}
	// The bearer is authenticated, and its owner read, before the hook
	// runs, as Rancher's authentication filter precedes the handler: a
	// credential revoked while the hook holds the request (a caller that
	// gave up and is cleaning up) does not turn this create into a 401.
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	owner, _ := f.BearerOwner(bearerFromHeader(r))
	if f.opts.DerivedCreateHook != nil {
		f.opts.DerivedCreateHook(r)
	}

	var createReq struct {
		Spec struct {
			TTL         int64  `json:"ttl"`
			ClusterName string `json:"clusterName"`
			Description string `json:"description"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(body, &createReq)

	role := "derived"
	if createReq.Spec.ClusterName != "" {
		role = "cluster"
	}

	forcedStatus := f.opts.DerivedCreateStatus
	if role == "cluster" && f.opts.ScopedCreateStatus != 0 {
		forcedStatus = f.opts.ScopedCreateStatus
	}

	if deterministic4xx(forcedStatus) {
		f.recordCreate(CreateRecord{Kind: "ext", Role: role, Bearer: bearerFromHeader(r), ClusterName: createReq.Spec.ClusterName, Description: createReq.Spec.Description, OwnerUserID: owner})
		w.WriteHeader(forcedStatus)
		return
	}

	rec := f.newToken(createReq.Spec.ClusterName, createReq.Spec.TTL)
	f.mu.Lock()
	rec.kind = "ext"
	rec.role = role
	rec.description = createReq.Spec.Description
	rec.userID = owner
	if createReq.Spec.Description != "" {
		f.descriptions = append(f.descriptions, createReq.Spec.Description)
	}
	f.creates = append(f.creates, CreateRecord{Kind: "ext", Role: role, Name: rec.name, Bearer: bearerFromHeader(r), ClusterName: createReq.Spec.ClusterName, Description: createReq.Spec.Description, OwnerUserID: owner})
	f.mu.Unlock()

	echoedClusterName := createReq.Spec.ClusterName
	if f.opts.ScopedCreateClusterName != nil {
		echoedClusterName = *f.opts.ScopedCreateClusterName
	}

	switch {
	case forcedStatus != 0:
		w.WriteHeader(forcedStatus)
	case f.opts.DropDerivedCreateResponse, f.opts.DropDerivedCreateResponseOnce && f.dropFirstCreate():
		dropConnection(w)
	case f.opts.MalformedDerivedCreateResponse:
		writeJSON(w, http.StatusCreated, map[string]any{"broken": true})
	default:
		bearer := rec.bearer()
		if f.opts.DerivedBearerOverride != "" {
			bearer = f.opts.DerivedBearerOverride
		}
		writeExtTokenAsWithScope(w, http.StatusCreated, rec, rec.name, bearer, !f.opts.OmitSpecOnCreate, echoedClusterName)
	}
}

// dropFirstCreate reports whether this create is the first one, marking it
// dropped so every later create is answered normally.
func (f *Facade) dropFirstCreate() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dropped {
		return false
	}
	f.dropped = true
	return true
}

// handleNormanTokensPOST serves the Norman v3 token create: POST /v3/tokens
// with clusterId answers the Norman shape with clusterId and userId echoed.
func (f *Facade) handleNormanTokensPOST(w http.ResponseWriter, r *http.Request) {
	if !f.opts.V3TokenCreate {
		http.NotFound(w, r)
		return
	}
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	var req struct {
		Type        string `json:"type"`
		Description string `json:"description"`
		ClusterID   string `json:"clusterId"`
		TTL         int64  `json:"ttl"`
	}
	_ = json.Unmarshal(body, &req)

	owner, _ := f.BearerOwner(bearerFromHeader(r))
	role := "derived"
	if req.ClusterID != "" {
		role = "cluster"
	}

	if deterministic4xx(f.opts.V3CreateStatus) {
		f.recordCreate(CreateRecord{Kind: "v3", Role: role, Bearer: bearerFromHeader(r), ClusterName: req.ClusterID, Description: req.Description, OwnerUserID: owner})
		w.WriteHeader(f.opts.V3CreateStatus)
		return
	}

	rec := f.newToken(req.ClusterID, req.TTL)
	f.mu.Lock()
	rec.kind = "v3"
	rec.role = role
	rec.description = req.Description
	rec.userID = owner
	f.creates = append(f.creates, CreateRecord{Kind: "v3", Role: role, Name: rec.name, Bearer: bearerFromHeader(r), ClusterName: req.ClusterID, Description: req.Description, OwnerUserID: owner})
	f.mu.Unlock()

	echoedClusterID := req.ClusterID
	if f.opts.V3CreateClusterID != nil {
		echoedClusterID = *f.opts.V3CreateClusterID
	}

	status := http.StatusCreated
	if f.opts.V3CreateStatus != 0 {
		status = f.opts.V3CreateStatus
	}

	if status < 200 || status > 299 {
		w.WriteHeader(status)
		return
	}

	body2 := map[string]any{
		"id":        rec.name,
		"token":     rec.bearer(),
		"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339),
		"userId":    rec.userID,
		"type":      "token",
		"baseType":  "token",
	}
	if echoedClusterID != "" {
		body2["clusterId"] = echoedClusterID
	}
	writeJSON(w, status, body2)
}

// handleExtCollection lists every ext record as a TokenList. With a
// fallthrough, the records the real apiserver holds are listed first and
// the facade's own appended, so a listing shows both.
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

// handleAuthTokens serves the authtokens collection: 404 until a token is
// deposited for the request id, then the encrypted bearer, and a DELETE
// removes it. Every request is recorded before it is answered.
func (f *Facade) handleAuthTokens(w http.ResponseWriter, r *http.Request, id string) {
	f.mu.Lock()
	f.authTokensReqs = append(f.authTokensReqs, AuthTokensRequest{Method: r.Method, Path: r.URL.Path})
	payload, ok := f.authTokens[id]
	if r.Method == http.MethodDelete {
		delete(f.authTokens, id)
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
			"expiresAt": time.Now().Add(defaultDerivedTTL).UTC().Format(time.RFC3339),
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

	owner, _ := f.BearerOwner(bearerFromHeader(r))
	if kind == "v3" && f.opts.V3DeleteForbiddenForNonAdmin && !f.admins[owner] {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if rec.userID != "" && owner != rec.userID && !f.admins[owner] {
		http.NotFound(w, r)
		return
	}

	switch f.deleteMode(rec) {
	case "gone":
		http.NotFound(w, r)
		return
	case "fail":
		w.WriteHeader(http.StatusInternalServerError)
		return
	case "hang":
		<-r.Context().Done()
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

// SetDeleteMode makes DELETE of one token answer a fault mode: "gone"
// (404), "fail" (500), "hang" (until the request ends). The name is the
// one the create answered, so a caller faults the very token it holds
// instead of guessing what the facade numbered it.
func (f *Facade) SetDeleteMode(name, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteModes == nil {
		f.deleteModes = map[string]string{}
	}
	f.deleteModes[name] = mode
}

// deleteMode is the fault mode configured for the located record by name,
// else for its role, so an ext-shaped session token fails where the
// derived token succeeds.
func (f *Facade) deleteMode(rec *record) string {
	f.mu.Lock()
	mode, ok := f.deleteModes[rec.name]
	f.mu.Unlock()
	if ok {
		return mode
	}
	switch rec.role {
	case "session":
		return f.opts.SessionDeleteMode
	case "derived":
		return f.opts.DerivedDeleteMode
	}
	return ""
}

// handleKubeconfigsPOST serves `create kubeconfig`'s create-only ext
// Kubeconfig store: a request carrying its own metadata.name (`create
// resource -f`) falls through to envtest's real CRD, unchanged; an unnamed
// request (the command's own store-generated-identity shape, mirroring
// handleTokensPOST) is answered directly, ?dryRun=All echoing the object
// with no status, a real POST additionally filling status.value AND
// forwarding a named copy to envtest (forwardNamedCreate) so a later
// get/delete kubeconfig <name> succeeds against the real store.
//
// ASSUMPTION (needs a fidelity probe against a real server): the real
// server's status.value is a full kubeconfig document embedding a live,
// revocable bearer scoped to spec.clusters, with entries Rancher's own
// formatter builds (server URL, CA, impersonation-ready user). This facade
// fills status.value with a fixture kubeconfig naming spec.clusters (or
// "local" when empty) and an inert placeholder server/token - enough for
// the command's own success path (it only checks status.value is
// non-empty), never a claim about the real document's shape; the persisted
// object's own spec is real, forwarded verbatim.
func (f *Facade) handleKubeconfigsPOST(w http.ResponseWriter, r *http.Request) {
	body, handled := f.namedBodyFallthrough(w, r)
	if handled {
		return
	}

	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	spec, ok := decodeSpec(w, body)
	if !ok {
		return
	}

	f.mu.Lock()
	f.kubeconfigCounter++
	n := f.kubeconfigCounter
	f.mu.Unlock()
	name := fmt.Sprintf("fac-kubeconfig-%d", n)

	obj := map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Kubeconfig",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}
	if !slices.Contains(r.URL.Query()["dryRun"], "All") {
		f.forwardNamedCreate(r, "Kubeconfig", "ext.cattle.io/v1", name, spec)
		clusters := stringsFromAny(spec["clusters"])
		currentContext, _ := spec["currentContext"].(string)
		value, err := kubeconfigValueFixture(clusters, currentContext)
		if err != nil {
			http.Error(w, "building kubeconfig fixture: "+err.Error(), http.StatusInternalServerError)
			return
		}
		obj["status"] = map[string]any{"value": value, "summary": "Complete"}
	}
	writeJSON(w, http.StatusCreated, obj)
}

// forwardNamedCreate persists a named copy of an unnamed facade-owned create
// into envtest's real CRD, through the fallthrough router (bypassing this
// facade's own route dispatch, exactly like a named `create resource -f`
// request would) - so a later get/delete by the generated name succeeds
// against the real store. Errors are silently ignored: the caller's own
// synthetic response has already answered the client, and a persistence gap
// here surfaces as its own explicit failure, a later get/delete 404.
func (f *Facade) forwardNamedCreate(r *http.Request, kind, apiVersion, name string, spec map[string]any) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, r.URL.String(), bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	f.fallthroughTo(httptest.NewRecorder(), req)
}

// stringsFromAny converts a decoded JSON array (element type always any) to
// a string slice, skipping non-string entries; nil for anything else,
// including an absent field.
func stringsFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// kubeconfigValueFixture builds a deterministic, valid kubeconfig document
// naming each requested cluster (or "local" for an empty list) via the
// repo's own clientcmdapi.Config + clientcmd.Write path - the same one
// internal/engine/kubeconfig/command.go uses - rather than hand-built YAML, so a
// cluster name carrying a YAML-significant character still encodes
// correctly. One shared "rancher" AuthInfo carries every context, mirroring
// the real store's single reused credential. See handleKubeconfigsPOST's
// ASSUMPTION comment.
func kubeconfigValueFixture(clusters []string, currentContext string) (string, error) {
	if len(clusters) == 0 {
		clusters = []string{"local"}
	}
	if currentContext == "" {
		currentContext = clusters[0]
	}
	const authInfoName = "rancher"
	cfg := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{authInfoName: {Token: "fac-kubeconfig-token"}},
		Contexts:       map[string]*clientcmdapi.Context{},
		CurrentContext: currentContext,
	}
	for _, c := range clusters {
		cfg.Clusters[c] = &clientcmdapi.Cluster{Server: "https://facade.example.invalid/k8s/clusters/" + c}
		cfg.Contexts[c] = &clientcmdapi.Context{Cluster: c, AuthInfo: authInfoName}
	}
	b, err := clientcmd.Write(cfg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// handlePasswordChangeRequestsPOST serves `create password-change-request`'s
// create-only ext PasswordChangeRequest resource: no CRD backs this route
// (see PasswordChangeRequestsCollectionPath), so the facade owns both the
// dry-run preview (echoed with no status, proving the workflow is present)
// and the real POST, which the real server completes synchronously.
//
// ASSUMPTION (needs a fidelity probe against a real server): the real
// server's response shape beyond the PasswordUpdated condition this facade
// fills - additional conditions, a resource name, or timestamps the command
// does not read today.
func (f *Facade) handlePasswordChangeRequestsPOST(w http.ResponseWriter, r *http.Request) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading body", http.StatusBadRequest)
		return
	}
	spec, ok := decodeSpec(w, body)
	if !ok {
		return
	}

	obj := map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "PasswordChangeRequest",
		"metadata":   map[string]any{},
		"spec":       spec,
	}
	if !slices.Contains(r.URL.Query()["dryRun"], "All") {
		obj["status"] = map[string]any{
			"summary": "Complete",
			"conditions": []map[string]any{
				{"type": "PasswordUpdated", "status": "True"},
			},
		}
	}
	writeJSON(w, http.StatusCreated, obj)
}

// decodeSpec reads body's "spec" field into a generic map - echoed verbatim
// by every create-only ext resource handler so no per-field struct can drop
// a flag the real command sends - answering w with a malformed-body error
// and reporting ok=false on decode failure. Shared by
// handleKubeconfigsPOST and handlePasswordChangeRequestsPOST.
func decodeSpec(w http.ResponseWriter, body []byte) (spec map[string]any, ok bool) {
	var req struct {
		Spec map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeMalformedBody(w, err)
		return nil, false
	}
	return req.Spec, true
}

// writeMalformedBody answers a request whose body could not be decoded with
// a 400 carrying a Kubernetes-shaped Status object, the same error shape a
// real apiserver's decode failure returns - never a silent 201 over a
// zero-value fixture.
func writeMalformedBody(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    "malformed request body: " + err.Error(),
		"reason":     "BadRequest",
		"code":       http.StatusBadRequest,
	})
}

// crtFixture is one facade-served clusterregistrationtoken. Token is the
// deterministic real value spliced into the seven command/manifest fields,
// mirroring rancher's own steve formatter (clusterregistrationtoken.go:
// 24-72, the ReplaceAll at :69). ClusterName mirrors the real CRD's
// spec.clusterName, which always equals the CRT's own management namespace.
type crtFixture struct {
	Namespace string
	Name      string
	Token     string
}

// crtFixtures spans multiple namespaces (the namespace-filter test fixture
// set): "c-fixture" is the namespace
// test/e2e/scripts/registration-token.txtar addresses; "c-other" exists purely
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
// fixture in crtFixtures - the caller (NewGetCRT) filters by namespace
// locally, so the facade always returns the full set regardless of query
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

// ClusterFixture is one management Cluster as the steve collection lists
// it: the id and the display name the CLI shows.
type ClusterFixture struct {
	ID          string
	DisplayName string
}

// handleClustersCollection serves the steve clusters collection filtered
// by the caller's bearer through Options.ClusterVisibility, with steve's
// envelope: data, count (the collection total), revision, and continue (an
// offset) when ClustersPageSize cuts a page.
func (f *Facade) handleClustersCollection(w http.ResponseWriter, r *http.Request) {
	visible, ok := f.opts.ClusterVisibility(bearerFromHeader(r))
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Sort a copy: the callback's slice may alias storage the harness owns.
	visible = slices.Clone(visible)
	sort.Slice(visible, func(i, j int) bool { return visible[i].ID < visible[j].ID })
	items := make([]map[string]any, 0, len(visible))
	for _, c := range visible {
		items = append(items, map[string]any{
			"id":       c.ID,
			"type":     "management.cattle.io.cluster",
			"metadata": map[string]any{"name": c.ID},
			"spec":     map[string]any{"displayName": c.DisplayName},
		})
	}
	f.serveSteveCollection(w, r, "management.cattle.io.cluster", items)
}

// ProjectFixture is one management Project the facade's steve collection
// lists, as <Cluster>/<ID> with its display name.
type ProjectFixture struct {
	Cluster     string
	ID          string
	DisplayName string
}

// NamespaceFixture is one namespace of a cluster's steve collection; Project
// is the bare project id its field.cattle.io/projectId label carries, empty
// for a namespace outside every project.
type NamespaceFixture struct {
	Name    string
	Project string
}

// builtinProjects are the projects Rancher creates on the local cluster,
// served when Options.ProjectVisibility is nil. Rancher generates the ids;
// these are fixed.
var builtinProjects = []ProjectFixture{
	{Cluster: "local", ID: "p-default", DisplayName: "Default"},
	{Cluster: "local", ID: "p-system", DisplayName: "System"},
}

// isProjectsCollection reports whether path is the projects collection of
// every cluster or of one cluster (ProjectsCollectionPath/<cluster>).
func isProjectsCollection(path string) bool {
	return path == ProjectsCollectionPath || projectsCollectionCluster(path) != ""
}

// projectsCollectionCluster returns the cluster id of a one-cluster
// projects collection path, or "" for any other path.
func projectsCollectionCluster(path string) string {
	cluster, ok := strings.CutPrefix(path, ProjectsCollectionPath+"/")
	if !ok || strings.Contains(cluster, "/") {
		return ""
	}
	return cluster
}

// visibleProjects is the projects the caller sees on the collection of
// cluster ("" for every cluster), through Options.ProjectVisibility or the
// built-in projects.
func (f *Facade) visibleProjects(r *http.Request, cluster string) ([]ProjectFixture, bool, error) {
	if f.opts.ProjectVisibility != nil {
		return f.opts.ProjectVisibility(r.Context(), bearerFromHeader(r), cluster)
	}
	if !f.authorizedBearer(r) {
		return nil, false, nil
	}
	return builtinProjects, true, nil
}

// handleProjectsCollection serves the projects collection of every cluster
// (cluster "") or of one cluster, with steve's item shape: id
// <cluster>/<project>, metadata.namespace the cluster.
func (f *Facade) handleProjectsCollection(w http.ResponseWriter, r *http.Request, cluster string) {
	visible, ok, err := f.visibleProjects(r, cluster)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	visible = slices.DeleteFunc(slices.Clone(visible), func(p ProjectFixture) bool {
		return cluster != "" && p.Cluster != cluster
	})
	sort.Slice(visible, func(i, j int) bool {
		if visible[i].Cluster != visible[j].Cluster {
			return visible[i].Cluster < visible[j].Cluster
		}
		return visible[i].ID < visible[j].ID
	})
	items := make([]map[string]any, 0, len(visible))
	for _, p := range visible {
		items = append(items, map[string]any{
			"id":         p.Cluster + "/" + p.ID,
			"type":       "management.cattle.io.project",
			"apiVersion": "management.cattle.io/v3",
			"kind":       "Project",
			"metadata":   map[string]any{"name": p.ID, "namespace": p.Cluster},
			"spec":       map[string]any{"clusterName": p.Cluster, "displayName": p.DisplayName},
		})
	}
	f.serveSteveCollection(w, r, "management.cattle.io.project", items)
}

// handleLocalProjectGet answers the Kubernetes GET of one built-in project
// by id: the Project object, or the apiserver's 404 Status for an id it
// does not hold.
func (f *Facade) handleLocalProjectGet(w http.ResponseWriter, r *http.Request, id string) {
	if !f.authorizedBearer(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	for _, p := range builtinProjects {
		if p.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{
				"apiVersion": "management.cattle.io/v3",
				"kind":       "Project",
				"metadata":   map[string]any{"name": p.ID, "namespace": p.Cluster},
				"spec":       map[string]any{"clusterName": p.Cluster, "displayName": p.DisplayName},
			})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    fmt.Sprintf("projects.management.cattle.io %q not found", id),
		"reason":     "NotFound",
		"details":    map[string]any{"name": id, "group": "management.cattle.io", "kind": "projects"},
		"code":       http.StatusNotFound,
	})
}

// steveNamespacesCluster returns the cluster id of a steve namespace
// collection path (/k8s/clusters/<id>/v1/namespaces), or "" for any other
// path.
func steveNamespacesCluster(path string) string {
	rest, ok := strings.CutPrefix(path, "/k8s/clusters/")
	if !ok {
		return ""
	}
	cluster, tail, ok := strings.Cut(rest, "/")
	if !ok || cluster == "" || tail != "v1/namespaces" {
		return ""
	}
	return cluster
}

func (f *Facade) handleNamespacesCollection(w http.ResponseWriter, r *http.Request, cluster string) {
	visible, ok := f.opts.NamespaceVisibility(bearerFromHeader(r), cluster)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	visible = slices.Clone(visible)
	sort.Slice(visible, func(i, j int) bool { return visible[i].Name < visible[j].Name })
	items := make([]map[string]any, 0, len(visible))
	for _, ns := range visible {
		labels := map[string]any{"kubernetes.io/metadata.name": ns.Name}
		if ns.Project != "" {
			labels["field.cattle.io/projectId"] = ns.Project
		}
		items = append(items, map[string]any{
			"id":       ns.Name,
			"type":     "namespace",
			"metadata": map[string]any{"name": ns.Name, "labels": labels},
		})
	}
	f.serveSteveCollection(w, r, "namespace", items)
}

// serveSteveCollection writes one page of a steve collection: count is the
// whole collection and left out when it is zero, as steve answers an empty
// collection; continue an offset token when ClustersPageSize cuts the page;
// revision fixed.
func (f *Facade) serveSteveCollection(w http.ResponseWriter, r *http.Request, resourceType string, items []map[string]any) {
	offset := 0
	if c := r.URL.Query().Get("continue"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 || n > len(items) {
			http.Error(w, "invalid continue token", http.StatusBadRequest)
			return
		}
		offset = n
	}
	end := len(items)
	if f.opts.ClustersPageSize > 0 && offset+f.opts.ClustersPageSize < end {
		end = offset + f.opts.ClustersPageSize
	}
	body := map[string]any{
		"type":         "collection",
		"resourceType": resourceType,
		"revision":     "1",
		"data":         items[offset:end],
	}
	if len(items) > 0 {
		body["count"] = len(items)
	}
	if end < len(items) {
		body["continue"] = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, body)
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
// each channel-server route serves: raw
// "vMAJOR.MINOR.PATCH+<distro>rN"-shaped strings, exactly the form
// internal/engine/provisioning's normalizeVersion strips down to
// MAJOR.MINOR.PATCH before any supported-range comparison.
var (
	rke2ReleaseFixture = []string{"v1.30.5+rke2r1", "v1.30.6+rke2r1"}
	k3sReleaseFixture  = []string{"v1.30.5+k3s1", "v1.30.6+k3s1"}
)

// handleRelease serves a channel-server release collection envelope
// ({"data":[{"version": "..."}, ...]}) - the "data" key is kept identical to
// internal/engine/provisioning's fetchReleaseVersions decode (channelserver
// model/config.go:22-30's own collection key; a test against a
// real server would correct this fixture if the real shape differs). Unlike the
// facade's other routes, this one is deliberately UNAUTHENTICATED: rancher's
// routes.go:139-140 registers it as a bare mux route server-side, so no
// authorizedBearer check gates it here - RootClient still sends credentials
// to it (the one-policy rule), but the fixture must accept the
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

// newToken issues the next deterministic name/secret pair from the
// instance-local counter, shared by /v1-public/login (the session token) and
// the derived token create endpoint (the derived token) - the same mechanism that makes a
// provider login consume two counter slots.
func (f *Facade) newToken(clusterName string, ttlMs int64) *record {
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
		rec.expiresAt = time.Now().Add(defaultDerivedTTL)
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
func writeExtTokenAs(w http.ResponseWriter, status int, rec *record, name, bearer string, withSpec bool) {
	writeExtTokenAsWithScope(w, status, rec, name, bearer, withSpec, rec.clusterName)
}

// writeExtTokenAsWithScope writes the ext Token object with the given
// identity and an optionally overridden echoed clusterName.
func writeExtTokenAsWithScope(w http.ResponseWriter, status int, rec *record, name, bearer string, withSpec bool, echoedClusterName string) {
	statusBlock := map[string]any{"expiresAt": rec.expiresAt.UTC().Format(time.RFC3339)}
	if bearer != "" {
		statusBlock["bearerToken"] = bearer
	}
	metadata := map[string]any{"resourceVersion": rec.resourceVersion}
	if name != "" {
		metadata["name"] = name
	}
	body := map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "Token",
		"metadata":   metadata,
		"status":     statusBlock,
	}
	if withSpec {
		spec := map[string]any{}
		if echoedClusterName != "" {
			spec["clusterName"] = echoedClusterName
		}
		if rec.description != "" {
			spec["description"] = rec.description
		}
		if rec.userID != "" {
			spec["userID"] = rec.userID
		}
		body["spec"] = spec
	}
	writeJSON(w, status, body)
}

// writeV3Token writes the Norman token shape a responseType json login
// returns: the bearer under "token" and the resource name under "id".
// Either may be empty, which withholds that field.
func writeV3Token(w http.ResponseWriter, status int, rec *record, name, bearer string, withUserID bool) {
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
	if withUserID && rec.userID != "" {
		body["userId"] = rec.userID
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// This JSON is a test fixture read by the CLI, not embedded in HTML - keep
	// `<`, `>`, `&` literal, matching internal/output/render.go's convention.
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
