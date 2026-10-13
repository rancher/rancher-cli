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

// DirSource names where a resolved config directory came from.
type DirSource string

const (
	DirSourceFlag    DirSource = "flag"
	DirSourceEnv     DirSource = "env"
	DirSourceXDG     DirSource = "xdg"
	DirSourceDefault DirSource = "default"
)

// resolveDirFrom resolves the config directory from the three inputs the
// CLI owns, passed explicitly rather than read here: clearing all three
// yields the platform default without mutating the process environment,
// which a parallel test could not do safely.
func resolveDirFrom(flagDir, envDir, xdgHome string) (string, DirSource, error) {
	if flagDir != "" {
		return flagDir, DirSourceFlag, nil
	}
	if envDir != "" {
		return envDir, DirSourceEnv, nil
	}
	if xdgHome != "" {
		return filepath.Join(xdgHome, "rancher"), DirSourceXDG, nil
	}
	if goos == "windows" {
		if dir, err := os.UserConfigDir(); err == nil {
			return filepath.Join(dir, "rancher"), DirSourceDefault, nil
		}
		// %AppData% unset (rare): fall through to the home-relative path
		// rather than failing - same shape the CLI has always produced.
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".config", "rancher"), DirSourceDefault, nil
}

// ResolveDir returns the config directory using precedence:
// flag > RANCHER_CLI_CONFIG_DIR > $XDG_CONFIG_HOME/rancher > default.
// The default is ~/.config/rancher, except on Windows where it is
// os.UserConfigDir()/rancher (%APPDATA%\rancher) - the platform
// convention. The Unix/macOS default is deliberately NOT moved to
// UserConfigDir (that would relocate existing macOS users' config to
// ~/Library/Application Support with no migration). A flag or variable
// value is returned as the caller spelled it, so a relative value stays
// relative; ResolveDirWithSource returns the canonical form instead.
func ResolveDir(flagDir string) (string, error) {
	dir, _, err := resolveDirFrom(flagDir, os.Getenv(EnvConfigDir), os.Getenv("XDG_CONFIG_HOME"))
	return dir, err
}

// ResolveDirWithSource returns the resolved config directory as an
// absolute, cleaned path together with where it came from - one value any
// caller that has to name the directory can record or compare. Symlinks
// are preserved rather than resolved: two spellings that reach one
// directory through different links stay two values, and IsDefaultDir
// decides through the filesystem whether they are one place.
func ResolveDirWithSource(flagDir string) (string, DirSource, error) {
	dir, source, err := resolveDirFrom(flagDir, os.Getenv(EnvConfigDir), os.Getenv("XDG_CONFIG_HOME"))
	if err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	return abs, source, nil
}

// PlatformDefaultDir returns the directory the CLI would use with none of
// its own three inputs set, in ResolveDirWithSource's canonical form.
// Every platform variable still counts (%APPDATA% on Windows, HOME
// elsewhere): the question it answers is "where would this machine put
// the config", not "where would an empty environment put it".
func PlatformDefaultDir() (string, error) {
	dir, _, err := resolveDirFrom("", "", "")
	if err != nil {
		return "", err
	}
	return filepath.Abs(dir)
}

// IsDefaultDir reports whether dir names the platform default directory.
// When both exist, os.SameFile decides, so a differently cased spelling
// of one directory on a case-insensitive filesystem answers true. When
// either is absent there is nothing to compare through the filesystem and
// the canonical forms are compared exactly, which is case-sensitive even
// where the filesystem is not.
func IsDefaultDir(dir string) (bool, error) {
	def, err := PlatformDefaultDir()
	if err != nil {
		return false, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	if abs == def {
		return true, nil
	}
	a, errA := os.Stat(abs)
	b, errB := os.Stat(def)
	if errA != nil || errB != nil {
		return false, nil
	}
	return os.SameFile(a, b), nil
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
// equality of credhelper.CanonicalServerURL outputs - the one server-identity
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

// EnvCACertFor returns RANCHER_CLI_CACERT for a command that connects to
// serverURL: empty when the variable is unset, or when RANCHER_CLI_URL is
// set and names another server - a variable exported with RANCHER_CLI_URL
// is issued for that server and never applied to a different one. Every
// reader of the variable goes through it.
func EnvCACertFor(serverURL string) string {
	v := os.Getenv(EnvCACert)
	if v == "" {
		return ""
	}
	if u := os.Getenv(EnvURL); u != "" && !SameServerURL(u, serverURL) {
		return ""
	}
	return v
}

// CASourceKind says where the CA a connection trusts came from.
type CASourceKind uint8

const (
	// CASourceNone is no custom CA: the system roots.
	CASourceNone CASourceKind = iota
	// CASourceFlag is --cacert, or --cacerts of the legacy token shim.
	CASourceFlag
	// CASourceVariable is RANCHER_CLI_CACERT.
	CASourceVariable
	// CASourceEntry is the CA saved on a server entry in config.yaml.
	CASourceEntry
)

// CASource is where a connection's CA came from: the kind, and for a
// server entry its name.
type CASource struct {
	Kind  CASourceKind
	Entry string
}

// Effective is the merged connection info (file + env overrides).
type Effective struct {
	ServerName string
	URL        string
	CA         string
	// CASource is where CA came from: RANCHER_CLI_CACERT, the current
	// server entry, or none - also none when a RANCHER_CLI_URL that names
	// another server cleared the entry's CA.
	CASource CASource
	Token    string
	// KnownServers is the configured server inventory (name → URL),
	// carried so a downstream consumer that finds URL empty (no current
	// server, no env) can answer with the re-login shortcut instead of
	// the blank-machine wording - see ErrNotLoggedInFor and kube.RESTConfig.
	KnownServers map[string]string
	// ServersWithCA holds the names of the entries in KnownServers that
	// save a CA, for the "not logged in" wording of a plugin login entry.
	ServersWithCA map[string]bool
	// noTokenFrom is the config whose current server's entry has no stored
	// credential, kept for UnfinishedChange; nil otherwise. The change
	// record is read only when a caller builds the "not logged in" error,
	// never here: the exec plugin takes this branch and goes on to serve
	// its cache.
	noTokenFrom *Config
}

// UnfinishedChange is what the "not logged in" error says about an
// unfinished store change when the current server's entry has no stored
// credential, and nothing otherwise. Call it only where that error is
// built: it reads the change record when config.yaml names one.
func (e Effective) UnfinishedChange() UnfinishedChange {
	if e.noTokenFrom == nil {
		return UnfinishedChange{}
	}
	return UnfinishedChangeOf(e.noTokenFrom.dir(), e.noTokenFrom)
}

// IsSupportedSchemeURL reports whether rawURL canonicalizes to an http or
// https identity. The credential store is read ONLY when this returns true;
// for an exec store that means the helper binary is never invoked for an
// unsupported or malformed server URL, so it acts as a pre-read guard.
// Exported so internal/token and internal/cli can share the same predicate
// without duplicating the canonicalization + scheme-check logic.
func IsSupportedSchemeURL(rawURL string) bool {
	cu, err := credhelper.CanonicalServerURL(rawURL)
	if err != nil {
		return false
	}
	return IsSupportedScheme(cu)
}

// IsSupportedScheme reports whether canonicalURL - a value already produced
// by credhelper.CanonicalServerURL - carries an http or https scheme. Cheap
// sibling of IsSupportedSchemeURL for callers that canonicalized (and paid
// that parse) one line earlier; raw, unvalidated input goes through
// IsSupportedSchemeURL instead.
func IsSupportedScheme(canonicalURL string) bool {
	scheme, _, ok := strings.Cut(canonicalURL, "://")
	return ok && (scheme == "http" || scheme == "https")
}

// EffectiveConnection is Effective without the credential: the server
// name, URL, CA and inventory the current server and the environment
// give, and no store access at all. A caller that needs only where to
// connect and whom to trust uses it, so it never reads, and cannot fail
// on, a credential it will not use.
func (c *Config) EffectiveConnection() Effective {
	eff, _, _, _ := c.connection()
	return eff
}

// connection resolves everything Effective resolves before the credential,
// and returns the current server's entry beside it for the credential read.
func (c *Config) connection() (eff Effective, name string, s Server, serverErr error) {
	if len(c.Servers) > 0 {
		eff.KnownServers = make(map[string]string, len(c.Servers))
		for n, srv := range c.Servers {
			eff.KnownServers[n] = srv.URL
			if srv.CA != "" {
				if eff.ServersWithCA == nil {
					eff.ServersWithCA = map[string]bool{}
				}
				eff.ServersWithCA[n] = true
			}
		}
	}
	name, s, serverErr = c.CurrentServerEntry()
	if serverErr == nil {
		// Field assignments, not a struct literal: a literal would zero the
		// KnownServers inventory built above.
		eff.ServerName, eff.URL, eff.CA = name, s.URL, s.CA
		if s.CA != "" {
			eff.CASource = CASource{Kind: CASourceEntry, Entry: name}
		}
	}

	// Effective URL first (env > file).
	if v := os.Getenv(EnvURL); v != "" {
		if serverErr == nil && !SameServerURL(v, s.URL) {
			// Never pair a CA or display name with a server they were not
			// issued for; and (in Effective) never read the stored token
			// either.
			eff.CA = ""
			eff.CASource = CASource{}
			eff.ServerName = ""
		}
		eff.URL = v
	}
	if v := EnvCACertFor(eff.URL); v != "" {
		eff.CA, eff.CASource = v, CASource{Kind: CASourceVariable}
	}
	return eff, name, s, serverErr
}

// Effective merges env vars over the current server. Env wins.
// Ordering is deliberate: the effective URL and env token are resolved BEFORE
// any credential-store access, so (a) RANCHER_CLI_TOKEN works even when the
// configured store is broken (CI escape hatch), and (b) a stored token is
// read only when the effective URL still addresses the server it belongs to -
// a divergent RANCHER_CLI_URL never receives another server's credential.
func (c *Config) Effective(store CredStore) (Effective, error) {
	eff, name, s, serverErr := c.connection()

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
			// No stored token: leave it empty; RESTConfig reports it with a
			// login hint, and names an unfinished store change beside it.
			eff.noTokenFrom = c
		default:
			return Effective{}, fmt.Errorf("credential store: %w", terr)
		}
	}
	return eff, nil
}

// readOnlyActive and readOnlySources carry the process-wide resolved
// read-only state (config readOnly + RANCHER_CLI_READONLY env + --read-only
// flag, OR'd together), set once by the CLI adapter's root Before hook
// before any command dispatches - a process-wide stash so the resolved state
// crosses layers. internal/runtime must not import internal/config, so the runtime
// pipeline and the schema command never call this directly: internal/cli
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
