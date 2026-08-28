package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/rancher/rancher-cli/credhelper"
)

// goos is runtime.GOOS behind a package var, so ResolveDir's
// Windows-only default branch is unit-testable from any host.
var goos = runtime.GOOS

// ResolveDir returns the config directory using precedence:
// flag > RANCHER_CLI_CONFIG_DIR > $XDG_CONFIG_HOME/rancher > default.
// The default is ~/.config/rancher, except on Windows where it is
// os.UserConfigDir()/rancher (%APPDATA%\rancher) — the platform
// convention. The Unix/macOS default is deliberately NOT moved to
// UserConfigDir (that would relocate existing macOS users' config to
// ~/Library/Application Support with no migration).
func ResolveDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if env := os.Getenv(EnvConfigDir); env != "" {
		return env, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rancher"), nil
	}
	if goos == "windows" {
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "rancher"), nil
		}
		// %AppData% unset (rare): fall through to the home-relative path
		// rather than failing — same shape the CLI has always produced.
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "rancher"), nil
}

// ResolveCA turns a --cacert value or config CA (pem-or-path) into a PEM
// string. An inline PEM (has a BEGIN header) passes through; anything else is
// a file path.
func ResolveCA(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if strings.Contains(s, "-----BEGIN") {
		return s, nil
	}
	data, err := os.ReadFile(s)
	if err != nil {
		return "", fmt.Errorf("reading CA file %q: %w", s, err)
	}
	return string(data), nil
}

// SameServerURL reports whether a and b address the same server. Defined as
// equality of credhelper.CanonicalServerURL outputs — the one server-identity
// algorithm, shared with the helper wire protocol and helper records.
// Falls back to exact string equality when either side fails to canonicalize,
// so a malformed value never spuriously counts as a match.
func SameServerURL(a, b string) bool {
	ca, aErr := credhelper.CanonicalServerURL(a)
	cb, bErr := credhelper.CanonicalServerURL(b)
	if aErr != nil || bErr != nil {
		return a == b
	}
	return ca == cb
}

// Effective is the merged connection info (file + env overrides).
type Effective struct {
	ServerName string
	URL        string
	CA         string
	Token      string
	// KnownServers is the configured server inventory (name → URL),
	// carried so a downstream consumer that finds URL empty (no current
	// server, no env) can answer with the re-login shortcut instead of
	// the blank-machine wording — see ErrNotLoggedInFor and kube.RESTConfig.
	KnownServers map[string]string
}

// IsSupportedSchemeURL reports whether rawURL canonicalizes to an http or
// https identity. The credential store is read ONLY when this returns true;
// for an exec store that means the helper binary is never invoked for an
// unsupported or malformed server URL, so it acts as a pre-read guard.
// Exported so internal/token and surface/cli can share the same predicate
// without duplicating the canonicalization + scheme-check logic.
func IsSupportedSchemeURL(rawURL string) bool {
	cu, err := credhelper.CanonicalServerURL(rawURL)
	if err != nil {
		return false
	}
	return IsSupportedScheme(cu)
}

// IsSupportedScheme reports whether canonicalURL — a value already produced
// by credhelper.CanonicalServerURL — carries an http or https scheme. Cheap
// sibling of IsSupportedSchemeURL for callers that canonicalized (and paid
// that parse) one line earlier; raw, unvalidated input goes through
// IsSupportedSchemeURL instead.
func IsSupportedScheme(canonicalURL string) bool {
	scheme, _, ok := strings.Cut(canonicalURL, "://")
	return ok && (scheme == "http" || scheme == "https")
}

// Effective merges env vars over the current server. Env wins.
// Ordering is deliberate: the effective URL and env token are resolved BEFORE
// any credential-store access, so (a) RANCHER_CLI_TOKEN works even when the
// configured store is broken (CI escape hatch), and (b) a stored token is
// read only when the effective URL still addresses the server it belongs to —
// a divergent RANCHER_CLI_URL never receives another server's credential.
func (c *Config) Effective(store CredStore) (Effective, error) {
	var eff Effective
	if len(c.Servers) > 0 {
		eff.KnownServers = make(map[string]string, len(c.Servers))
		for n, srv := range c.Servers {
			eff.KnownServers[n] = srv.URL
		}
	}
	name, s, serverErr := c.CurrentServerEntry()
	if serverErr == nil {
		// Field assignments, not a struct literal: a literal would zero the
		// KnownServers inventory built above.
		eff.ServerName, eff.URL, eff.CA = name, s.URL, s.CA
	}

	// Effective URL first (env > file).
	if v := os.Getenv(EnvURL); v != "" {
		if serverErr == nil && !SameServerURL(v, s.URL) {
			// Never pair a CA or display name with a server they were not
			// issued for; and (below) never read the stored token either.
			eff.CA = ""
			eff.ServerName = ""
		}
		eff.URL = v
	}

	// Token: env short-circuits the store entirely; otherwise read the
	// stored token only when the effective URL still matches the configured
	// server it was stored for.
	if v := os.Getenv(EnvToken); v != "" {
		eff.Token = v
	} else if serverErr == nil && SameServerURL(eff.URL, s.URL) && IsSupportedSchemeURL(s.URL) {
		tok, terr := store.GetToken(ServerRef{Name: name, URL: s.URL})
		switch {
		case terr == nil:
			eff.Token = tok
		case errors.Is(terr, ErrNoToken):
			// no stored token: leave empty; RESTConfig reports it with a login hint
		default:
			return Effective{}, fmt.Errorf("credential store: %w", terr)
		}
	}

	if v := os.Getenv(EnvCACert); v != "" {
		eff.CA = v
	}
	return eff, nil
}

// readOnlyActive and readOnlySources carry the process-wide resolved
// read-only state (config readOnly + RANCHER_CLI_READONLY env + --read-only
// flag, OR'd together), set once by the CLI adapter's root Before hook
// before any command dispatches — a process-wide stash so the resolved state
// crosses layers. internal/runtime must not import internal/config, so the runtime
// pipeline and the schema command never call this directly: surface/cli
// reads it into runtime.IO.ReadOnly, and passes ReadOnlyOverride itself as
// the schema command's resolver function.
var (
	readOnlyActive  bool
	readOnlySources []string
)

// SetReadOnlyOverride records the resolved read-only state for this process.
func SetReadOnlyOverride(active bool, sources []string) {
	readOnlyActive = active
	readOnlySources = sources
}

// ReadOnlyOverride returns the recorded read-only state (see
// SetReadOnlyOverride).
func ReadOnlyOverride() (bool, []string) {
	return readOnlyActive, readOnlySources
}
