package cli

import (
	"os"
	"strconv"
	"strings"

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
	// (AC-6a: a downstream-scoped token cannot resolve a display name via the
	// management cluster list).
	IDOnly bool
	// ServerURL is the Rancher server URL (for HTTP client construction).
	ServerURL string
	// CACert is the PEM CA bundle (empty = system roots).
	CACert string
}

// completionConnection is the read-only resolved connection for a TAB request.
// It mirrors Config.Effective precedence (finding 7) without touching the
// credential store.
type completionConnection struct {
	ServerURL string
	CACert    string
	Cluster   string
	Project   string
}

// resolveCompletionConnection resolves server URL, CA, cluster, and project in
// strict read-only precedence, mirroring Config.Effective (finding 7, AC-17):
//
//  1. Config current context/server (base)
//  2. RANCHER_CLI_URL, RANCHER_CLI_CACERT, RANCHER_CLI_CONTEXT env overrides
//  3. --context from the parsed arg tail (highest precedence)
//
// Never touches the credential store or keyring (AC-22).
func resolveCompletionConnection(argTail []string) completionConnection {
	cfg := storedConfigSnapshot()
	var conn completionConnection

	// Base from config file.
	if cfg != nil {
		if name, srv, err := cfg.CurrentServer(); err == nil {
			ctx := cfg.Contexts[cfg.CurrentContext]
			conn.ServerURL = srv.URL
			conn.CACert = srv.CA
			conn.Cluster = ctx.Cluster
			conn.Project = ctx.Project
			_ = name
		}
	}

	// Env overrides (RANCHER_CLI_URL clears mismatched CA per Config.Effective semantics).
	if v := os.Getenv(config.EnvURL); v != "" {
		if !config.SameServerURL(v, conn.ServerURL) {
			conn.CACert = ""
		}
		conn.ServerURL = v
	}
	if v := os.Getenv(config.EnvCACert); v != "" {
		conn.CACert = v
	}
	if v := os.Getenv(config.EnvContext); v != "" {
		cl, pr, hasProject := config.SplitContext(v)
		conn.Cluster = cl
		if hasProject {
			conn.Project = pr
		}
	}

	// --context flag in arg tail (highest precedence).
	if v := extractFlagValue(argTail, "context"); v != "" {
		cl, pr, hasProject := config.SplitContext(v)
		conn.Cluster = cl
		if hasProject {
			conn.Project = pr
		}
	}

	// Resolve CA PEM from file path or inline PEM.
	if conn.CACert != "" {
		if pem, err := config.ResolveCA(conn.CACert); err == nil {
			conn.CACert = pem
		}
	}

	return conn
}

// resolveCompletionToken resolves the token for a TAB completion request
// following AC-4 → AC-5 → AC-6 order. It NEVER touches the keyring (AC-22).
// It uses resolveCompletionConnection for URL/CA (finding 7) so env overrides
// (RANCHER_CLI_URL, RANCHER_CLI_CACERT, RANCHER_CLI_CONTEXT) are honoured.
//
//   - AC-4: RANCHER_CLI_TOKEN env — full capability (management + downstream).
//   - AC-5: file credential store login token — full capability (explicit file
//     policy or pinned to file only; unpinned auto does NOT qualify, AC-5).
//   - AC-6: single unexpired exec-cred cache entry for server+cluster —
//     downstream-scoped (ID-only for cluster-dependent sites, AC-6a).
//
// argTail is the pre-stripped leaf arg tail (for --context flag extraction).
// Returns a zero TokenCapability (Token=="") when no usable source is found.
func resolveCompletionToken(argTail []string, clusterID string) TokenCapability {
	conn := resolveCompletionConnection(argTail)
	if conn.ServerURL == "" {
		return TokenCapability{}
	}

	// Validate the server URL before any stored-credential read. Completion
	// is best-effort: an unsupported or malformed URL silently yields an
	// empty capability rather than surfacing a stored record that cannot
	// be safely used (AC-22: never trigger a helper exec for an invalid identity).
	canonServerURL, csErr := credhelper.CanonicalServerURL(conn.ServerURL)
	if csErr != nil {
		return TokenCapability{}
	}
	if scheme, _, ok := strings.Cut(canonServerURL, "://"); !ok || (scheme != "http" && scheme != "https") {
		return TokenCapability{}
	}

	dir := ""
	if v, ok := loadedConfigView(); ok {
		dir = v.ConfigDir()
	}

	// AC-4: env token — full management capability.
	if tok := os.Getenv(config.EnvToken); tok != "" {
		return TokenCapability{Token: tok, IDOnly: false, ServerURL: conn.ServerURL, CACert: conn.CACert}
	}

	// AC-5: file credential store — full management capability.
	// Only when the store policy explicitly targets the file (AC-5, finding 8).
	// Pass the effective URL so fileStoreTokenSilent looks up the credential for
	// the server whose URL MATCHES the effective URL, not the stored current server
	// name (round-2 finding 3: RANCHER_CLI_URL pointing at server B while config
	// current server is A must not send A's token to B).
	if tok := fileStoreTokenSilent(dir, conn.ServerURL); tok != "" {
		return TokenCapability{Token: tok, IDOnly: false, ServerURL: conn.ServerURL, CACert: conn.CACert}
	}

	// AC-6: exec-cred cache — downstream-scoped, ID-only.
	if clusterID == "" {
		return TokenCapability{}
	}
	serverID := canonServerURL
	// Finding 8: use WithTokens variant to propagate the token through the
	// first file read; avoids a second parse of token-cache.yaml.
	entries, err := config.ReadOnlyCachedCredsForClusterWithTokens(dir, serverID, clusterID)
	if err != nil || len(entries) != 1 {
		// Missing, error, or ambiguous (AC-6a: multiple principals → skip).
		return TokenCapability{}
	}
	tok := entries[0].Token
	if tok == "" {
		return TokenCapability{}
	}
	return TokenCapability{Token: tok, IDOnly: true, ServerURL: conn.ServerURL, CACert: conn.CACert}
}

// fileStoreTokenSilent reads the login token from the file credential store
// WITHOUT logging a warnLoosePerm warning (AC-23, finding 8). Returns "" when:
//   - policy is not explicitly file, OR pin does not resolve to file (AC-5:
//     unpinned auto does NOT qualify — we only read when the user explicitly
//     chose file storage or has pinned the store to file)
//   - credentials.yaml is absent or unreadable
//   - no stored server URL matches effectiveURL (round-2 finding 3: cross-server
//     URL override must not send server A's token to server B)
//
// effectiveURL is the resolved server URL from resolveCompletionConnection;
// it may differ from the stored current server's URL when RANCHER_CLI_URL is set.
func fileStoreTokenSilent(dir, effectiveURL string) string {
	cfg := storedConfigSnapshot()
	if cfg == nil || dir == "" || effectiveURL == "" {
		return ""
	}
	policy := config.EffectivePolicy(cfg.CredentialStore)
	pin := config.ReadPin(dir)
	// AC-5: explicit file policy OR pin resolved to file — not unpinned auto.
	isFile := policy == "file" || (policy == "auto" && pin == "file")
	if !isFile {
		return ""
	}

	// Round-2 finding 3: find the server whose URL matches the effective URL.
	// If no stored server matches unambiguously, skip the file token (the effective
	// URL is pointing at a different server than any stored context).
	var matchName string
	for name, srv := range cfg.Servers {
		if config.SameServerURL(srv.URL, effectiveURL) {
			if matchName != "" {
				// Ambiguous: two stored servers share the same URL — skip.
				return ""
			}
			matchName = name
		}
	}
	if matchName == "" {
		return ""
	}
	// Silent read: bypass NewFileStore.GetToken (which calls warnLoosePerm).
	return config.ReadFileTokenSilent(dir, matchName)
}

// flagPresent reports whether --flagName or --flagName=... appears in argTail,
// regardless of whether a value follows. Used for string flags where an
// equals-form token with an empty or partial value is a mid-edit heuristic
// ("--project= <TAB>" is about to receive a value; presence is sufficient).
func flagPresent(argTail []string, flagName string) bool {
	long := "--" + flagName
	prefix := long + "="
	for _, a := range argTail {
		if a == long || strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// boolFlagSet reports whether a boolean flag is semantically set on the tail:
// bare "--name" counts as true; "--name=<value>" is parsed via strconv.ParseBool
// (covering "true"/"false"/"1"/"0"/"t"/"f"/"T"/"F"/"TRUE"/"FALSE" etc.); an
// unparseable value is treated as present (mid-edit token). Used by
// roleVocabulary so that "--global=false", "--global=0", "--global=False" do
// not select the global vocabulary — unlike string flags, any boolean-false
// spelling is unambiguous and should not trigger the mid-edit heuristic.
func boolFlagSet(argTail []string, flagName string) bool {
	long := "--" + flagName
	prefix := long + "="
	for _, a := range argTail {
		if a == long {
			return true
		}
		if strings.HasPrefix(a, prefix) {
			if v, err := strconv.ParseBool(strings.TrimPrefix(a, prefix)); err == nil {
				return v
			}
			return true // unparseable: mid-edit token, treat as present
		}
	}
	return false
}

// extractFlagValue scans argTail for --flagName value or --flagName=value and
// returns the value, or "" when not found. Used for --context extraction.
func extractFlagValue(argTail []string, flagName string) string {
	long := "--" + flagName
	prefix := long + "="
	for i, a := range argTail {
		if strings.HasPrefix(a, prefix) {
			return a[len(prefix):]
		}
		if a == long && i+1 < len(argTail) {
			return argTail[i+1]
		}
	}
	return ""
}
