package cli

import (
	"os"

	"github.com/rancher/rancher-cli/credhelper"
	"github.com/rancher/rancher-cli/internal/config"
)

// TokenCapability describes the resolved token and its scope for a
// completion request.
type TokenCapability struct {
	// Token is the bearer token to use for HTTP requests (may be empty when
	// no usable source was found).
	Token string
	// IDOnly reports that the token is exec-credential-only: cluster-dependent
	// completion sites must accept only cluster IDs, never display names
	// (a downstream-scoped token cannot resolve a display name via the
	// management cluster list).
	IDOnly bool
	// ServerURL is the Rancher server URL (for HTTP client construction).
	ServerURL string
	// CACert is the PEM CA bundle (empty = system roots).
	CACert string
}

// completionConnection is the read-only resolved connection for a TAB request.
// It mirrors Config.Effective precedence without touching the
// credential store. Scope (cluster/project) is not connection state - it
// comes from the typed arg tail alone, extracted at the dispatch layer.
type completionConnection struct {
	ServerURL string
	CACert    string
}

// resolveCompletionConnection resolves server URL and CA in strict read-only
// precedence, mirroring Config.Effective:
//
//  1. Config current server (base)
//  2. RANCHER_CLI_URL, RANCHER_CLI_CACERT env overrides
//
// Never touches the credential store or keyring.
func resolveCompletionConnection() completionConnection {
	cfg := storedConfigSnapshot()
	var conn completionConnection

	// Base from config file.
	if cfg != nil {
		if _, srv, err := cfg.CurrentServerEntry(); err == nil {
			conn.ServerURL = srv.URL
			conn.CACert = srv.CA
		}
	}

	// Env overrides (RANCHER_CLI_URL clears mismatched CA per Config.Effective semantics).
	if v := os.Getenv(config.EnvURL); v != "" {
		if !config.SameServerURL(v, conn.ServerURL) {
			conn.CACert = ""
		}
		conn.ServerURL = v
	}
	if v := config.EnvCACertFor(conn.ServerURL); v != "" {
		conn.CACert = v
	}

	// Resolve CA PEM from file path or inline PEM.
	if conn.CACert != "" {
		if pem, err := config.ResolveCA(conn.CACert); err == nil {
			conn.CACert = pem
		}
	}

	return conn
}

// completionServerURL validates a server URL before any request or stored
// credential read of a completion: it returns the canonical form, and false
// for a URL that carries userinfo, holds an @ after the host or does not
// parse (treated as a missing URL) or one with a scheme other than http and
// https. Completion is best-effort, so the refusal is silent.
func completionServerURL(raw string) (string, bool) {
	canonical, err := credhelper.CheckServerURL(raw)
	if err != nil {
		completionDebugf("stop: server URL: %v", err)
		return "", false
	}
	if !config.IsSupportedScheme(canonical) {
		completionDebugf("stop: server URL scheme unsupported: %s", config.DisplayServerURL(raw))
		return "", false
	}
	return canonical, true
}

// resolveCompletionToken resolves the token for a TAB completion request
// in this order: env token, file credential store, exec-cred cache. It
// NEVER touches the keyring.
// It uses resolveCompletionConnection for URL/CA so env overrides
// (RANCHER_CLI_URL, RANCHER_CLI_CACERT) are honoured.
//
//   - RANCHER_CLI_TOKEN env - full capability (management + downstream).
//   - file credential store login token - full capability (explicit file
//     policy or pinned to file only; unpinned auto does NOT qualify).
//   - single unexpired exec-cred cache entry for server+cluster -
//     downstream-scoped (ID-only for cluster-dependent sites).
//
// Returns a zero TokenCapability (Token=="") when no usable source is found.
func resolveCompletionToken(clusterID string) TokenCapability {
	conn := resolveCompletionConnection()
	if conn.ServerURL == "" {
		completionDebugf("stop: no server URL (no current server in config and no RANCHER_CLI_URL)")
		return TokenCapability{}
	}
	completionDebugf("connection: url=%s ca=%t", config.DisplayServerURL(conn.ServerURL), conn.CACert != "")

	// Validate the server URL before any stored-credential read. Completion
	// is best-effort: an unsupported or malformed URL silently yields an
	// empty capability rather than surfacing a stored record that cannot
	// be safely used (never trigger a helper exec for an invalid identity).
	canonServerURL, ok := completionServerURL(conn.ServerURL)
	if !ok {
		return TokenCapability{}
	}

	dir := ""
	if v, ok := loadedConfigView(); ok {
		dir = v.ConfigDir()
	}

	// Env token - full management capability.
	if tok := os.Getenv(config.EnvToken); tok != "" {
		completionDebugf("token: %s (full capability)", config.EnvToken)
		return TokenCapability{Token: tok, IDOnly: false, ServerURL: conn.ServerURL, CACert: conn.CACert}
	}

	// File credential store - full management capability.
	// Only when the store policy explicitly targets the file.
	// Pass the effective URL so fileStoreTokenSilent looks up the credential for
	// the server whose URL MATCHES the effective URL, not the stored current server
	// name (RANCHER_CLI_URL pointing at server B while config
	// current server is A must not send A's token to B).
	if tok := fileStoreTokenSilent(dir, conn.ServerURL); tok != "" {
		completionDebugf("token: file credential store (full capability)")
		return TokenCapability{Token: tok, IDOnly: false, ServerURL: conn.ServerURL, CACert: conn.CACert}
	}

	// Neither remaining branch may touch a file without a directory to
	// read it from. A nil snapshot or an empty directory would send the
	// cache read at a path relative to the working directory, where a
	// token-cache.yaml belonging to nothing in particular would be read
	// and its token used to answer a live completion request.
	if dir == "" || storedConfigSnapshot() == nil {
		completionDebugf("stop: no config directory resolved for this invocation")
		return TokenCapability{}
	}

	// Exec-cred cache - downstream-scoped, ID-only.
	if clusterID == "" {
		completionDebugf("stop: no token: no %s, no file-store login for this URL, and no --cluster for the exec-credential cache", config.EnvToken)
		return TokenCapability{}
	}
	serverID := canonServerURL
	// Completion is the fourth reader of the exec-credential cache and
	// takes the same judgement as the other three: a record made in
	// another directory, or under a login this entry no longer has, is a
	// miss, and the binding stamp is read again before the token is used
	// so a login that completed in between is not answered around.
	anchor := config.ProcessCacheAnchor()
	if !anchor.SameDirectory(dir) {
		anchor = nil
	}
	entries, hit, err := config.CachedHit(anchor, func(a *config.CacheAnchor) ([]config.CachedCredEntry, bool, error) {
		found, rerr := config.ReadOnlyCachedCredsForClusterWithTokens(dir, serverID, clusterID, a)
		return found, len(found) > 0, rerr
	})
	if err != nil {
		completionDebugf("stop: exec-credential cache: %v", err)
		return TokenCapability{}
	}
	if !hit {
		completionDebugf("stop: no usable exec-credential cache record for cluster %s", clusterID)
		return TokenCapability{}
	}
	// Exclude legacy:-prefixed cache entries before the single-entry ambiguity
	// check. Shim slots (written by `rancher token` for ACE kubeconfigs)
	// are never completion candidates - a user never selects them via --name, and
	// the native get-token command explicitly rejects them. Filtering here
	// preserves the fail-closed semantics for genuinely ambiguous NATIVE
	// entries: shim + one native → native resolves; shim only → no candidates.
	// config.IsLegacyCacheSlot is the single source for this predicate so a
	// future prefix rename compiles instead of silently diverging.
	var native []config.CachedCredEntry
	for _, e := range entries {
		if !config.IsLegacyCacheSlot(e.Name) {
			native = append(native, e)
		}
	}
	if len(native) != 1 {
		// Missing, error, or ambiguous (multiple principals: skip).
		completionDebugf("stop: no token: %d unexpired exec-credential cache entries for cluster %s (need exactly 1)", len(native), clusterID)
		return TokenCapability{}
	}
	tok := native[0].Token
	if tok == "" {
		completionDebugf("stop: exec-credential cache entry for cluster %s has an empty token", clusterID)
		return TokenCapability{}
	}
	completionDebugf("token: exec-credential cache for cluster %s (id-only capability)", clusterID)
	return TokenCapability{Token: tok, IDOnly: true, ServerURL: conn.ServerURL, CACert: conn.CACert}
}

// fileStoreTokenSilent reads the login token from the file credential store
// WITHOUT logging a warnLoosePerm warning. Returns "" when:
//   - the directory does not read the file store: the policy is not file,
//     the pin does not name file, and for an unpinned auto directory what
//     its entries have stored is not the file store (config.UnpinnedStoreOf)
//   - credentials.yaml is absent or unreadable
//   - no stored server URL matches effectiveURL (a cross-server
//     URL override must not send server A's token to server B)
//
// effectiveURL is the resolved server URL from resolveCompletionConnection;
// it may differ from the stored current server's URL when RANCHER_CLI_URL is set.
//
// The current server is the operator's own selection: when its URL is the
// effective URL its token is used directly, exactly as an authenticated
// command would. Several stored entries may legitimately share one URL
// (re-logins under new names, per-CA variants), so the URL scan and its
// ambiguity guard apply only when the effective URL points somewhere other
// than the current server.
func fileStoreTokenSilent(dir, effectiveURL string) string {
	cfg := storedConfigSnapshot()
	if cfg == nil || dir == "" || effectiveURL == "" {
		return ""
	}
	// Completion offers nothing at all through a store the directory did
	// not declare: a token read against a policy the directory disagrees
	// with is the same wrong-identity read anywhere else, and completion
	// has no way to report it.
	policy, conflict := config.EffectivePolicy(cfg.CredentialStore)
	if conflict != nil {
		completionDebugf("file store skipped: %v", conflict)
		return ""
	}
	pin := config.ReadPin(dir)
	isFile := policy == "file" || (policy == "auto" && pin == "file")
	if policy == "auto" && pin == "" {
		// The same answer every reader of an unpinned directory gets:
		// file tokens and no identity record read the file store, and
		// credentials in both stores offer nothing.
		u, uerr := config.UnpinnedStoreOf(dir, cfg)
		if uerr != nil {
			completionDebugf("file store skipped: %v", uerr)
			return ""
		}
		isFile = u.Row == config.UnpinnedFile
	}
	if !isFile {
		completionDebugf("file store skipped: credential store policy=%s pin=%q (needs file)", policy, pin)
		return ""
	}

	// The current server wins when its URL is the effective URL - the same
	// priority internal/token's candidateRanker gives the
	// authenticated-command path. Completion is deliberately STRICTER than
	// that shared ranking: a current server without a stored token yields
	// nothing, never the token of another entry that shares the URL,
	// because completion runs silently and an unexpected identity could not
	// be noticed or corrected the way an interactive command's output
	// allows. A change to candidateRanker needs a matching decision here.
	if name, cur, err := cfg.CurrentServerEntry(); err == nil && config.SameServerURL(cur.URL, effectiveURL) {
		tok := config.ReadFileTokenSilent(dir, name)
		if tok == "" {
			completionDebugf("file store skipped: no login token stored for current server %q", name)
		}
		return tok
	}

	// Find the server whose URL matches the effective URL.
	// If no stored server matches unambiguously, skip the file token (the effective
	// URL is pointing at a different server than any stored context).
	var matchName string
	for name, srv := range cfg.Servers {
		if config.SameServerURL(srv.URL, effectiveURL) {
			if matchName != "" {
				// Ambiguous: two stored servers share the same URL - skip.
				completionDebugf("file store skipped: servers %q and %q both have URL %s and neither is current", matchName, name, config.DisplayServerURL(effectiveURL))
				return ""
			}
			matchName = name
		}
	}
	if matchName == "" {
		completionDebugf("file store skipped: no stored server has URL %s", config.DisplayServerURL(effectiveURL))
		return ""
	}
	// Silent read: bypass NewFileStore.GetToken (which calls warnLoosePerm).
	return config.ReadFileTokenSilent(dir, matchName)
}
