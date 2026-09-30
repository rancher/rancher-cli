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
	"slices"
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
// notice - a real (non-ErrNoToken) store error. When true, the resolved-key
// re-check is quiet so the notice fires exactly once (not zero times: a clean
// miss on the flag key does NOT suppress a distinct error on the resolved key).
//
// legacyShim marks the caller as the legacy token shim - the one structural
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
// SkipVerify disables TLS verification. Only the legacy token shim sets it -
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
	GetID() string
}

// unsupportedProvidersRFE is the server change that would let the CLI log
// in with the code-exchange providers (GitHub and the like): the server
// holds their client secrets, so the CLI cannot complete the exchange.
const unsupportedProvidersRFE = "rancher/rancher#57034"

type tokenCmd struct {
	dir   string
	store config.CredStore
	// anchor is what every cache hit in this invocation is judged
	// against: the directory, the binding stamp read before the config
	// load, and that load's Config. It is built by the caller, not here,
	// so the stamp provably precedes the load; a nil anchor is a miss.
	anchor *config.CacheAnchor
	// openStore resolves the store the plugin-created login persists
	// through, from the policy read under the config lock; nil means
	// config.OpenStoreWithNotice. Tests inject a failing store.
	openStore func(policy, dir string) (config.CredStore, string, error)
	// newClient builds the client every Rancher request goes through; nil
	// means httpx.New. Tests inject a client bound to an in-memory server
	// so the whole invocation can run on a fake clock.
	newClient func(httpx.Options) (*http.Client, error)
	// newDirectClient builds the client the direct-endpoint probe uses
	// from the exec cluster's rest.Config; nil means kube.HTTPClient. The
	// probe builds its own client, so the Rancher seam cannot reach it.
	newDirectClient directClientFactory
	// resolved is the identification the connection was set up under, when
	// this call read config.yaml to choose the CA. The credential lookup
	// reads config.yaml again; if it identifies another entry the file
	// changed between the two reads, and the call stops rather than pair one
	// entry's trust with another's login.
	resolved entryChoice
}

// clientFor is newClient or its production default.
func (t tokenCmd) clientFor(opts httpx.Options) (*http.Client, error) {
	if t.newClient != nil {
		return t.newClient(opts)
	}
	return httpx.New(opts)
}

// directClientFor is newDirectClient or its production default.
func (t tokenCmd) directClientFor() directClientFactory {
	if t.newDirectClient != nil {
		return t.newDirectClient
	}
	return kube.HTTPClient
}

// NewTokenCommand builds `auth get-token`. anchor pairs the binding stamp
// read before the caller's config load with that load's Config; a nil one
// makes every cache hit a miss, which is the answer for a config.yaml the
// caller could not read.
func NewTokenCommand(dir string, store config.CredStore, anchor *config.CacheAnchor) runtime.Command {
	return &tokenCmd{dir: dir, store: store, anchor: anchor}
}

// cacheDir is the directory a record this invocation writes is bound to:
// the canonical value the anchor already resolved, so the write and every
// later read compare the same form. A command built without an anchor -
// a test, a direct construction - resolves it once here instead.
func (t tokenCmd) cacheDir() string {
	if t.anchor != nil {
		return t.anchor.Dir
	}
	return config.CanonicalDir(t.dir)
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
		// kubectl parses directly - no envelope exists to vary with -o. table
		// (the default) and json are the two forms this command has ever
		// honored; every other value is now rejected pre-Plan instead of
		// being silently ignored.
		OutputPolicy: runtime.OutputPolicy{AllowedFormats: []string{"table", "json"}},
		Shape:        runtime.ShapeExecJSON,
		// kubectl runs the plugin with the directory its exec entry names,
		// which the operator's shell does not carry.
		RemedyConfigDir: true,
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
	// reaching resolveConn's check - a token returned for a cluster the
	// caller didn't name. The pipeline's validateInput normally rejects this
	// first; this guards direct Execute callers. The shim keeps its unscoped
	// path via runWithParams, which run() never fronts for it.
	if in.String("cluster") == "" {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, "--cluster is required")
	}

	flagServer := in.String("server")
	var flagKey string
	var noticed bool
	// The flag-only fast path serves a cached credential without reading
	// its readiness record; with a cluster entry in KUBERNETES_EXEC_INFO
	// the resolved path must decide whether that endpoint still needs a
	// probe, so the fast path is skipped.
	// A malformed value takes the resolved path too, where its warning
	// is attached to whatever that path returns.
	if ec, err := loadExecCluster(); err == nil && ec == nil {
		var r runtime.Result
		var hit bool
		flagKey, noticed, r, hit = t.probeFlagCache(name, flagServer, in.String("cluster"), p)
		if hit {
			return r, nil
		}
	}

	res, err := t.runWithParams(ctx, runParams{
		Server:        flagServer,
		Name:          name,
		Cluster:       in.String("cluster"),
		AuthProvider:  in.String("auth-provider"),
		AuthFlow:      in.String("auth-flow"),
		CACert:        in.String("cacert"),
		ProbedKey:     flagKey,
		ProbedNoticed: noticed,
	}, p)
	if err != nil {
		return res, t.withStoreConflictSteps(err, flagServer, name, in.String("cluster"))
	}
	return res, nil
}

// runWithParams runs the full get-token flow (config resolution, cache,
// auth) from an already-populated runParams. tokenCmd.run calls it after the
// flag fast path; the legacy token shim calls it directly.
func (t tokenCmd) runWithParams(ctx context.Context, params runParams, p runtime.Prompter) (runtime.Result, error) {
	// Reject any --name starting with the legacy prefix unless the caller is
	// the shim (legacyShim, an unexported field only shim.go sets). This
	// guard lives here - not just in run() - so every runWithParams caller is
	// protected, not only the CLI-flag path.
	if config.IsLegacyCacheSlot(params.Name) && !params.legacyShim {
		return runtime.Result{}, clierr.New(clierr.CodeValidation,
			"--name values starting with '"+config.LegacyCachePrefix+"' are reserved for the legacy token shim's cache slots; use a different name")
	}

	// Full resolution (config + CA disk I/O): required to authenticate on a miss
	// and to build the definitive cache key when --server was not provided.
	server, caPEM, clusterID, caSource, choice, err := t.resolveConnWithCA(params.Server, params.Cluster, params.CACert, params.Name, params.legacyShim)
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

	// The endpoint kubectl is about to use, when the kubeconfig asked
	// kubectl to say: a direct endpoint gets the readiness wait, the
	// Rancher proxy needs none.
	ec, ecErr := loadExecCluster()
	var directTarget *execCluster
	if ec != nil && !isRancherProxy(ec.server, server, clusterID) {
		directTarget = ec
	}
	var loginWarnings []string
	if ecErr != nil {
		loginWarnings = append(loginWarnings, execInfoEnv+" could not be read ("+ecErr.Error()+"); not waiting for the direct endpoint")
	}

	switch params.Name {
	case "":
		p.Notice("caching disabled: no --name; each call re-authenticates")
	default:
		// Skip the re-check when the caller already probed this exact key (run's
		// flag fast path or the shim's pre-probe): probing the same key twice would
		// double any "cache read failed" notice on a real store error.
		// When the keys differ (e.g. cluster sourced from config, not from flags),
		// this is the first probe for the resolved key. quiet is set only when the
		// fast-path probe NOTICED a real store error (ProbedNoticed) - a clean miss
		// on the flag key must NOT suppress a distinct error on the resolved key,
		// which would degrade the invariant from "exactly once" to "zero notices".
		if cacheKey != params.ProbedKey {
			quiet := params.ProbedNoticed
			r, hit, err := t.cachedResultQ(ctx, cacheKey, quiet, p, directTarget, params.Name)
			if err != nil {
				return runtime.Result{}, err
			}
			if hit {
				r.Warnings = append(r.Warnings, loginWarnings...)
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

	client, err := t.clientFor(httpx.Options{CACert: caPEM, Insecure: input.skipVerify, Timeout: httpx.DefaultTimeout})
	if err != nil {
		return runtime.Result{}, err
	}

	t.resolved = choice
	rancherToken, reuseOn, binding, err := t.storedCredToken(ctx, client, server, clusterID, params.Name, params.legacyShim, p)
	// The plugin-created login exists to serve cluster-scoped tokens, so it
	// runs only for an invocation that names a cluster. The legacy shim
	// without --cluster keeps creating the unscoped kubeconfig token.
	pluginLane := clusterID != "" && reuseOn
	// An unsettled login counts as a clean miss for this one decision, and
	// only here: parallel kubectl calls made while an operator's login is
	// running must queue on the plugin login's own lock rather than each
	// fail, and the lane re-reads the credential after that lock, where the
	// error is terminal again. A credential another config directory
	// replaced is never a miss anywhere.
	if (errors.Is(err, errNoStoredCredential) || isLoginIncomplete(err)) && pluginLane {
		// A clean miss with the setting on: the plugin-created login. It
		// returns errNoStoredCredential itself when the setting was turned
		// off while it waited for the flock, which the switch below then
		// handles as today's scoped interactive login.
		var pluginWarnings []string
		rancherToken, pluginWarnings, err = t.pluginLogin(ctx, t.pluginInput(server, serverID, caPEM, clusterID, client, params), p)
		loginWarnings = append(loginWarnings, pluginWarnings...)
	}
	if rej, ok := errors.AsType[*pluginCredentialRejected](err); ok {
		if pluginLane {
			// A rejected plugin-created login: one refresh under the flock.
			in := t.pluginInput(server, serverID, caPEM, clusterID, client, params)
			in.refreshEntry = rej.name
			var pluginWarnings []string
			rancherToken, pluginWarnings, err = t.pluginLogin(ctx, in, p)
			loginWarnings = append(loginWarnings, pluginWarnings...)
		} else {
			// No cluster to scope a refreshed login to: this invocation
			// takes the interactive login the rejection always falls back to.
			err = errStoredCredentialRejected
		}
	}
	switch {
	case errors.Is(err, errNoStoredCredential), errors.Is(err, errStoredCredentialRejected):
		// Built only when a live login is needed: the cached-credential
		// path, the steady state under kubectl's exec plugin, never dials
		// an identity provider.
		input.idpClient = httpx.NewExternal(httpx.DefaultTimeout)
		var res SessionAcquireResult
		res, err = loginAndGenerateCred(ctx, client, input, p)
		if err != nil {
			// This lane is taken only when the plugin-created login is not,
			// so a sign-in here keeps no login credential.
			return runtime.Result{}, t.withNoTerminalRemedies(err, noTerminalCall{name: params.Name, clusterID: clusterID, serverURL: serverID, caCert: caSource, shim: params.legacyShim})
		}
		// The interactive login's cluster-scoped token has no parent
		// bearer: it revokes itself if the wait below is cancelled.
		rancherToken = Token{Token: res.Create.Bearer, ExpiresAt: res.Create.ExpiresAt,
			fresh:   newFreshToken(res.Create, client, kube.BaseURL(server), res.Create.Bearer),
			binding: binding}
	case err != nil:
		// The plugin-created login reaches the same sign-in prompts.
		return runtime.Result{}, t.withNoTerminalRemedies(err, noTerminalCall{name: params.Name, clusterID: clusterID, serverURL: serverID, caCert: caSource, shim: params.legacyShim, keepsLogin: pluginLane})
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

	// A token this invocation created is accepted by a direct endpoint
	// only after Rancher has synced it there; wait for that endpoint to
	// authenticate it before kubectl uses it. One wait, whichever producer
	// made the token; a cached token skips it.
	markReady := false
	if directTarget != nil && rancherToken.fresh != nil {
		ready, werr := waitDirectReady(ctx, directTarget, rancherToken.Token, directWaitBound, t.directClientFor())
		switch {
		case werr == nil && ready:
			markReady = true
		case werr == nil:
			loginWarnings = append(loginWarnings, directNotReadyWarning(directTarget.server, params.Name))
		default:
			if unavailable, ok := errors.AsType[*directProbeUnavailable](werr); ok {
				loginWarnings = append(loginWarnings, fmt.Sprintf("the direct endpoint %s cannot be probed (%v); the credential is returned without waiting for it", directTarget.server, unavailable.err))
				break
			}
			// The caller cancelled: the token that would now be orphaned
			// is revoked with the bearer that created it, detached from the
			// cancellation and bounded, and reported.
			cctx, ccancel := cleanupContext(ctx)
			out := rancherToken.fresh.cleanup(cctx)
			ccancel()
			return runtime.Result{}, directWaitCancelled(werr, rancherToken.fresh, out)
		}
	}

	if params.Name != "" {
		switch {
		case tokenExpiry.IsZero() && !params.legacyShim:
			// A zero expiry would be cached and read back as never-expiring
			// (GetCachedCred only evicts a past, non-zero expiry) - never
			// cache an immortal stale token on the native path. The legacy
			// `token` shim opts in: the legacy CLI cached these
			// indefinitely, and the read path already treats zero expiry
			// as never-expiring.
			p.Notice("not caching: the new token has no expiry")
		case rancherToken.noDiskCache:
			// A credential-helper short-lived token served through the legacy
			// fallback is memory-only by contract - never write it to disk.
			p.Notice("not caching: the credential helper serves this token from memory only")
		default:
			if t.writeCacheRecord(cacheKey, rancherToken, tokenExpiry, params.Name, serverID, clusterID, p) && markReady {
				if _, merr := t.store.MarkCachedCredReady(cacheKey, rancherToken.Token, directTarget.fingerprint); merr != nil {
					p.Notice("cache readiness write failed: " + merr.Error())
				}
			}
		}
	}

	return runtime.Result{Raw: true, Kind: "ExecCredential", Warnings: loginWarnings, Data: newExecCredential(rancherToken.Token, tokenExpiry)}, nil
}

// writeCacheRecord writes the cache record for a credential this
// invocation made, under the binding the token carries, and reports
// whether it did. Every record the plugin writes passes through here: the
// store check first, then the write, which refuses a binding whose load
// showed any entry it is bound to with its cache writes paused. A record
// that is not written is a notice, never an error, except for a call its
// lookup refuses, which writes nothing and says nothing; the credential is
// returned either way.
func (t tokenCmd) writeCacheRecord(key string, tok Token, expiry time.Time, name, serverID, clusterID string, p runtime.Prompter) bool {
	b := tok.binding
	if b.Refused() {
		// The call's lookup refuses the call as the directory now stands,
		// so nothing is cached; the next call's refusal names the remedy.
		return false
	}
	if err := b.CheckStore(); err != nil {
		cause := err.Error()
		if changed, ok := errors.AsType[*config.CacheStoreChangedError](err); ok {
			cause = changed.Cause
		}
		p.Notice(fmt.Sprintf("not caching: the credential store for %s changed during this call (%s)", recordSubject(b, serverID), cause))
		return false
	}
	err := t.store.SetCachedCred(key, tok.Token, expiry, name, serverID, clusterID, b)
	if err == nil {
		return true
	}
	if paused, ok := errors.AsType[*config.CacheWritesPausedError](err); ok {
		t.resumeCacheWrites(paused.Entry, p)
		return false
	}
	p.Notice("cache write failed: " + err.Error())
	return false
}

// resumeCacheWrites is what a miss does when its record was refused for
// a pause of entry, one of the entries the record is bound to: it tries the
// config lock once, and holding it resumes every pause a resumer may
// clear, so the next call caches. This call's own record is not written
// either way, because its credential was read before the lock was taken.
// The skipped write is a notice only when entry's pause stays; a resume
// write that failed adds nothing to it, and the next miss tries again.
func (t tokenCmd) resumeCacheWrites(entry string, p runtime.Prompter) {
	res, err := config.TryResumeCacheWrites(t.dir, entry)
	if err == nil && res.Tried && res.Stays == "" {
		return
	}
	msg := fmt.Sprintf("not caching: a credential change for %s has not finished", entry)
	if err == nil && res.Tried {
		// With the lock taken, a pause that stays is one only another
		// command or the operator can clear, and config status names what.
		msg += " (see `" + config.StatusCommand + "`)"
	}
	p.Notice(msg)
}

// recordSubject names what a skipped record would have been for: the
// entry the call identified, or the server for a call that identifies
// none.
func recordSubject(b config.CacheBinding, serverID string) string {
	if b.Entry() != "" {
		return b.Entry()
	}
	return serverID
}

// cachedResultQ returns an ExecCredential result for a warm cache entry and
// whether it hit. When quiet is true a real
// store error is silently treated as a miss rather than noticed. Used when the
// caller's fast-path probe already noticed the error for this invocation so the
// resolved-key re-check does not fire the notice a second time. With a
// direct target whose fingerprint the record does not carry, the cached
// token is re-probed there first (a shorter bound than a fresh token's) and
// the record marked on success; a mark refused because the record changed
// underneath is a miss, and a cancelled re-probe is the error.
func (t tokenCmd) cachedResultQ(ctx context.Context, key string, quiet bool, p runtime.Prompter, direct *execCluster, entry string) (runtime.Result, bool, error) {
	return config.CachedHit(t.anchor, func(a *config.CacheAnchor) (runtime.Result, bool, error) {
		return t.cachedResultOnce(ctx, key, quiet, p, direct, entry, a)
	})
}

// cachedResultOnce is one pass of the warm-hit path: the record read, the
// readiness re-probe and the mark. The binding-stamp confirmation that
// follows it is the caller's, so it lands after the re-probe rather than
// before - a login that completed while this waited on an endpoint must
// still turn the hit into a miss.
func (t tokenCmd) cachedResultOnce(ctx context.Context, key string, quiet bool, p runtime.Prompter, direct *execCluster, entry string, anchor *config.CacheAnchor) (runtime.Result, bool, error) {
	d, err := t.store.GetCachedCredDetail(key, anchor)
	if err != nil {
		if !errors.Is(err, config.ErrNoToken) && !quiet {
			p.Notice("cache read failed: " + err.Error())
		}
		return runtime.Result{}, false, nil
	}
	r := runtime.Result{Raw: true, Kind: "ExecCredential", Data: newExecCredential(d.Token, d.Expiry)}
	if direct == nil || slices.Contains(d.ReadyEndpoints, direct.fingerprint) {
		return r, true, nil
	}
	ready, werr := waitDirectReady(ctx, direct, d.Token, directRecheckBound, t.directClientFor())
	switch {
	case werr == nil && ready:
		marked, merr := t.store.MarkCachedCredReady(key, d.Token, direct.fingerprint)
		if merr != nil {
			p.Notice("cache readiness write failed: " + merr.Error())
		}
		if merr == nil && !marked {
			// Another process replaced or deleted the record while the
			// endpoint was probed: the token just read is stale and is
			// never handed to kubectl; this is a miss.
			return runtime.Result{}, false, nil
		}
	case werr == nil:
		r.Warnings = append(r.Warnings, directNotReadyWarning(direct.server, entry))
	default:
		if unavailable, ok := errors.AsType[*directProbeUnavailable](werr); ok {
			r.Warnings = append(r.Warnings, fmt.Sprintf("the direct endpoint %s cannot be probed (%v); the credential is returned without waiting for it", direct.server, unavailable.err))
			break
		}
		return runtime.Result{}, false, werr
	}
	return r, true, nil
}

// directNotReadyWarning names the endpoint that has not accepted the
// credential within the bound and the proxy context to fall back to: the
// kubeconfig's exec user entry name, which is also its proxy context.
func directNotReadyWarning(server, entry string) string {
	fallback := "the proxy context of this kubeconfig"
	if entry != "" {
		fallback = "the proxy context " + entry
	}
	return fmt.Sprintf("the new credential may not yet be accepted at %s; %s is the immediate fallback", server, fallback)
}

// computeCacheProbeKey validates and normalizes name, server, and cluster into
// the cache-key string that both probeFlagCache and shimProbeFlagCache use.
// Keeping this logic here means future scheme, normalization, or CacheKey-shape
// changes need only be applied once - the "fast-path probed key == slow-path
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
// given - the same fast-path logic run uses, factored out so the legacy token
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
		type read struct {
			token  string
			expiry time.Time
		}
		got, ok, err := config.CachedHit(t.anchor, func(a *config.CacheAnchor) (read, bool, error) {
			tok, expiry, gerr := t.store.GetCachedCred(k, a)
			if gerr != nil {
				if errors.Is(gerr, config.ErrNoToken) {
					return read{}, false, nil
				}
				return read{}, false, gerr
			}
			return read{token: tok, expiry: expiry}, true, nil
		})
		if err != nil {
			return "", time.Time{}, "", err
		}
		if !ok {
			return "", time.Time{}, "", config.ErrNoToken
		}
		return got.token, got.expiry, "", nil
	}, p)
	return key, noticed, r, hit
}

// probeCacheWith is the one flag-only cache probe: compute the key via
// computeCacheProbeKey, read through read, and classify the outcome. Shared
// by probeFlagCache (store-backed) and shimProbeFlagCache (direct read-only
// file probe) so hit construction and the notice-once/notice-zero
// classification - noticed is true only for a real, non-ErrNoToken error -
// cannot drift between the two paths. warn is a notice read returned
// alongside the token (a loose-permission finding on the shim's direct file
// read, say) - fired independently of hit/miss, since read may notice
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
	// sharing the name are the same logical credential - deleting cached tokens
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
// read-only cache scan. Composite keys are not offered.
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
// is the first consumer.
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
	authProviders, unsupported, err := getAuthProviders(ctx, client, input.server)
	if err != nil {
		return SessionAcquireResult{}, err
	}
	selectedProvider, err := selectAuthProvider(ctx, authProviders, unsupported, input.authProvider, p)
	if err != nil {
		return SessionAcquireResult{}, err
	}
	input.authProvider = selectedProvider.GetType()

	if samlProviders[input.authProvider] {
		if p.NoInput() {
			return SessionAcquireResult{}, interactiveLoginRequired(input.authProvider, "run without --no-input, or use a cached --name")
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
			return notAttempted, interactiveLoginRequired(input.authProvider, "run in a terminal, or use a cached --name")
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
			return notAttempted, interactiveLoginRequired(input.authProvider, "run in a terminal, or use a cached --name")
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

// samlAuth signs in through the browser: it publishes a URL carrying a request
// id and a public key, then polls Rancher's authtokens collection for the
// encrypted token the browser deposits there. Nothing here creates a token -
// the server does, out of band - so every failure after the URL is published
// leaves a session token that may exist and can only be found through the SAML
// handoff this returns. The created token is always v3 and never carries a
// description, so it can never be marked.
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

	// The SAML handoff is recorded before the URL reaches the operator: from
	// the moment it can be opened, a token may exist, and every exit below must
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

// getAuthProviders lists the server's auth providers: the ones this CLI can
// log in with (local first), and the types the server advertises that it
// cannot, so a caller can name them instead of silently dropping them.
func getAuthProviders(ctx context.Context, client *http.Client, server string) ([]typedProvider, []string, error) {
	authProvidersURL := fmt.Sprintf(authProviderURL, server)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authProvidersURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating request: %w", err)
	}

	resp, respBody, err := httpx.Do(client, req)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("error listing auth providers: %w", err)
	}

	if !gjson.ValidBytes(respBody) {
		return nil, nil, errors.New("invalid JSON response")
	}
	data := gjson.GetBytes(respBody, "data").Array()

	var supportedProviders []typedProvider
	var unsupported []string
	for _, provider := range data {
		providerType := provider.Get("type").String()
		if providerType != "" && !supportedAuthProviders[providerType] {
			unsupported = append(unsupported, providerType)
		}

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
				return nil, nil, fmt.Errorf("error decoding the auth provider %s: %w", providerType, err)
			}

			if tp.GetType() == "localProvider" {
				supportedProviders = append([]typedProvider{tp}, supportedProviders...)
			} else {
				supportedProviders = append(supportedProviders, tp)
			}
		}
	}

	slices.Sort(unsupported)
	return supportedProviders, slices.Compact(unsupported), nil
}

// unsupportedNote names the providers the server advertises that this CLI
// cannot log in with; "" when there are none.
func unsupportedNote(unsupported []string) string {
	if len(unsupported) == 0 {
		return ""
	}
	return "advertised by the server but not supported by this CLI: " + strings.Join(unsupported, ", ") +
		" (server change requested: " + unsupportedProvidersRFE + ")"
}

// selectAuthProvider picks the provider to log in with. want matches a
// provider's type (localProvider) or its id (local). A miss names the
// supported providers as candidates and, in the message, the advertised
// ones this CLI cannot use, so an operator can tell "not configured on the
// server" from "not supported here". The same note precedes the prompt,
// also when only one provider is left to prompt for: a user who came for
// GitHub must not type those credentials into the local password prompt.
func selectAuthProvider(ctx context.Context, providers []typedProvider, unsupported []string, want string, p runtime.Prompter) (typedProvider, error) {
	note := unsupportedNote(unsupported)
	if len(providers) == 0 {
		msg := "no auth provider configured"
		if note != "" {
			msg += "; " + note
		}
		return nil, clierr.New(clierr.CodeGeneric, msg)
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
			if prov.GetType() == want || (prov.GetID() != "" && prov.GetID() == want) {
				return prov, nil
			}
		}
		msg := "auth provider not found: " + want
		if note != "" {
			msg += "; " + note
		}
		return nil, clierr.New(clierr.CodeNotFound, msg).WithCandidates(cands())
	}

	if len(providers) == 1 {
		if note != "" && p.CanPrompt() {
			p.Notice("Auth provider: " + providers[0].GetType() + "\n" + note)
		}
		return providers[0], nil
	}

	if p.CanPrompt() {
		var b strings.Builder
		for i, prov := range providers {
			fmt.Fprintf(&b, "%d - %s\n", i, prov.GetType())
		}
		if note != "" {
			b.WriteString(note + "\n")
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

	msg := "multiple auth providers; select one"
	if note != "" {
		msg += "; " + note
	}
	return nil, clierr.New(clierr.CodeAmbiguousSelection, msg).
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
// --server with no --cacert never inherits the CURRENT context's CA - that CA
// belongs to whatever server the context happens to point at, which may differ
// from the server just named; instead it borrows the CA of the configured
// server entry whose URL actually matches (see caForServer).
// When ignoreCurrentContext is true caForServer's current-server CA preference is
// suppressed: an empty --cluster stays unscoped, and among same-host CA
// candidates sorted order wins instead of the current-server entry - driven
// purely by the flags, never by stored current-context state.
// resolveConn accepts the three flag values that drive connection resolution
// as plain strings rather than a runtime.Input, so callers (runWithParams)
// can pass runParams fields directly without an adapter wrapper.
func (t tokenCmd) resolveConn(flagServer, flagCluster, caIn string, ignoreCurrentContext bool) (server, caPEM, cluster string, err error) {
	server, caPEM, cluster, _, _, err = t.resolveConnWithCA(flagServer, flagCluster, caIn, "", ignoreCurrentContext)
	return server, caPEM, cluster, err
}

// resolveConnWithCA is resolveConn that also returns the CA as it was
// chosen - the --cacert value, or else the CA of the entry wantName
// identifies, or else of the matching entry, from config.yaml, a path or an
// inline PEM - before it is read, and the identification the CA was chosen
// under when config.yaml was read to make it.
func (t tokenCmd) resolveConnWithCA(flagServer, flagCluster, caIn, wantName string, ignoreCurrentContext bool) (server, caPEM, cluster, caSource string, choice entryChoice, err error) {
	server = flagServer
	cluster = flagCluster
	explicitServer := server != ""
	// Normalize the flag value's scheme here, once: it must happen before
	// caForServer below, and a config-sourced server (from eff.URL) is always
	// already scheme-qualified (login rejects a schemeless --server), so
	// there is no second site left needing this. Guarded on explicitServer -
	// an empty server must stay "" for the fallback-to-config check below,
	// not become the bogus non-empty "https://".
	if explicitServer {
		server = applyDefaultScheme(server)
	}

	// A ignoreCurrentContext caller is flag-driven by definition: an absent
	// --server must fail here, never be filled from the config's current
	// context below - past this point server == "" implies !ignoreCurrentContext.
	if server == "" && ignoreCurrentContext {
		return "", "", "", "", entryChoice{}, clierr.New(clierr.CodeValidation, "--server is required")
	}

	// The native get-token path requires --cluster: a token must never be created
	// for a cluster the caller didn't name. Shim calls (ignoreCurrentContext) keep
	// the legacy semantic where an empty --cluster is a deliberate unscoped
	// create - still purely flag-driven, never filled from stored state.
	if cluster == "" && !ignoreCurrentContext {
		return "", "", "", "", entryChoice{}, clierr.New(clierr.CodeValidation, "--cluster is required")
	}

	if server == "" || caIn == "" {
		cfg, cerr := config.Load(config.FilePath(t.dir))
		if cerr != nil {
			return "", "", "", "", entryChoice{}, cerr
		}
		// The current server's URL and CA come from EffectiveConnection, which
		// reads no credential: this call needs where to connect and whom to
		// trust, never the current entry's login, and a store failure on an
		// entry it does not use must not abort it.
		eff := cfg.EffectiveConnection()
		if server == "" {
			server = eff.URL
		}
		if caIn == "" && server != "" {
			identified, ierr := identifyEntry(cfg, server, wantName, cluster, ignoreCurrentContext)
			if ierr != nil {
				return "", "", "", "", entryChoice{}, ierr
			}
			choice = entryChoice{known: true, name: identified}
			if identified != "" || explicitServer {
				caIn = caForServer(cfg, server, identified, ignoreCurrentContext)
			} else {
				caIn = eff.CA
			}
		}
	}

	if server == "" {
		return "", "", "", "", entryChoice{}, clierr.New(clierr.CodeValidation,
			"--server is required; pass it, or add and log in to a server first")
	}
	caPEM, err = config.ResolveCA(caIn)
	if err != nil {
		return "", "", "", "", entryChoice{}, clierr.New(clierr.CodeValidation, "reading --cacert: "+err.Error())
	}
	return server, caPEM, cluster, caIn, choice, nil
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
// proxy path on the LOCAL cluster's apiserver - ext.cattle.io is an
// aggregated/CRD API, never served at the server root, so a bare /apis/...
// 404s on every Rancher.
const ExtTokensPath = "/k8s/clusters/local/apis/ext.cattle.io/v1/tokens"

// ErrExtGroupAbsent signals a 404 on the derived token create call itself - the aggregated
// ext.cattle.io API isn't served (an older Rancher), not that some other
// resource is missing.
var ErrExtGroupAbsent = errors.New("ext.cattle.io not served")

// errCredentialUnauthorized and errCredentialForbidden wrap the two
// rejections a request made with a stored credential can receive, as
// opposed to a network/TLS failure or a server error. They are separate
// because they need different handling: a 401 says the credential itself is
// no longer valid, a 403 says the credential is valid but the permission is
// missing, which no re-authentication fixes.
var (
	errCredentialUnauthorized = errors.New("credential rejected: 401 Unauthorized")
	errCredentialForbidden    = errors.New("credential lacks permission to create tokens: 403 Forbidden")
)

// CreateDerivedToken asks the ext Token store to create a purpose-made derived
// token (bearer bearerToken), scoped to clusterID when set (spec.clusterName,
// the same field internal/engine/extres/create.go's `create token --cluster`
// uses), carrying description as spec.description. It returns what the create
// left behind - the resource name, bearer, and expiry when the response
// identified them - on the error path too, so a caller that must clean up
// knows what exists. ErrExtGroupAbsent signals the group-level 404 that means
// an older Rancher; errCredentialUnauthorized and errCredentialForbidden
// signal a 401 and a 403 (the bearer credential is bad, or lacks the
// permission), distinct from any other error (network/TLS/5xx), which
// callers must treat as a real failure, not a "try something else" signal. ttlMs <= 0 omits
// spec.ttl entirely (server default). This is the SAME derived-token path
// `auth login` uses (via resolveProviderLoginToken in package auth), so
// get-token's stored-credential path creates exactly the same shape of token.
func CreateDerivedToken(ctx context.Context, client *http.Client, base, bearerToken, clusterID string, ttlMs int64, description string) (CreateResult, error) {
	spec := map[string]any{
		"description": description,
	}
	if ttlMs > 0 {
		spec["ttl"] = ttlMs
	}
	if clusterID != "" {
		spec["clusterName"] = clusterID
	}
	r, resp, err := postTokenCreate(ctx, client, base+ExtTokensPath, bearerToken, map[string]any{"spec": spec}, TokenKindExt, description, "creating derived token")
	if err != nil {
		return r, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return r, ErrExtGroupAbsent
	}
	if r.Bearer == "" {
		return r, clierr.New(clierr.CodeGeneric, "token create response contained no bearer token")
	}
	return r, nil
}

// postTokenCreate POSTs one token create and classifies what it left
// behind. verb names the operation in error text ("creating derived
// token"). Every failure the two create routes answer the same way is
// returned here: a transport failure (cause kept, so a cancelled operation
// stays recognizable as cancellation after cleanup wraps it), a 401 or a
// 403 on the bearer, and any status other than 200, 201, and 404. The
// caller is left with 404 and success, which mean different things on each
// route.
func postTokenCreate(ctx context.Context, client *http.Client, url, bearer string, body map[string]any, kind TokenKind, description, verb string) (CreateResult, *http.Response, error) {
	notAttempted := CreateResult{Outcome: CreateNotAttempted, Kind: kind, Marked: description != ""}
	raw, err := json.Marshal(body)
	if err != nil {
		return notAttempted, nil, fmt.Errorf("building the token create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return notAttempted, nil, fmt.Errorf("building the token create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)

	createdAt := time.Now().UTC().Format(time.RFC3339)
	resp, respBody, err := httpx.Do(client, req)
	r := classifyCreate(resp, respBody, err, createdAt, description != "", kind)
	if err != nil {
		return r, nil, clierr.New(clierr.CodeGeneric, verb+": "+err.Error()).WithCause(err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNotFound:
		return r, resp, nil
	case http.StatusUnauthorized:
		return r, resp, fmt.Errorf("%w: %d %s", errCredentialUnauthorized, resp.StatusCode, http.StatusText(resp.StatusCode))
	case http.StatusForbidden:
		return r, resp, fmt.Errorf("%w: %d %s", errCredentialForbidden, resp.StatusCode, http.StatusText(resp.StatusCode))
	default:
		return r, resp, clierr.New(clierr.CodeGeneric,
			fmt.Sprintf("%s: %d %s", verb, resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
}

// hostLoginDescription is the description a token create carries when no
// login operation owns it: get-token's per-cluster creates, which no cleanup
// path has to find again.
func hostLoginDescription() string {
	return "rancher-cli login @ " + hostname()
}

// hostname is this machine's name for a token description; a machine that
// cannot name itself is "unknown".
func hostname() string {
	host, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return host
}

// matchingServerNames returns the names of the cfg.Servers entries whose URL
// matches server (compared by scheme+host via config.SameServerURL), sorted
// for determinism. caForServer and storedTokenForServer both select from this
// one candidate set, so a change to the matching policy cannot apply to one
// and silently diverge in the other.
func matchingServerNames(cfg *config.Config, server string) []string {
	return cfg.MatchingServerNames(server)
}

// errNoStoredCredential is storedCredToken's clean-miss sentinel: no
// eligible stored credential exists at all. Any other error
// storedCredToken returns is a real
// failure (network/TLS/5xx, or a resolution error for --cluster) that the
// caller must surface directly, never paper over with "interactive login
// required".
var errNoStoredCredential = errors.New("no usable stored login credential")

// errStoredCredentialRejected is a 401 on a user's stored credential, from
// the cluster list or the create: the caller runs the scoped interactive
// login for this invocation and never a plugin login.
var errStoredCredentialRejected = errors.New("the stored login credential was rejected")

// pluginCredentialRejected is a 401 on a plugin-created login: the caller
// takes the plugin-login flock and refreshes, at most one prompt.
type pluginCredentialRejected struct {
	name string
}

func (e *pluginCredentialRejected) Error() string {
	return "the plugin-created login " + e.name + " was rejected by the server"
}

// storedCredential is one stored login credential with the entry it came
// from, so the caller can apply provenance-specific rules.
type storedCredential struct {
	name  string
	token string
	ref   config.ServerRef
	entry config.Server
}

// provenanceTier orders candidates before any other rank: every user entry
// (any provenance but execLogin, including pre-provenance entries) ranks
// ahead of every plugin login entry.
func provenanceTier(s config.Server) int {
	if s.Provenance == clierr.ProvenanceExecLogin {
		return 1
	}
	return 0
}

// The ranker orders an entry by its provenance tier first and its place
// among the tier's entries second. candidateRankCount is the number of
// distinct ranks that gives, derived from both counts so a caller can scan
// every rank without a second number drifting out of sync with the ranker:
// a third provenance tier raises candidateTierCount, and the scan follows.
const (
	candidateTierCount = 2
	candidatePlaces    = 2
	candidateRankCount = candidateTierCount * candidatePlaces
)

// candidateRanker orders the entries that answer for one server URL when
// the call names none of them: lower is preferred. The provenance tier
// comes first, then the current server ahead of every other entry. One
// ranker for both callers, for the reason MatchingServerNames gives for
// the candidate set itself - the entry a cache record is bound to and the
// entry a credential is read from must be the same entry, and a
// tie-break changed at one site and not the other would bind a record to
// an entry the credential lookup never chooses, so every record for that
// server becomes a miss.
// ignoreCurrentContext drops the current-server preference: the shim is
// driven by its flags alone, never by stored current-context state.
func candidateRanker(cfg *config.Config, ignoreCurrentContext bool) func(string) int {
	curServer := ""
	if !ignoreCurrentContext {
		curServer = cfg.CurrentServer
	}
	return func(n string) int {
		rank := provenanceTier(cfg.Servers[n]) * candidatePlaces
		if curServer == "" || n != curServer {
			rank++
		}
		return rank
	}
}

// isLoginIncomplete reports whether err says a login for the entry wrote
// its identity and has not settled it. It is the one identity verdict the
// plugin's lane decision treats as a clean miss; every other one, that
// code included, is terminal wherever it is met.
func isLoginIncomplete(err error) bool {
	ce, ok := errors.AsType[*clierr.Error](err)
	return ok && ce.Code == clierr.CodeLoginIncomplete
}

// storedTokenForServer finds the stored login credential the call may use
// for server. identified is the entry identifyEntry chose; when it is set,
// only that entry's credential is read, with the plugin's own login entry
// as the one fallback while token reuse is on (see identifiedCredential).
// When identified is empty the call named no entry, and the entries that
// answer for the URL are ranked by candidateRanker; more than one
// credentialed entry at the winning rank is an error, never a pick.
// A plugin login entry is a candidate only while the token-reuse setting is
// on. ignoreCurrentContext is the shim's flag-driven mode: no
// current-server preference, and the error's remedy is the shim's.
func storedTokenForServer(cfg *config.Config, store CredentialReader, server, identified, clusterID string, ignoreCurrentContext bool) (storedCredential, bool, error) {
	if identified != "" {
		return identifiedCredential(cfg, store, server, identified)
	}
	return rankedCredential(cfg, store, server, clusterID, ignoreCurrentContext)
}

// rankedCredential is the lookup for a call that named no entry.
func rankedCredential(cfg *config.Config, store CredentialReader, server, clusterID string, ignoreCurrentContext bool) (storedCredential, bool, error) {
	candidates := cfg.MatchingServerNames(server)
	if !cfg.TokenReuse {
		candidates = slices.DeleteFunc(candidates, func(n string) bool {
			return cfg.Servers[n].Provenance == clierr.ProvenanceExecLogin
		})
	}
	if len(candidates) == 0 {
		return storedCredential{}, false, nil
	}

	rankOf := candidateRanker(cfg, ignoreCurrentContext)
	sort.SliceStable(candidates, func(i, j int) bool {
		return rankOf(candidates[i]) < rankOf(candidates[j])
	})

	hitsByRank := map[int][]storedCredential{}
	// firstHardErr remembers the first non-not-found helper/store failure - a
	// missing helper binary, timeout, malformed response, version mismatch,
	// backend outage - and hardErrRank the rank of the entry it came from.
	// Candidates are read in rank order, so the first is the lowest. A
	// genuinely misconfigured store is surfaced rather than silently treated
	// as "no credential" and papered over by the interactive fallback. A
	// clean miss (ErrNoToken / empty token) is skipped.
	var (
		firstHardErr error
		hardErrRank  int
	)
	for _, n := range candidates {
		cred, ok, gerr := readEntryCredential(cfg, store, n)
		if gerr != nil {
			// Anything the identity layer says about this entry ends the
			// search instead of moving on: a credential another directory
			// replaced, a login that did not settle, and a record this
			// directory can no longer read are all "who this entry is
			// cannot be established". The remaining candidates are other
			// entries that merely share the URL, so using one of them
			// would authenticate as that entry's user and hide the
			// message. A store that could not be reached says nothing about
			// identity, but it leaves the winner unknown: the entry may
			// hold a credential at or above the rank of the one that
			// answers, so the choice below is made only when no failed
			// read could change it.
			if config.IsCredentialIdentityError(gerr) {
				return storedCredential{}, false, gerr
			}
			if firstHardErr == nil {
				firstHardErr, hardErrRank = gerr, rankOf(n)
			}
			continue
		}
		if ok {
			hitsByRank[rankOf(n)] = append(hitsByRank[rankOf(n)], cred)
		}
	}
	for rank := range candidateRankCount {
		hs := hitsByRank[rank]
		if len(hs) == 0 {
			continue
		}
		if firstHardErr != nil && hardErrRank <= rank {
			return storedCredential{}, false, firstHardErr
		}
		if len(hs) > 1 {
			return storedCredential{}, false, entryNotNamedError(cfg, hs, clusterID, ignoreCurrentContext)
		}
		return hs[0], true, nil
	}
	// No candidate produced a token. If any candidate failed with a hard
	// error, surface it - a helper misconfiguration must not silently fall
	// through to interactive auth.
	return storedCredential{}, false, firstHardErr
}

// clustersPath is the management Cluster collection/object path, reached
// through the same local-cluster proxy ExtTokensPath uses.
const clustersPath = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"

// resolveClusterIDForCreate resolves clusterID (an id, a display name, "", or
// "local") to its Cluster id before creating, using the stored login
// credential as bearer. It shares projects.MatchOneOrError - the same
// collision-and-narrowing helper get/patch's ResolveCluster and the
// cloud-credential resolver use - so an id match never short-circuits
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
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", fmt.Errorf("resolving --cluster: %w", errCredentialUnauthorized)
	case http.StatusForbidden:
		return "", fmt.Errorf("resolving --cluster: %w", errCredentialForbidden)
	default:
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
// credential for server, non-interactively: a user credential takes the
// same derived-token path `auth login` uses, a plugin-created one takes
// the fail-closed cluster-token chain. errNoStoredCredential means no eligible stored
// credential was found; errStoredCredentialRejected and
// *pluginCredentialRejected are the two 401 outcomes, and a 403 is an RBAC
// error; see classifyRejection. An unreadable config.yaml is noticed
// and also falls through to errNoStoredCredential (the config load failure
// itself is never returned as a hard error here - see the doc on the -
// no-input leftovers in the package's fix history). Any other non-nil error
// is a real failure (network/TLS/5xx, or an unresolvable --cluster) that
// the caller must surface directly.
// wantName is the exec entry's --name; identifyEntry decides which server
// entry, if any, it means, and everything below reads that entry alone.
// ignoreCurrentContext is the shim's flag-driven mode - see
// runParams.legacyShim.
// The second result is the token-reuse setting as this call read it, so the
// caller decides the plugin lane without loading config.yaml again; a config
// it could not read reads as off.
// storedCredToken also reports the cache binding a record written on this
// invocation belongs to, from the same config load it made here: the
// identified entry alone when its own credential answered, and otherwise
// what the lookup for this call reads (config.CacheBindingForLookup),
// which also binds the record of an interactive sign-in that follows.
func (t tokenCmd) storedCredToken(ctx context.Context, client *http.Client, server, clusterID, wantName string, ignoreCurrentContext bool, p runtime.Prompter) (Token, bool, config.CacheBinding, error) {
	cfg, err := config.Load(config.FilePath(t.dir))
	if err != nil {
		p.Notice(fmt.Sprintf("config unreadable: %v - skipping token creation from the stored credential", err))
		return Token{}, false, config.CacheBinding{}, errNoStoredCredential
	}
	lookup := lookupStoredLogin(cfg, t.store, server, wantName, clusterID, ignoreCurrentContext, t.resolved)
	if lookup.identifiedErr {
		return Token{}, cfg.TokenReuse, config.CacheBinding{}, lookup.Err
	}
	identified := lookup.Identified
	// Every record this call leads to is made after a read of t.store,
	// the stored credential's or the interactive fallback's because the
	// store held nothing, so each binding names that store; which backend
	// it read is asked at the write, once the read has fixed it. The
	// binding is built from this load alone, so building it after the read
	// changes nothing.
	binding := config.CacheBindingForLookup(t.cacheDir(), cfg, server, identified, "", ignoreCurrentContext).ReadFrom(t.store)
	switch lookup.Outcome {
	case LookupRefused, LookupReadFailed:
		// A hard store/helper failure (missing binary, timeout, malformed
		// response, version mismatch, backend outage) must surface, never be
		// papered over by falling through to interactive auth.
		return Token{}, cfg.TokenReuse, binding, lookup.Err
	case LookupNone:
		return Token{}, cfg.TokenReuse, binding, errNoStoredCredential
	}
	sc := lookup.cred
	// The entry that answered decides the binding: the identified entry's
	// own credential binds the record to that entry alone, and so does a
	// sign-in after the server rejects it.
	binding = config.CacheBindingForLookup(t.cacheDir(), cfg, server, identified, sc.name, ignoreCurrentContext).ReadFrom(t.store)
	stored, ref := sc.token, sc.ref
	base := kube.BaseURL(server)
	resolvedCluster, rerr := resolveClusterIDForCreate(ctx, client, base, stored, clusterID)
	if rerr != nil {
		return Token{}, cfg.TokenReuse, binding, t.classifyRejection(rerr, sc, p)
	}
	isPlugin := sc.entry.Provenance == clierr.ProvenanceExecLogin
	var created CreateResult
	if isPlugin {
		created, err = createClusterToken(ctx, client, base, stored, resolvedCluster, hostLoginDescription())
	} else {
		created, err = CreateDerivedToken(ctx, client, base, stored, resolvedCluster, 0, hostLoginDescription())
	}
	bearer, expiresAt := created.Bearer, created.ExpiresAt
	if err != nil {
		switch {
		case errors.Is(err, ErrExtGroupAbsent) && !isPlugin:
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
			return Token{Token: stored, ExpiresAt: expiry.UTC().Format(time.RFC3339), noDiskCache: noDiskCache, binding: binding}, cfg.TokenReuse, binding, nil
		case errors.Is(err, errCredentialUnauthorized), errors.Is(err, errCredentialForbidden):
			return Token{}, cfg.TokenReuse, binding, t.classifyRejection(err, sc, p)
		default:
			return Token{}, cfg.TokenReuse, binding, err
		}
	}
	// The child token is CLI-created even when the stored parent is not:
	// its cleanup deletes it with the parent bearer whatever the parent's
	// provenance.
	return Token{Token: bearer, ExpiresAt: expiresAt, fresh: newFreshToken(created, client, base, stored), binding: binding}, cfg.TokenReuse, binding, nil
}

// classifyRejection maps a 401 or 403 from any credential-authenticated
// request before or at the create onto the provenance-specific rule. A
// 403 authenticates but cannot create tokens: the permission error is
// surfaced and never prompts, because a new login yields the same RBAC.
func (t tokenCmd) classifyRejection(err error, sc storedCredential, p runtime.Prompter) error {
	switch {
	case errors.Is(err, errCredentialForbidden):
		return clierr.New(clierr.CodeRBAC,
			fmt.Sprintf("the stored login credential for %q cannot create tokens: %v", sc.name, err)).WithCause(err)
	case errors.Is(err, errCredentialUnauthorized) && sc.entry.Provenance == clierr.ProvenanceExecLogin:
		return &pluginCredentialRejected{name: sc.name}
	case errors.Is(err, errCredentialUnauthorized):
		p.Notice(fmt.Sprintf("the stored login credential for %q was rejected (%v); falling back to interactive login; refresh it with: "+clierr.Rancher+" auth login %s", sc.name, err, clierr.ShellQuote(sc.name)))
		return errStoredCredentialRejected
	}
	return err
}

// caForServer returns the CA of the configured server entry the call
// means: the identified entry's, when identifyEntry found one, even if it
// has none. Otherwise the CA of an entry whose URL matches server (compared
// by scheme+host via SameServerURL), or "" if none matches - an explicit
// --server with no --cacert never silently borrows a different server's
// trust root. Among several matching entries and no identified one, the
// current server wins unless ignoreCurrentContext, and the first by sorted
// entry name otherwise, so repeated calls with the same config always
// return the same CA regardless of map-iteration order.
func caForServer(cfg *config.Config, server, identified string, ignoreCurrentContext bool) string {
	if identified != "" {
		return cfg.Servers[identified].CA
	}
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
