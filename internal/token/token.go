package token

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/tidwall/gjson"
)

const (
	loginURL        = "%s/v1-public/login"
	authProviderURL = "%s/v1-public/authproviders"
	authTokenURL    = "%s/v1-public/authtokens/%s"
)

const (
	deviceAuthFlow = "devicecode"
	authCodeFlow   = "authcode"
)

var samlProviders = map[string]bool{
	"pingProvider":       true,
	"adfsProvider":       true,
	"keyCloakProvider":   true,
	"oktaProvider":       true,
	"shibbolethProvider": true,
}

var oauthProviders = map[string]bool{
	"azureADProvider": true,
}

var supportedAuthProviders = map[string]bool{
	"localProvider":           true,
	"freeIpaProvider":         true,
	"openLdapProvider":        true,
	"activeDirectoryProvider": true,
	"pingProvider":            true,
	"adfsProvider":            true,
	"keyCloakProvider":        true,
	"oktaProvider":            true,
	"shibbolethProvider":      true,
	"azureADProvider":         true,
}

type loginInput struct {
	server          string
	clusterID       string
	authProvider    string
	skipVerify      bool
	authFlow        string
	usernameDefault string
	// responseType is the shape the login request asks Rancher for. Empty
	// keeps get-token's kubeconfig form; `auth login` asks for json.
	responseType string
	// description is the description every token this login creates
	// carries. Empty creates an unmarked token.
	description string
	// idpClient dials the identity provider's OAuth endpoints. It trusts the
	// operating system store, never the Rancher entry's CA: the entry's
	// trust must not extend to a third-party origin, and a self-signed
	// Rancher must not break a publicly trusted provider.
	idpClient *http.Client
}

// runParams carries the resolved flag values for a get-token invocation.
// tokenCmd.run builds it from the Input's flag reads; the legacy token shim
// builds it directly, bypassing the flag layer.
//
// ProbedKey records the cache key already probed by the caller's fast path
// (run's flag-only probe or the shim's pre-probe). runWithParams skips its
// own cache check when its resolved key equals ProbedKey, preventing a double
// "cache read failed" notice on a real store error.
// ProbedNoticed is true when the fast-path probe fired a "cache read failed"
// notice — a real (non-ErrNoToken) store error. When true, the resolved-key
// re-check is quiet so the notice fires exactly once (not zero times: a clean
// miss on the flag key does NOT suppress a distinct error on the resolved key).
//
// legacyShim marks the caller as the legacy token shim — the one structural
// fact behind every shim-only behavior, folded into a single field so a
// caller cannot compile with some of the behaviors and not the others. Only
// shim.go (same package) sets it. It drives:
//   - no current-context lookups (forwarded as the ignoreCurrentContext parameter of
//     resolveConn/caForServer/storedTokenForServer), the gate for the shim's
//     "driven purely by the flags Rancher's kubeconfig template supplies"
//     principle: an absent --server is rejected rather than filled from the
//     current server, an empty --cluster stays unscoped, CA selection among
//     same-host Servers entries follows sorted order instead of preferring
//     the current-server entry, and stored-login selection ignores both
//     wantName (the shim's Name is a cache-slot label, not an entry-name
//     preference) and the current-server tier.
//   - zero-expiry caching: a created token that carries no expiry is cached
//     (never-expiring on the legacy path); the native path refuses to.
//   - legacy cache slots: a config.LegacyCachePrefix Name passes
//     runWithParams's reservation guard; every other caller is blocked.
//
// SkipVerify disables TLS verification. Only the legacy token shim sets it —
// Rancher-generated kubeconfigs pass --skip-verify verbatim. The native
// surface has no skip-verify flag anywhere; pin the CA instead.
type runParams struct {
	Server, Name, Cluster, AuthProvider, AuthFlow, CACert string
	SkipVerify                                            bool
	UsernameDefault                                       string
	ProbedKey                                             string
	ProbedNoticed                                         bool
	legacyShim                                            bool
}

type typedProvider interface {
	GetType() string
}

type tokenCmd struct {
	dir   string
	store config.CredStore
}

func NewTokenCommand(dir string, store config.CredStore) runtime.Command {
	return &tokenCmd{dir: dir, store: store}
}

func (tokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth get-token",
		Usage:       "Authenticate and print a kubeconfig ExecCredential",
		CommandType: runtime.Local,
		Flags: []runtime.ArgSpec{
			{Name: "server", Type: "string", Desc: "Rancher server `url`", Env: []string{"RANCHER_CLI_URL"}},
			{Name: "cluster", Type: "string", Required: true, Desc: "cluster `id|name` to create the exec-credential token for"},
			{Name: "name", Type: "string", Desc: "stored server `name` to use"},
			{Name: "auth-provider", Type: "string", Desc: "auth provider `name` to sign in with"},
			{Name: "auth-flow", Type: "string", Enum: []string{"devicecode", "authcode"}, Desc: "identity-provider `flow`", Env: []string{config.EnvOAuthAuthFlow, config.EnvLegacyOAuthAuthFlow}},
			{Name: "cacert", Type: "string", Desc: "PEM CA bundle, a `path` or the literal PEM", Env: []string{"RANCHER_CLI_CACERT"}},
		},
		Network: true,
		Auth:    true,
		Examples: []string{
			"rancher auth get-token --server https://rancher.example.com --cluster c-1 --name prod",
		},
		// The exec-credential plugin's stdout is a bare ExecCredential object
		// kubectl parses directly — no envelope exists to vary with -o. table
		// (the default) and json are the two forms this command has ever
		// honored; every other value is now rejected pre-Plan instead of
		// being silently ignored.
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table", "json"}},
		Shape:        runtime.ShapeExecJSON,
	}
}

func (t tokenCmd) Execute(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	return t.run(ctx, in, p)
}

func (t tokenCmd) run(ctx context.Context, in runtime.Input, p runtime.Prompter) (runtime.Result, error) {
	name := in.String("name")
	// Reject the reserved prefix before any cache read so a warm legacy:-slot
	// entry is never served by the native command. The runWithParams guard
	// below covers callers that bypass run() (e.g. future direct callers).
	if config.IsLegacyCacheSlot(name) {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--name values starting with '"+config.LegacyCachePrefix+"' are reserved for the legacy token shim's cache slots; use a different name")
	}

	// Require --cluster BEFORE the cache probe, not only in resolveConn: a
	// still-valid cache entry keyed with an empty cluster component (written
	// before the flag became required) would otherwise be served without ever
	// reaching resolveConn's check — a token returned for a cluster the
	// caller didn't name. The pipeline's validateInput normally rejects this
	// first; this guards direct Execute callers. The shim keeps its unscoped
	// path via runWithParams, which run() never fronts for it.
	if in.String("cluster") == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--cluster is required")
	}

	flagServer := in.String("server")
	flagKey, noticed, r, hit := t.probeFlagCache(name, flagServer, in.String("cluster"), p)
	if hit {
		return r, nil
	}

	return t.runWithParams(ctx, runParams{
		Server:        flagServer,
		Name:          name,
		Cluster:       in.String("cluster"),
		AuthProvider:  in.String("auth-provider"),
		AuthFlow:      in.String("auth-flow"),
		CACert:        in.String("cacert"),
		ProbedKey:     flagKey,
		ProbedNoticed: noticed,
	}, p)
}

// runWithParams runs the full get-token flow (config resolution, cache,
// auth) from an already-populated runParams. tokenCmd.run calls it after the
// flag fast path; the legacy token shim calls it directly.
func (t tokenCmd) runWithParams(ctx context.Context, params runParams, p runtime.Prompter) (runtime.Result, error) {
	// Reject any --name starting with the legacy prefix unless the caller is
	// the shim (legacyShim, an unexported field only shim.go sets). This
	// guard lives here — not just in run() — so every runWithParams caller is
	// protected, not only the CLI-flag path.
	if config.IsLegacyCacheSlot(params.Name) && !params.legacyShim {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--name values starting with '"+config.LegacyCachePrefix+"' are reserved for the legacy token shim's cache slots; use a different name")
	}

	// Full resolution (config + CA disk I/O): required to authenticate on a miss
	// and to build the definitive cache key when --server was not provided.
	server, caPEM, clusterID, err := t.resolveConn(params.Server, params.Cluster, params.CACert, params.legacyShim)
	if err != nil {
		return runtime.Result{}, err
	}
	// Validate and canonicalize the resolved server once: both the definitive
	// cache key and the subsequent cache write use this single validated identity.
	serverID, cerr := credhelper.CanonicalServerURL(server)
	if cerr != nil {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("resolved server %q is not a valid URL: %v", server, cerr))
	}
	if !config.IsSupportedScheme(serverID) {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("resolved server %q: unsupported scheme (only http and https are supported)", server))
	}
	cacheKey := config.CacheKey(params.Name, serverID, clusterID)

	switch params.Name {
	case "":
		p.Notice("caching disabled: no --name; each call re-authenticates")
	default:
		// Skip the re-check when the caller already probed this exact key (run's
		// flag fast path or the shim's pre-probe): probing the same key twice would
		// double any "cache read failed" notice on a real store error.
		// When the keys differ (e.g. cluster sourced from config, not from flags),
		// this is the first probe for the resolved key. quiet is set only when the
		// fast-path probe NOTICED a real store error (ProbedNoticed) — a clean miss
		// on the flag key must NOT suppress a distinct error on the resolved key,
		// which would degrade the invariant from "exactly once" to "zero notices".
		if cacheKey != params.ProbedKey {
			quiet := params.ProbedNoticed
			if r, hit := t.cachedResultQ(cacheKey, quiet, p); hit {
				return r, nil
			}
		}
	}

	input := &loginInput{
		server:          server,
		clusterID:       clusterID,
		authProvider:    params.AuthProvider,
		skipVerify:      params.SkipVerify,
		authFlow:        params.AuthFlow,
		usernameDefault: params.UsernameDefault,
	}

	client, err := httpx.New(httpx.Options{CACert: caPEM, Insecure: input.skipVerify, Timeout: httpx.DefaultTimeout})
	if err != nil {
		return runtime.Result{}, err
	}

	rancherToken, err := t.storedCredToken(ctx, client, server, clusterID, params.Name, params.legacyShim, p)
	switch {
	case errors.Is(err, errNoStoredCredential):
		// Built only when a live login is needed: the cached-credential
		// path, the steady state under kubectl's exec plugin, never dials
		// an identity provider.
		input.idpClient = httpx.NewExternal(httpx.DefaultTimeout)
		var res SessionAcquireResult
		res, err = loginAndGenerateCred(ctx, client, input, p)
		if err != nil {
			return runtime.Result{}, err
		}
		rancherToken = Token{Token: res.Create.Bearer, ExpiresAt: res.Create.ExpiresAt}
	case err != nil:
		return runtime.Result{}, err
	}
	if strings.TrimSpace(rancherToken.Token) == "" {
		return runtime.Result{}, clierr.New(clierr.CodeAuth, "authentication returned an empty token; check the auth provider and credentials")
	}

	var tokenExpiry time.Time
	if rancherToken.ExpiresAt != "" {
		tokenExpiry, err = time.Parse(time.RFC3339, rancherToken.ExpiresAt)
		if err != nil {
			return runtime.Result{}, clierr.New(clierr.CodeGeneric, "parsing token expiry: "+err.Error())
		}
	}

	if params.Name != "" {
		switch {
		case tokenExpiry.IsZero() && !params.legacyShim:
			// A zero expiry would be cached and read back as never-expiring
			// (GetCachedCred only evicts a past, non-zero expiry) — never
			// cache an immortal stale token on the native path. The legacy
			// `token` shim opts in: the legacy CLI cached these
			// indefinitely, and the read path already treats zero expiry
			// as never-expiring.
			p.Notice("not caching: the new token has no expiry")
		case rancherToken.noDiskCache:
			// A credential-helper short-lived token served through the legacy
			// fallback is memory-only by contract — never write it to disk.
			p.Notice("not caching: the credential helper serves this token from memory only")
		default:
			if cacheErr := t.store.SetCachedCred(cacheKey, rancherToken.Token, tokenExpiry, params.Name, serverID, clusterID); cacheErr != nil {
				p.Notice("cache write failed: " + cacheErr.Error())
			}
		}
	}

	return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(rancherToken.Token, tokenExpiry)}, nil
}

// cachedResultQ returns an ExecCredential result for a warm cache entry and
// whether it hit. When quiet is true a real
// store error is silently treated as a miss rather than noticed. Used when the
// caller's fast-path probe already noticed the error for this invocation so the
// resolved-key re-check does not fire the notice a second time.
func (t tokenCmd) cachedResultQ(key string, quiet bool, p runtime.Prompter) (runtime.Result, bool) {
	tok, expiry, err := t.store.GetCachedCred(key)
	if err == nil {
		return runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(tok, expiry)}, true
	}
	if !errors.Is(err, config.ErrNoToken) && !quiet {
		p.Notice("cache read failed: " + err.Error())
	}
	return runtime.Result{}, false
}

// computeCacheProbeKey validates and normalizes name, server, and cluster into
// the cache-key string that both probeFlagCache and shimProbeFlagCache use.
// Keeping this logic here means future scheme, normalization, or CacheKey-shape
// changes need only be applied once — the "fast-path probed key == slow-path
// resolved key" invariant and the notice-once suppression both depend on the two
// probes producing identical keys. Returns ("", false) when the probe is
// impossible: a required field is empty, the URL is not parseable, or the scheme
// is not http/https.
func computeCacheProbeKey(name, server, cluster string) (string, bool) {
	if name == "" || server == "" {
		return "", false
	}
	normalized := applyDefaultScheme(server)
	cu, err := credhelper.CanonicalServerURL(normalized)
	if err != nil {
		return "", false
	}
	if !config.IsSupportedScheme(cu) {
		return "", false
	}
	return config.CacheKey(name, cu, cluster), true
}

// probeFlagCache attempts a flag-only cache probe when name and server are both
// given — the same fast-path logic run uses, factored out so the legacy token
// shim can call it before runWithParams. Returns (key, noticed, result, hit).
// A miss or any condition that makes the probe impossible returns ("", false, {}, false).
// The key is returned even on a miss so the caller can set ProbedKey; noticed
// is true when the probe fired a "cache read failed" notice (a real, non-ErrNoToken
// error). ProbedNoticed in runParams uses this flag to distinguish a clean miss
// (do NOT suppress the resolved-key notice) from a noticed error (suppress).
//
// The probe itself is probeCacheWith (shared with shimProbeFlagCache),
// parameterized on the store-backed read. The store buffers its own
// notices (drained elsewhere via its NoticeSource) rather than returning
// one inline, so this read closure's own notice is always "" and the
// discarded 5th return value is never meaningful here.
func (t tokenCmd) probeFlagCache(name, server, cluster string, p runtime.Prompter) (key string, noticed bool, r runtime.Result, hit bool) {
	key, noticed, r, hit, _ = probeCacheWith(name, server, cluster, func(k string) (string, time.Time, string, error) {
		tok, expiry, err := t.store.GetCachedCred(k)
		return tok, expiry, "", err
	}, p)
	return key, noticed, r, hit
}

// probeCacheWith is the one flag-only cache probe: compute the key via
// computeCacheProbeKey, read through read, and classify the outcome. Shared
// by probeFlagCache (store-backed) and shimProbeFlagCache (direct read-only
// file probe) so hit construction and the notice-once/notice-zero
// classification — noticed is true only for a real, non-ErrNoToken error —
// cannot drift between the two paths. warn is a notice read returned
// alongside the token (a loose-permission finding on the shim's direct file
// read, say) — fired independently of hit/miss, since read may notice
// before it even knows whether the key is present.
func probeCacheWith(name, server, cluster string, read func(string) (string, time.Time, string, error), p runtime.Prompter) (key string, noticed bool, r runtime.Result, hit bool, warn string) {
	key, ok := computeCacheProbeKey(name, server, cluster)
	if !ok {
		return "", false, runtime.Result{}, false, ""
	}
	tok, expiry, warn, err := read(key)
	if err == nil {
		r := runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(tok, expiry)}
		if warn != "" {
			r.Warnings = append(r.Warnings, warn)
		}
		return key, false, r, true, warn
	}
	noticed = !errors.Is(err, config.ErrNoToken)
	if noticed {
		p.Notice("cache read failed: " + err.Error())
	}
	return key, noticed, runtime.Result{}, false, warn
}

type deleteTokenCmd struct {
	dir   string
	store config.CredStore
}

// NewDeleteTokenCommand builds `auth delete-token` (local): clear cached
// exec-credential token(s). A positional name is the cache entry name; --all
// clears every cached credential.
func NewDeleteTokenCommand(dir string, store config.CredStore) runtime.Command {
	return &deleteTokenCmd{dir: dir, store: store}
}

func (deleteTokenCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:        "auth delete-token",
		Usage:       "Delete cached exec-credential token(s)",
		CommandType: runtime.Local,
		Args:        []runtime.ArgSpec{{Name: "name", Type: "string"}},
		Flags:       []runtime.ArgSpec{{Name: "all", Type: "bool", Desc: "delete all cached credentials"}},
		Examples: []string{
			"rancher auth delete-token --all",
			"rancher auth delete-token prod",
		},
		Shape: runtime.ShapeText,
	}
}

func (d deleteTokenCmd) Execute(_ context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	all := in.Bool("all")
	keys := in.Args()
	if all && len(keys) > 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"specify either credential name(s) or --all, not both (e.g. rancher auth delete-token prod, or rancher auth delete-token --all)")
	}
	if all {
		if err := d.store.DeleteAllCachedCreds(); err != nil {
			return runtime.Result{}, err
		}
		res := runtime.TextLines("all cached credentials deleted")
		res.Kind = "TokenDelete"
		res.Data = map[string]any{"deleted": "all"}
		return res, nil
	}
	if len(keys) == 0 {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"provide a cached-credential name or --all")
	}
	// Resolve each argument against the cached record set: a bare display name
	// matches EVERY entry with that name (a legacy bare entry and a scoped entry
	// sharing the name are the same logical credential — deleting cached tokens
	// is always safe, they re-auth), and the full composite key matches one.
	// Deletion is idempotent: an unknown or already-deleted name is a success
	// with nothing removed, so teardown scripts can call it unconditionally.
	infos, err := d.store.ListCachedCreds()
	if err != nil {
		return runtime.Result{}, err
	}
	deleted := make([]string, 0, len(keys))
	for _, arg := range keys {
		for _, info := range infos {
			if info.Name != arg && info.Key != arg {
				continue
			}
			if derr := d.store.DeleteCachedCred(info.Key); derr != nil {
				return runtime.Result{}, clierr.Wrap(derr,
					fmt.Sprintf("failed to delete %q: %v (already deleted: %v)", arg, derr, deleted))
			}
			deleted = append(deleted, info.Key)
		}
	}
	var b strings.Builder
	for _, key := range deleted {
		b.WriteString("cached credential " + key + " deleted\n")
	}
	if len(deleted) == 0 {
		// Deletion is idempotent (an unknown or already-deleted name is a
		// no-op success, per the doc above); ShapeText requires non-empty
		// Text on every default-format success, so the zero-deleted case
		// needs its own line too.
		b.WriteString("no matching cached credential\n")
	}
	return runtime.Result{Kind: "TokenDelete", Data: map[string]any{"deleted": deleted}, Text: b.String()}, nil
}

// CompleteArgs implements runtime.ShellCompleter: `auth delete-token <TAB>`
// completes cached exec-credential entry display names from a strictly
// read-only cache scan (AC-12). Composite keys are not offered.
func (d deleteTokenCmd) CompleteArgs(_ context.Context, view runtime.ConfigView) []string {
	dir := view.ConfigDir()
	if dir == "" {
		return nil
	}
	infos, err := config.ReadOnlyCachedCreds(dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, info := range infos {
		if info.Name != "" && !seen[info.Name] {
			seen[info.Name] = true
			out = append(out, info.Name)
		}
	}
	sort.Strings(out)
	return out
}

// LoginRequest is the constructible counterpart to loginInput: loginInput's
// fields are unexported, so no command outside this package could build one
// to drive the identity-provider dance. `auth login`'s own provider flow
// (EXT-R-8) is the first consumer.
type LoginRequest struct {
	Server       string
	ClusterID    string
	AuthProvider string
	AuthFlow     string
	// IdPClient dials the identity provider's OAuth endpoints (see
	// loginInput.idpClient). Required for OAuth providers.
	IdPClient *http.Client
	// ResponseType is the shape asked of Rancher's login endpoint. Empty
	// keeps get-token's kubeconfig form.
	ResponseType string
}

// loginNotAttempted is the result of a login that failed before its request
// reached the wire: nothing was created, and the marker names nothing.
func loginNotAttempted(input *loginInput) CreateResult {
	return CreateResult{Outcome: CreateNotAttempted, Marked: input.description != ""}
}

// loginResponseType is the responseType a login carries: the caller's when
// set, else the kubeconfig form get-token has always asked for, scoped to
// the cluster when one is named.
func loginResponseType(input *loginInput) string {
	if input.responseType != "" {
		return input.responseType
	}
	if input.clusterID != "" {
		return "kubeconfig_" + input.clusterID
	}
	return "kubeconfig"
}

// loginAndGenerateCred creates the session token S. Its result is
// authoritative even when the error is non-nil: the outcome says whether S
// may exist, and the SAML handoff says where to look for it.
func loginAndGenerateCred(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (SessionAcquireResult, error) {
	authProviders, err := getAuthProviders(ctx, client, input.server)
	if err != nil {
		return SessionAcquireResult{}, err
	}
	selectedProvider, err := selectAuthProvider(ctx, authProviders, input.authProvider, p)
	if err != nil {
		return SessionAcquireResult{}, err
	}
	input.authProvider = selectedProvider.GetType()

	if samlProviders[input.authProvider] {
		if p.NoInput() {
			return SessionAcquireResult{}, clierr.New(clierr.CodeAuth,
				"interactive login required for "+input.authProvider+"; run without --no-input, or use a cached --name")
		}
		return samlAuth(ctx, client, input, p)
	}
	var create CreateResult
	if oauthProviders[input.authProvider] {
		create, err = oauthAuth(ctx, client, input, selectedProvider, p)
	} else {
		create, err = basicAuth(ctx, client, input, p)
	}
	return SessionAcquireResult{Create: create}, err
}

func basicAuth(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (CreateResult, error) {
	notAttempted := loginNotAttempted(input)
	label := "Enter username: "
	if def := input.usernameDefault; def != "" {
		label = "Enter username [" + def + "]: "
	}
	username, err := p.Prompt(ctx, label)
	if err != nil {
		if errors.Is(err, ui.ErrNoInput) {
			return notAttempted, clierr.New(clierr.CodeAuth,
				"interactive login required for "+input.authProvider+"; run in a terminal, or use a cached --name")
		}
		return notAttempted, err
	}
	// Trim before both the emptiness check and the request: incidental
	// padding (a paste with a trailing newline) must not reach the auth
	// provider as part of the username.
	username = strings.TrimSpace(username)
	if username == "" {
		username = input.usernameDefault
	}
	password, err := p.PromptSecret(ctx, "Enter password: ")
	if err != nil {
		if errors.Is(err, ui.ErrNoInput) {
			return notAttempted, clierr.New(clierr.CodeAuth,
				"interactive login required for "+input.authProvider+"; run in a terminal, or use a cached --name")
		}
		return notAttempted, err
	}

	body := map[string]any{
		"type": input.authProvider, "responseType": loginResponseType(input),
		"username": username, "password": password,
	}
	if input.description != "" {
		body["description"] = input.description
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return notAttempted, fmt.Errorf("failed to marshal request body: %w", err)
	}

	reqURL := fmt.Sprintf(loginURL, input.server)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return notAttempted, fmt.Errorf("error creating request: %w", err)
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	resp, respBody, err := httpx.Do(client, req)
	r := loginCreateResult(resp, respBody, err, createdAt, input.description != "")
	return r, loginError(resp, err)
}

// loginError classifies the outcome of a login POST the same way for every
// flow that carries credentials to Rancher: a transport error keeps its
// cause, 401 and 403 are AUTH rejections, any other non-201 is GENERIC.
// A caller reading the error code sees one class for one fact.
func loginError(resp *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("error logging user in: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return clierr.New(clierr.CodeAuth, fmt.Sprintf("login rejected: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	if resp.StatusCode != http.StatusCreated {
		return clierr.New(clierr.CodeGeneric, fmt.Sprintf("login failed: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	return nil
}

// loginCreateResult classifies a login response: the body declares which
// token kind Rancher created, and the classifier reads the identity out of
// that shape. A response that declares no shape leaves the kind unknown.
func loginCreateResult(resp *http.Response, body []byte, err error, createdAt string, marked bool) CreateResult {
	kind, _ := LoginResponseShape(body)
	return classifyCreate(resp, body, err, createdAt, marked, kind)
}

// samlAuthTimeout and samlPollInterval are package vars so tests can shrink
// the poll interval to exercise the ctx.Done() case deterministically without
// waiting out a real 10s tick.
var (
	samlAuthTimeout  = 15 * time.Minute
	samlPollInterval = 10 * time.Second
)

// SetSAMLTimingForTest shrinks the SAML wait so a test can drive the
// asynchronous state machine without waiting out a real poll tick, and
// returns the function that puts the production timing back.
func SetSAMLTimingForTest(timeout, interval time.Duration) (restore func()) {
	prevTimeout, prevInterval := samlAuthTimeout, samlPollInterval
	samlAuthTimeout, samlPollInterval = timeout, interval
	return func() { samlAuthTimeout, samlPollInterval = prevTimeout, prevInterval }
}

// samlAuth signs in through the browser: it publishes a URL carrying a
// request id and a public key, then polls Rancher's authtokens collection
// for the encrypted token the browser deposits there. Nothing here creates a
// token — the server does, out of band — so every failure after the URL is
// published leaves a session token that may exist and can only be found
// through the handoff this returns. The created token is always v3 and never
// carries a description, so it can never be marked.
func samlAuth(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (SessionAcquireResult, error) {
	res := SessionAcquireResult{Create: CreateResult{Outcome: CreateNotAttempted, Kind: TokenKindV3}}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return res, fmt.Errorf("error generating key: %w", err)
	}

	publicKey := privateKey.PublicKey
	marshalKey, err := json.Marshal(publicKey)
	if err != nil {
		return res, fmt.Errorf("error marshaling public key: %w", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(marshalKey)

	id, err := generateKey()
	if err != nil {
		return res, fmt.Errorf("error generating request id: %w", err)
	}

	tokenURL := fmt.Sprintf(authTokenURL, input.server, id)

	getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, bytes.NewBuffer(nil))
	if err != nil {
		return res, fmt.Errorf("error creating get token request: %w", err)
	}
	getReq.Header.Set("content-type", "application/json")
	getReq.Header.Set("accept", "application/json")

	deleteReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, tokenURL, bytes.NewBuffer(nil))
	if err != nil {
		return res, fmt.Errorf("error creating delete token request: %w", err)
	}
	deleteReq.Header.Set("content-type", "application/json")
	deleteReq.Header.Set("accept", "application/json")

	loginURL, err := url.Parse(input.server + "/dashboard/auth/login")
	if err != nil {
		return res, fmt.Errorf("error parsing login url: %w", err)
	}

	q := url.Values{}
	q.Set("cli", "true")
	q.Set("requestId", id)
	q.Set("publicKey", encodedKey)
	q.Set("responseType", loginResponseType(input))
	loginURL.RawQuery = q.Encode()

	// The handoff is recorded before the URL reaches the operator: from the
	// moment it can be opened, a token may exist, and every exit below must
	// carry the criteria that find it.
	res.Handoff = &SAMLHandoff{Provider: input.authProvider, RequestID: id, HandoffAt: time.Now().UTC().Format(time.RFC3339)}
	res.Create.Outcome = CreateUnknown
	defer func() { res.Handoff.PollEndedAt = time.Now().UTC().Format(time.RFC3339) }()

	p.Notice(fmt.Sprintf("\nLogin Request Id: %s\n", id))
	p.Notice(fmt.Sprintf("\nLogin to Rancher Server at %s \n", loginURL))

	timeout := time.NewTimer(samlAuthTimeout)
	defer timeout.Stop()
	poll := time.NewTicker(samlPollInterval)
	defer poll.Stop()

	for {
		select {
		case <-poll.C:
			resp, respBody, err := httpx.Do(client, getReq)
			if err != nil {
				return res, fmt.Errorf("error fetching auth token: %w", err)
			}
			switch resp.StatusCode {
			case http.StatusOK:
			case http.StatusNotFound:
				continue
			default:
				return res, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
			}

			var deposited Token
			err = json.Unmarshal(respBody, &deposited)
			if err != nil {
				return res, fmt.Errorf("error unmarshaling auth token response: %w", err)
			}
			if deposited.Token == "" {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(deposited.Token)
			if err != nil {
				return res, fmt.Errorf("error decoding auth token: %w", err)
			}
			decryptedBytes, err := privateKey.Decrypt(nil, decoded, &rsa.OAEPOptions{Hash: crypto.SHA256})
			if err != nil {
				return res, fmt.Errorf("error decrypting auth token: %w", err)
			}
			bearer := string(decryptedBytes)

			resp, _, err = httpx.Do(client, deleteReq)
			if err == nil {
				switch resp.StatusCode {
				case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
				default:
					err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
				}
			}
			if err != nil {
				p.Notice("error deleting auth token: " + err.Error())
			}

			res.Create.ExpiresAt = deposited.ExpiresAt
			// The deposited bearer is the only evidence of the token's
			// identity: this flow has no create response to read a name from.
			ref, ok := TokenRefFromValidatedBearer(TokenKindV3, bearer)
			if !ok {
				res.Create.Outcome = CreateUnidentified
				return res, clierr.New(clierr.CodeAuth, "the auth token deposited for this login is not a usable Rancher token")
			}
			res.Create.Outcome, res.Create.Ref, res.Create.Bearer = CreateKnown, &ref, bearer
			return res, nil

		case <-timeout.C:
			return res, clierr.Retryable(clierr.CodeAuth, "timed out waiting for the auth token")
		case <-ctx.Done():
			// Abort promptly on caller cancellation (Ctrl-C via main's
			// signal-cancellable context, SIGTERM, test timeout) instead of
			// waiting for the next poll tick, mirroring oauthAuthCodeAuth's wait.
			return res, ctx.Err()
		}
	}
}

func getAuthProviders(ctx context.Context, client *http.Client, server string) ([]typedProvider, error) {
	authProvidersURL := fmt.Sprintf(authProviderURL, server)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authProvidersURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	resp, respBody, err := httpx.Do(client, req)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return nil, fmt.Errorf("error listing auth providers: %w", err)
	}

	if !gjson.ValidBytes(respBody) {
		return nil, errors.New("invalid JSON response")
	}
	data := gjson.GetBytes(respBody, "data").Array()

	var supportedProviders []typedProvider
	for _, provider := range data {
		providerType := provider.Get("type").String()

		if providerType != "" && supportedAuthProviders[providerType] {
			var tp typedProvider

			switch providerType {
			case "azureADProvider":
				tp = &AzureADProvider{}
			case "localProvider":
				tp = &LocalProvider{}
			default:
				tp = &AuthProvider{}
			}

			err = json.Unmarshal([]byte(provider.Raw), tp)
			if err != nil {
				return nil, fmt.Errorf("error decoding the auth provider %s: %w", providerType, err)
			}

			if tp.GetType() == "localProvider" {
				supportedProviders = append([]typedProvider{tp}, supportedProviders...)
			} else {
				supportedProviders = append(supportedProviders, tp)
			}
		}
	}

	return supportedProviders, nil
}

func selectAuthProvider(ctx context.Context, providers []typedProvider, want string, p runtime.Prompter) (typedProvider, error) {
	if len(providers) == 0 {
		return nil, clierr.New(clierr.CodeGeneric, "no auth provider configured")
	}

	cands := func() []clierr.Candidate {
		cs := make([]clierr.Candidate, 0, len(providers))
		for _, p := range providers {
			cs = append(cs, clierr.Candidate{ID: p.GetType(), Name: p.GetType()})
		}
		return cs
	}

	if want != "" {
		for _, prov := range providers {
			if prov.GetType() == want {
				return prov, nil
			}
		}
		return nil, clierr.New(clierr.CodeNotFound, "auth provider not found: "+want).
			WithCandidates(cands())
	}

	if len(providers) == 1 {
		return providers[0], nil
	}

	if p.CanPrompt() {
		var b strings.Builder
		for i, prov := range providers {
			fmt.Fprintf(&b, "%d - %s\n", i, prov.GetType())
		}
		p.Notice("Auth providers:\n" + b.String())
		sel, err := p.Prompt(ctx, "Select auth provider: ")
		if err != nil {
			return nil, err
		}
		if idx, aerr := strconv.Atoi(strings.TrimSpace(sel)); aerr == nil && idx >= 0 && idx < len(providers) {
			return providers[idx], nil
		}
		// invalid (non-numeric / out-of-range) pick → fall through to AMBIGUOUS_SELECTION
	}

	return nil, clierr.New(clierr.CodeAmbiguousSelection, "multiple auth providers; select one").
		WithCandidates(cands())
}

func generateKey() (string, error) {
	characters := "abcdfghjklmnpqrstvwxz12456789"
	tokenLength := 32
	token := make([]byte, tokenLength)
	for i := range token {
		r, err := rand.Int(rand.Reader, big.NewInt(int64(len(characters))))
		if err != nil {
			return "", err
		}
		token[i] = characters[r.Int64()]
	}

	return string(token), nil
}

// resolveConn fills server/CA/cluster from flags, falling back to the CLI config
// (which already merges env over the file). Flags win over config. An explicit
// --server with no --cacert never inherits the CURRENT context's CA — that CA
// belongs to whatever server the context happens to point at, which may differ
// from the server just named; instead it borrows the CA of the configured
// server entry whose URL actually matches (see caForServer).
// When ignoreCurrentContext is true caForServer's current-server CA preference is
// suppressed: an empty --cluster stays unscoped, and among same-host CA
// candidates sorted order wins instead of the current-server entry — driven
// purely by the flags, never by stored current-context state.
// resolveConn accepts the three flag values that drive connection resolution
// as plain strings rather than a runtime.Input, so callers (runWithParams)
// can pass runParams fields directly without an adapter wrapper.
func (t tokenCmd) resolveConn(flagServer, flagCluster, caIn string, ignoreCurrentContext bool) (server, caPEM, cluster string, err error) {
	server = flagServer
	cluster = flagCluster
	explicitServer := server != ""
	// Normalize the flag value's scheme here, once: it must happen before
	// caForServer below, and a config-sourced server (from eff.URL) is always
	// already scheme-qualified (login rejects a schemeless --server), so
	// there is no second site left needing this. Guarded on explicitServer —
	// an empty server must stay "" for the fallback-to-config check below,
	// not become the bogus non-empty "https://".
	if explicitServer {
		server = applyDefaultScheme(server)
	}

	// A ignoreCurrentContext caller is flag-driven by definition: an absent
	// --server must fail here, never be filled from the config's current
	// context below. This also keeps the needsEff gate honest — past this
	// point server == "" implies !ignoreCurrentContext.
	if server == "" && ignoreCurrentContext {
		return "", "", "", clierr.New(clierr.CodeValidation, "--server is required")
	}

	// The native get-token path requires --cluster: a token must never be created
	// for a cluster the caller didn't name. Shim calls (ignoreCurrentContext) keep
	// the legacy semantic where an empty --cluster is a deliberate unscoped
	// create — still purely flag-driven, never filled from stored state.
	if cluster == "" && !ignoreCurrentContext {
		return "", "", "", clierr.New(clierr.CodeValidation, "--cluster is required")
	}

	if server == "" || caIn == "" {
		cfg, cerr := config.Load(config.FilePath(t.dir))
		if cerr != nil {
			return "", "", "", cerr
		}
		// cfg.Effective reads the current server's credential from the store —
		// GetToken for a server that may be entirely unrelated to the --server
		// the caller wants. Call it only when a field actually needs it:
		// URL from config (server == "") or CA from the current server
		// (!explicitServer). With an explicit --server and ignoreCurrentContext (every
		// shim call), config.yaml may still be needed for caForServer, but
		// Effective must not run — a hard store error for an unrelated server must
		// never abort an otherwise healthy create.
		// needsEff mirrors the two Effective fields consumed below:
		//   - eff.URL when server == ""
		//   - eff.CA  when caIn == "" && !explicitServer
		// If a new Effective-sourced field is added below, add its condition here.
		needsEff := server == "" || (caIn == "" && !explicitServer)
		var eff config.Effective
		if needsEff {
			var effErr error
			eff, effErr = cfg.Effective(t.store)
			if effErr != nil {
				return "", "", "", effErr
			}
		}
		if server == "" {
			server = eff.URL
		}
		if caIn == "" {
			if explicitServer {
				caIn = caForServer(cfg, server, ignoreCurrentContext)
			} else {
				caIn = eff.CA
			}
		}
	}

	if server == "" {
		return "", "", "", clierr.New(clierr.CodeValidation,
			"--server is required; pass it, or add and log in to a server first")
	}
	caPEM, err = config.ResolveCA(caIn)
	if err != nil {
		return "", "", "", clierr.New(clierr.CodeValidation, "reading --cacert: "+err.Error())
	}
	return server, caPEM, cluster, nil
}

// schemeRE matches an explicit URI scheme followed by "://", independent of
// url.Parse's inference. url.Parse parses "host:8443" as scheme "host" and
// fails on schemeless bracketed IPv6, so syntactic detection is necessary.
var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+\-.]*://`)

// applyDefaultScheme prepends "https://" when the value has no scheme, so
// that both the flag fast path and resolveConn share one normalization policy.
// An empty input is returned unchanged so the caller can detect the "no flag"
// case. Scheme detection uses schemeRE rather than url.Parse to handle
// host:port and bracketed IPv6 authorities correctly.
func applyDefaultScheme(s string) string {
	if s == "" {
		return s
	}
	if !schemeRE.MatchString(s) {
		return "https://" + s
	}
	return s
}

// extTokensPath and ErrExtGroupAbsent are reached ONLY through the Rancher
// proxy path on the LOCAL cluster's apiserver — ext.cattle.io is an
// aggregated/CRD API, never served at the server root, so a bare /apis/...
// 404s on every Rancher.
const ExtTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

// ErrExtGroupAbsent signals a 404 on the derived token create call itself — the aggregated
// ext.cattle.io API isn't served (an older Rancher), not that some other
// resource is missing.
var ErrExtGroupAbsent = errors.New("ext.cattle.io not served")

// errDerivedCreateDenied wraps a 401/403 from the derived token create call: the bearer credential
// itself is invalid or insufficient, as opposed to a network/TLS failure or
// a server error — the two classes need different handling by the caller
// (fall back to interactive vs. surface the real failure).
var errDerivedCreateDenied = errors.New("token create rejected: invalid or insufficient credential")

// CreateDerivedToken asks the ext Token store to create a purpose-made derived
// token (bearer bearerToken), scoped to clusterID when set (spec.clusterName,
// the same field internal/engine/extres/create.go's `create token --cluster`
// uses), carrying description as spec.description. It returns what the create
// left behind — the resource name, bearer, and expiry when the response
// identified them — on the error path too, so a caller that must clean up
// knows what exists. ErrExtGroupAbsent signals the group-level 404 that means
// an older Rancher; errDerivedCreateDenied signals a 401/403 (the bearer credential is
// bad), distinct from any other error (network/TLS/5xx), which callers must
// treat as a real failure, not a "try something else" signal. ttlMs <= 0 omits
// spec.ttl entirely (server default). This is the SAME derived-token path
// `auth login` uses (via resolveProviderLoginToken in package commands), so
// get-token's stored-credential path creates exactly the same shape of token.
func CreateDerivedToken(ctx context.Context, client *http.Client, base, bearerToken, clusterID string, ttlMs int64, description string) (CreateResult, error) {
	notAttempted := CreateResult{Outcome: CreateNotAttempted, Kind: TokenKindExt, Marked: description != ""}
	spec := map[string]any{
		"description": description,
	}
	if ttlMs > 0 {
		spec["ttl"] = ttlMs
	}
	if clusterID != "" {
		spec["clusterName"] = clusterID
	}
	body, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return notAttempted, fmt.Errorf("building the token create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+ExtTokensPath, bytes.NewReader(body))
	if err != nil {
		return notAttempted, fmt.Errorf("building the token create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	createdAt := time.Now().UTC().Format(time.RFC3339)
	resp, respBody, err := httpx.Do(client, req)
	r := classifyCreate(resp, respBody, err, createdAt, description != "", TokenKindExt)
	if err != nil {
		// The cause is kept: a cancelled operation must still be recognizable
		// as cancellation after cleanup has wrapped the failure.
		return r, clierr.New(clierr.CodeGeneric, "creating derived token: "+err.Error()).WithCause(err)
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return r, ErrExtGroupAbsent
	case http.StatusUnauthorized, http.StatusForbidden:
		return r, fmt.Errorf("%w: %d %s", errDerivedCreateDenied, resp.StatusCode, http.StatusText(resp.StatusCode))
	case http.StatusOK, http.StatusCreated:
		if r.Bearer == "" {
			return r, clierr.New(clierr.CodeGeneric, "token create response contained no bearer token")
		}
		return r, nil
	default:
		return r, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("creating derived token: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
}

// hostLoginDescription is the description a token create carries when no
// login operation owns it: get-token's per-cluster creates, which no cleanup
// path has to find again.
func hostLoginDescription() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return "rancher-cli login @ " + host
}

// matchingServerNames returns the names of the cfg.Servers entries whose URL
// matches server (compared by scheme+host via config.SameServerURL), sorted
// for determinism. caForServer and storedTokenForServer both select from this
// one candidate set, so a change to the matching policy cannot apply to one
// and silently diverge in the other.
func matchingServerNames(cfg *config.Config, server string) []string {
	var names []string
	for n, s := range cfg.Servers {
		if config.SameServerURL(s.URL, server) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// errNoStoredCredential is storedCredToken's sentinel for "try the
// interactive provider flow instead" — no usable stored credential exists,
// or the create was rejected as an invalid/insufficient credential
// (errDerivedCreateDenied). Any other error storedCredToken returns is a real
// failure (network/TLS/5xx, or a resolution error for --cluster) that the
// caller must surface directly, never paper over with "interactive login
// required".
var errNoStoredCredential = errors.New("no usable stored login credential")

// serverCandidateRank orders storedTokenForServer's BaseURL-matching
// candidates: (0) an exact --name match, (1) a candidate whose name is a
// prefix of --name in the "<server>-<clusterID>" entry-name shape, (2) the
// current server, (3) everything else (broken by the
// lexicographic order candidates already carry on input).
// serverCandidateRankCount is the number of distinct ranks serverCandidateRank
// returns (0..serverCandidateRankCount-1), so callers can scan every rank
// without a second magic number drifting out of sync with the switch below.
const serverCandidateRankCount = 4

func serverCandidateRank(name, wantName, curServer string) int {
	switch {
	case wantName != "" && name == wantName:
		return 0
	case wantName != "" && strings.HasPrefix(wantName, name+"-"):
		return 1
	case curServer != "" && name == curServer:
		return 2
	default:
		return 3
	}
}

// storedTokenForServer finds the configured server entry whose URL matches
// server (compared by scheme+host via kube.BaseURL) and has a stored login
// token, and returns that name and token. Selects from the same
// matchingServerNames candidate set caForServer uses, but resolves the
// credential store's token instead of the config's CA. Multiple Servers entries can share a BaseURL (e.g. re-login
// under a new --name against the same host); serverCandidateRank orders
// them deterministically, and every candidate in that order is tried in
// turn — the first one with an actual stored token wins, so a
// higher-ranked-but-tokenless sibling never masks a lower-ranked one that
// does have a token. p is noticed only when more than one candidate AT THE
// WINNING RANK had a stored token — an exact --name match (rank 0) winning
// outright is a certainty, not a tie-break, even if some unrelated
// lower-ranked server also happens to have a stale token.
// ignoreCurrentContext gates every non-sorted tier: wantName is forced to ""
// (the shim's name is a cache-slot label, never an entry-name preference, so
// ranks 0 and 1 must not fire even against a hand-edited "legacy:"-named
// entry) and curServer is forced to "" (rank 2, the operator's
// interactive-session state) — stored-login selection falls directly to
// rank-3 (deterministic sorted order), consistent with how ignoreCurrentContext
// gates the cluster and CA fallbacks in resolveConn/caForServer.
func storedTokenForServer(cfg *config.Config, store config.CredStore, server, wantName string, ignoreCurrentContext bool, p runtime.Prompter) (name, tok string, ref config.ServerRef, ok bool, err error) {
	candidates := matchingServerNames(cfg, server)
	if len(candidates) == 0 {
		return "", "", config.ServerRef{}, false, nil
	}

	// A ignoreCurrentContext caller's wantName is a cache-slot label
	// ("legacy:<user>"), not a principal or entry-name preference:
	// stored-login selection must follow server-ranking alone, so the
	// name-driven tiers (ranks 0 and 1) are suppressed the same way rank 2's
	// current-server tier is below. A hand-edited config.yaml can contain a
	// literally "legacy:"-prefixed entry name (auth login rejects the prefix,
	// but nothing rewrites existing files), so this cannot rely on the
	// entry-name namespace staying clean.
	if ignoreCurrentContext {
		wantName = ""
	}
	curServer := ""
	if !ignoreCurrentContext {
		curServer = cfg.CurrentServer
	}
	rankOf := func(n string) int { return serverCandidateRank(n, wantName, curServer) }
	sort.SliceStable(candidates, func(i, j int) bool {
		return rankOf(candidates[i]) < rankOf(candidates[j])
	})

	type hit struct {
		name, tok string
		ref       config.ServerRef
	}
	hitsByRank := map[int][]hit{}
	// firstHardErr remembers the FIRST non-not-found helper/store failure — a
	// missing helper binary, timeout, malformed response, version mismatch,
	// backend outage — so a genuinely misconfigured store is surfaced rather
	// than silently treated as "no credential" and papered over by the
	// interactive fallback. A clean miss (ErrNoToken / empty token) is skipped.
	var firstHardErr error
	for _, n := range candidates {
		r := config.ServerRef{Name: n, URL: cfg.Servers[n].URL}
		t, gerr := store.GetToken(r)
		if gerr != nil {
			if !errors.Is(gerr, config.ErrNoToken) && firstHardErr == nil {
				firstHardErr = gerr
			}
			continue
		}
		if strings.TrimSpace(t) == "" {
			continue
		}
		hitsByRank[rankOf(n)] = append(hitsByRank[rankOf(n)], hit{n, t, r})
	}
	for rank := range serverCandidateRankCount {
		hs := hitsByRank[rank]
		if len(hs) == 0 {
			continue
		}
		if len(hs) > 1 {
			p.Notice(fmt.Sprintf("%d configured servers matching this URL have stored credentials; using %q", len(hs), hs[0].name))
		}
		return hs[0].name, hs[0].tok, hs[0].ref, true, nil
	}
	// No candidate produced a token. If any candidate failed with a hard
	// error, surface it — a helper misconfiguration must not silently fall
	// through to interactive auth.
	return "", "", config.ServerRef{}, false, firstHardErr
}

// clustersPath is the management Cluster collection/object path, reached
// through the same local-cluster proxy ExtTokensPath uses.
const clustersPath = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"

// resolveClusterIDForCreate resolves clusterID (an id, a display name, "", or
// "local") to its Cluster id before creating, using the stored login
// credential as bearer. It shares projects.MatchOneOrError — the same
// collision-and-narrowing helper get/patch's ResolveCluster and the
// cloud-credential resolver use — so an id match never short-circuits
// ahead of checking every cluster's spec.displayName (id/display
// collisions are ambiguous, never a silent id-wins create), and ambiguous
// errors list only the colliding candidates. One list call serves both
// checks.
func resolveClusterIDForCreate(ctx context.Context, client *http.Client, base, bearerToken, clusterID string) (string, error) {
	if clusterID == "" || clusterID == "local" {
		return clusterID, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+clustersPath, nil)
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, "resolving --cluster: "+err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	resp, body, err := httpx.Do(client, req)
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, "resolving --cluster: "+err.Error())
	}
	if resp.StatusCode != http.StatusOK {
		return "", clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("resolving --cluster: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}

	var ids []string
	displayByID := map[string]string{}
	var candidates []clierr.Candidate
	for _, item := range gjson.GetBytes(body, "items").Array() {
		id := item.Get("metadata.name").String()
		display := item.Get("spec.displayName").String()
		ids = append(ids, id)
		displayByID[id] = display
		candidates = append(candidates, clierr.Candidate{ID: id, Name: display})
	}

	return projects.MatchOneOrError(clusterID, ids,
		func(id string) string { return displayByID[id] }, candidates,
		fmt.Sprintf("cluster not found: %q", clusterID), "",
		func(idFound bool, displayMatchCount int) string {
			if idFound {
				return fmt.Sprintf("%q matches both a cluster id and a different cluster's display name; pass the cluster id", clusterID)
			}
			return fmt.Sprintf("%d clusters have display name %q; pass the cluster id", displayMatchCount, clusterID)
		}, "")
}

// legacyTokenTTL is the bounded staleness a legacy (pre-ext.cattle.io)
// Rancher's stored-credential fallback caches under, since the raw stored
// login token carries no expiry of its own to propagate: caching nothing
// (a zero expiry) would force a full create-then-404 round trip on every
// single invocation, but caching forever would mean serving a revoked
// token indefinitely. 15 minutes trades a little staleness for a lot fewer
// round trips.
const legacyTokenTTL = 15 * time.Minute

// storedCredToken creates a cluster-scoped token from the stored login
// credential for server, non-interactively, using the same derived-token
// path `auth login` uses. errNoStoredCredential means the caller should
// fall through to the interactive provider flow exactly as if no stored
// credential existed — either none was found, or the create was rejected as
// an invalid/insufficient credential. An unreadable config.yaml is noticed
// and also falls through to errNoStoredCredential (the config load failure
// itself is never returned as a hard error here — see the doc on the
// —no-input leftovers in the package's fix history). Any other non-nil error
// is a real failure (network/TLS/5xx, or an unresolvable --cluster) that
// the caller must surface directly.
// ignoreCurrentContext is forwarded to storedTokenForServer to gate rank-2
// (current-server preference) — see runParams.legacyShim.
func (t tokenCmd) storedCredToken(ctx context.Context, client *http.Client, server, clusterID, wantName string, ignoreCurrentContext bool, p runtime.Prompter) (Token, error) {
	cfg, err := config.Load(config.FilePath(t.dir))
	if err != nil {
		p.Notice(fmt.Sprintf("config unreadable: %v — skipping token creation from the stored credential", err))
		return Token{}, errNoStoredCredential
	}
	name, stored, ref, ok, serr := storedTokenForServer(cfg, t.store, server, wantName, ignoreCurrentContext, p)
	if serr != nil {
		// A hard store/helper failure (missing binary, timeout, malformed
		// response, version mismatch, backend outage) must surface, never be
		// papered over by falling through to interactive auth.
		return Token{}, serr
	}
	if !ok {
		return Token{}, errNoStoredCredential
	}
	base := kube.BaseURL(server)
	resolvedCluster, rerr := resolveClusterIDForCreate(ctx, client, base, stored, clusterID)
	if rerr != nil {
		return Token{}, rerr
	}
	created, err := CreateDerivedToken(ctx, client, base, stored, resolvedCluster, 0, hostLoginDescription())
	bearer, expiresAt := created.Bearer, created.ExpiresAt
	if err != nil {
		switch {
		case errors.Is(err, ErrExtGroupAbsent):
			// Legacy Rancher with no ext.cattle.io: use the stored token
			// itself, matching auth login's own fallback. The stored token
			// normally carries no expiry of its own, so cache it for a
			// bounded, conservative TTL rather than never (a zero expiry) or
			// forever.
			//
			// But when the backing store is a credential helper serving a
			// SHORT-LIVED token (one with a real expiresAt, held in memory
			// only), that token must NOT be persisted to the on-disk cache,
			// and its returned expiry must never outlast the helper's real
			// one: cap the expiry at the helper's, and mark it noDiskCache.
			expiry := time.Now().Add(legacyTokenTTL)
			noDiskCache := false
			if slt, isSLT := t.store.(config.ShortLivedTokenStore); isSLT {
				if helperExp, short := slt.ShortLivedTokenExpiry(ref); short {
					noDiskCache = true
					if helperExp.Before(expiry) {
						expiry = helperExp
					}
				}
			}
			if noDiskCache {
				p.Notice("this Rancher does not serve ext.cattle.io; serving the credential-helper token from memory only (not cached to disk)")
			} else {
				p.Notice(fmt.Sprintf("this Rancher does not serve ext.cattle.io; caching the stored login token for %s", legacyTokenTTL))
			}
			return Token{Token: stored, ExpiresAt: expiry.UTC().Format(time.RFC3339), noDiskCache: noDiskCache}, nil
		case errors.Is(err, errDerivedCreateDenied):
			p.Notice(fmt.Sprintf("creating a token from the stored login credential for %q failed: %v; falling back to interactive login", name, err))
			return Token{}, errNoStoredCredential
		default:
			return Token{}, err
		}
	}
	return Token{Token: bearer, ExpiresAt: expiresAt}, nil
}

// caForServer returns the CA of the configured server entry whose URL matches
// server (compared by scheme+host via SameServerURL), or "" if none matches —
// an explicit --server with no --cacert never silently borrows a different
// server's trust root. When multiple entries share a host and ignoreCurrentContext
// is false, the current server wins; ties are broken by sorted entry
// name (the same determinism storedTokenForServer uses at ranks 2 and 3), so
// repeated calls with the same config always return the same CA regardless of
// map-iteration order. When ignoreCurrentContext is true the current-server
// preference is suppressed and sorted order alone determines the winner —
// consistent with how ignoreCurrentContext gates the rank-2 fallback.
func caForServer(cfg *config.Config, server string, ignoreCurrentContext bool) string {
	matches := matchingServerNames(cfg, server)
	if len(matches) == 0 {
		return ""
	}
	if !ignoreCurrentContext {
		curName, _, err := cfg.CurrentServerEntry()
		if err == nil {
			for _, n := range matches {
				if n == curName {
					return cfg.Servers[n].CA
				}
			}
		}
	}
	return cfg.Servers[matches[0]].CA
}
