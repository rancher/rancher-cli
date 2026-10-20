package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"sigs.k8s.io/yaml"
)

const fileName = "config.yaml"

// LoginHintFor returns the bare, shell-quoted `rancher auth login <name>`
// command for name - the concrete login hint every not-logged-in outcome
// below emits when (and only when) a name is known. Replaces the retired
// LoginHint constant, which could not carry a concrete name under the
// executable-hint contract (a machine hint is a complete command, never a
// placeholder). A name in the exec plugin's own login entry namespace gets
// none: auth login refuses it, and a hint is a command that runs.
func LoginHintFor(name string) string {
	if IsExecLoginEntryName(name) {
		return ""
	}
	return LoginCommand + " " + clierr.ShellQuote(name)
}

// The remedy commands the shared credential and store-change code names,
// each opened by clierr.Rancher so the invocation that prints it can name
// its config directory.
const (
	// LoginCommand logs in; LoginHintFor names the entry.
	LoginCommand = clierr.Rancher + " auth login"
	// DeleteServerCommand removes a server entry; the entry follows.
	DeleteServerCommand = clierr.Rancher + " config delete-server"
	// TokenReuseOffCommand turns token reuse off.
	TokenReuseOffCommand = clierr.Rancher + " config set-token-reuse off"
	// StatusCommand reports the directory's state, a store change's
	// included: the hint of every error about a store change that no other
	// command fixes, since its report shows the change record, what is left
	// and why it stopped.
	StatusCommand = clierr.Rancher + " config status"
)

// LogoutHintFor is the command that logs out entry name.
func LogoutHintFor(name string) string {
	return clierr.Rancher + " auth logout " + clierr.ShellQuote(name)
}

// ErrNotLoggedIn builds the standard not-logged-in error for callers that
// HAVE the loaded config and can see stored servers: the current-server
// pointer is empty, dangling, or (after a logout, which never touches the
// pointer) simply credential-less, but server entries survive either way,
// so the answer to "am I logged in" is "no - and here is the shortcut
// back", never a hint that implies a blank machine. With no stored
// servers it falls back to the blank-machine wording, which is then
// accurate.
func (c *Config) ErrNotLoggedIn() *clierr.Error {
	servers := make(map[string]string, len(c.Servers))
	withCA := make(map[string]bool, len(c.Servers))
	for n, s := range c.Servers {
		servers[n] = s.URL
		withCA[n] = s.CA != ""
	}
	return ErrNotLoggedInFor(servers, withCA)
}

// InlineCAClause is the clause that follows an auth login command whose
// CA is carried inline (a PEM, not a path): the login needs the CA from a
// file, which the command cannot name, and says where the PEM is carried.
// The login remedies of internal/token and ErrNotLoggedInFor share it.
func InlineCAClause(src CASource) string {
	const clause = ", adding --cacert with the path of a file that holds the server's CA certificate, which "
	switch {
	case src.Kind == CASourceVariable:
		return clause + EnvCACert + " carries inline"
	case src.Kind == CASourceEntry && src.Entry != "":
		return clause + "server entry " + clierr.ShellQuote(src.Entry) + " carries inline in config.yaml"
	}
	return clause + "this exec entry carries inline"
}

// ErrNotLoggedInFor is ErrNotLoggedIn's core over a name→URL map, usable by
// callers that carry Effective.KnownServers instead of a loaded Config
// (kube.RESTConfig - the shared client factory behind nearly every server
// command). Exact zero/one/many outcomes: zero known servers puts onboarding
// guidance in the message BODY with no machine hint (neither a name nor a
// URL is known, so nothing here is a real value to interpolate); exactly one
// gets a concrete quoted login hint; more than one lists every name as a
// selectable candidate plus body guidance, again with no template hint - a
// rewritten constructor cannot adopt the executable-hint contract and keep a
// placeholder form. withCA names the entries that save a CA: the login the
// message gives for the plugin login entry's server then adds the CA it
// needs, and drops the hint, which would not run as written.
func ErrNotLoggedInFor(servers map[string]string, withCA map[string]bool) *clierr.Error {
	if len(servers) == 0 {
		return clierr.New(clierr.CodeNotLoggedIn,
			"not logged in and no "+EnvURL+" set; add a server with: "+clierr.Rancher+" auth login --url <url>")
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 1 {
		if IsExecLoginEntryName(names[0]) {
			// auth login refuses the name; a login under the name derived
			// from the server's host and port reaches the same server. The
			// name is given, since auth login derives none from an IPv6
			// address; it is free, the plugin login entry being the only one,
			// and has no leading dash, which would read as a flag ([::1]).
			url := servers[names[0]]
			name := strings.TrimLeft(clierr.HostEntryName("", url), "-")
			login := LoginCommand + " " + clierr.ShellQuote(name) + " --url " + clierr.ShellQuote(url)
			msg := "not logged in - no current server; the only server entry, " +
				clierr.ShellQuote(names[0]) + ", is the exec plugin's own login entry, which auth login refuses; log in under a server entry of your own with: "
			if withCA[names[0]] {
				return clierr.New(clierr.CodeNotLoggedIn, msg+login+InlineCAClause(CASource{Kind: CASourceEntry, Entry: names[0]}))
			}
			return clierr.New(clierr.CodeNotLoggedIn, msg+login).WithHint(login)
		}
		return clierr.New(clierr.CodeNotLoggedIn, "not logged in - no current server").
			WithHint(LoginHintFor(names[0]))
	}
	cands := make([]clierr.Candidate, 0, len(names))
	for _, n := range names {
		cands = append(cands, clierr.Candidate{ID: n, Name: n})
	}
	return clierr.New(clierr.CodeNotLoggedIn,
		"not logged in - no current server; configured servers: "+strings.Join(names, ", ")+
			"; select one with: "+clierr.Rancher+" config use-server <name>").
		WithCandidates(cands)
}

// CompletionConfig holds the shell-completion preferences written by
// `config set-completion`.
type CompletionConfig struct {
	// Network enables live resource-name completion over the Rancher API.
	// Default false; set via `config set-completion --network true`.
	Network bool `json:"network,omitempty"`
	// Timeout is the whole-request budget per TAB invocation stored as a
	// plain-text duration string (e.g. "500ms", "1s"). Using string avoids
	// time.Duration's YAML marshal as integer nanoseconds.
	// Parsed and clamped by EffectiveCompletionTimeout; hand-edited values
	// outside [1ms, 2s] are clamped at use, not at write.
	Timeout string `json:"timeout,omitempty"`
}

const (
	// CompletionTimeoutDefault is the effective timeout when Timeout is empty
	// or unparseable.
	CompletionTimeoutDefault = 500 * time.Millisecond
	// CompletionTimeoutMax is the hard cap for per-TAB HTTP budgets.
	CompletionTimeoutMax = 2 * time.Second
)

// EffectiveCompletionTimeout returns the per-TAB HTTP budget from cfg,
// applying default/floor/cap semantics:
//   - empty or unparseable → CompletionTimeoutDefault (500ms)
//   - ≤ 0 → CompletionTimeoutDefault (floor non-positive)
//   - > CompletionTimeoutMax → CompletionTimeoutMax (2s cap)
//
// Called by both the set-completion command and TAB dispatch so hand-edited
// values in config.yaml are always clamped at use.
func EffectiveCompletionTimeout(cfg *Config) time.Duration {
	d, _, _ := CompletionTimeoutWithRaw(cfg)
	return d
}

// CompletionTimeoutWithRaw returns the effective timeout, the raw configured
// string, and whether the raw value diverges from the effective (clamped,
// defaulted, or unparseable). Computed in one resolution so the effective and
// divergence sides can never disagree. `config status` reports this triple;
// EffectiveCompletionTimeout is the value-only form every other path uses.
func CompletionTimeoutWithRaw(cfg *Config) (effective time.Duration, raw string, diverges bool) {
	if cfg == nil || cfg.Completion.Timeout == "" {
		return CompletionTimeoutDefault, "", false
	}
	raw = cfg.Completion.Timeout
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return CompletionTimeoutDefault, raw, true
	}
	if d > CompletionTimeoutMax {
		return CompletionTimeoutMax, raw, true
	}
	return d, raw, false
}

// SupersededToken is a CLI-created token an entry no longer holds the
// credential for; see clierr.SupersededToken.
type SupersededToken = clierr.SupersededToken

// Server is a Rancher endpoint (secrets live in the credential store, not here).
type Server struct {
	URL string `json:"url"`
	CA  string `json:"ca,omitempty"`
	// CAFingerprint is the SHA-256 fingerprint (lowercase hex, see
	// tlstrust.Fingerprint) of the certificate a guided-trust consent
	// accepted for CA. It exists only while CA is exactly that single
	// certificate: the trust-prompt write and its rollback set it to the
	// accepted certificate's fingerprint, and the CA-only update and the URL
	// change clear it.
	CAFingerprint string `json:"caFingerprint,omitempty"`
	// CAFromEnvironment records, for an entry with no CA, that the last
	// auth login connected with RANCHER_CLI_CACERT: the server needs that CA
	// and the entry does not save it. It is never set while CA is set, is
	// cleared by a login that connected without the variable and by every
	// write that replaces or clears CA, and records only that fact, never
	// the variable's value. Revokes use the variable for such an entry.
	CAFromEnvironment bool `json:"caFromEnvironment,omitempty"`
	// Provenance says who created the stored login credential and therefore
	// who may revoke it. Absent on entries written before provenance was
	// persisted; those read as user entries.
	Provenance clierr.Provenance `json:"provenance,omitempty"`
	// TokenRef names the stored credential's server-side token, read only by
	// revocation. Missing or malformed means "cannot revoke by name".
	TokenRef *clierr.TokenRef `json:"tokenRef,omitempty"`
	// TokenUserID is the token's owner as the create response reported it,
	// or, for a pasted token, as auth login's SelfSubjectReview reported it;
	// empty when no response carried one (a SAML legacy credential, or a
	// pasted token stored with --no-verify).
	TokenUserID string `json:"tokenUserId,omitempty"`
	// TokenAuthConfig is the name of the auth config the token was made
	// through, as the principal of a token create response named it: the
	// prefix of the principal's name. Empty when no response carried one (a
	// pasted token, a v3 token, a login made before it was recorded); the
	// first token the exec plugin creates from the login for a call that
	// names an auth config records it.
	TokenAuthConfig string `json:"tokenAuthConfig,omitempty"`
	// CredentialExpiresAt is the server-reported expiry, for display only:
	// serving a token is decided by the server's answer, never by this.
	CredentialExpiresAt string `json:"credentialExpiresAt,omitempty"`
	// SupersededTokens are the CLI-created tokens earlier logins to this
	// entry left live; each is retired when a same-owner login replaces the
	// credential, or listed until then.
	SupersededTokens []SupersededToken `json:"supersededTokens,omitempty"`
	// LoggedOutAt is stamped by auth logout and config delete-server, which
	// clear TokenRef in the same write: the entry then holds no login, and
	// the token the logout deleted, when the CLI created and named it, is
	// one of SupersededTokens until it is revoked. Every login clears the
	// stamp, and so does a URL change. An entry an earlier version logged
	// out keeps its TokenRef beside the stamp: that names a live token with
	// no local credential, which config status lists like a superseded
	// record without reading the store.
	LoggedOutAt string `json:"loggedOutAt,omitempty"`
	// CacheBindingID says which exec-credential cache records belong to
	// this entry's current credential. It is a random value, advanced by
	// the first config write of every change of the entry's credential,
	// before any of it is written or deleted, so a record made from the
	// previous credential is a miss rather than a credential served for a
	// user this entry no longer holds.
	CacheBindingID string `json:"cacheBindingId,omitempty"`
	// CacheWritesPaused is set by that same first write and says the
	// change has not finished: no exec-credential cache record is written
	// for the entry until it is cleared. PauseLocked is a change made under
	// the config lock, PauseNoLock one made with RANCHER_CLI_CONFIG_NO_LOCK;
	// any other value reads as paused too.
	CacheWritesPaused string `json:"cacheWritesPaused,omitempty"`
}

// The values of CacheWritesPaused a change writes.
const (
	PauseLocked = "locked"
	PauseNoLock = "noLock"
)

// Paused reports whether the entry's exec-credential cache writes are
// paused.
func (s Server) Paused() bool { return s.CacheWritesPaused != "" }

// CLICreated reports whether a CLI login created the entry's current
// token: the provenance is derived, legacySession, or execLogin. A token
// the user pasted (userSupplied, or no provenance at all) is not ours to
// revoke, however the entry came to hold superseded records.
func (s Server) CLICreated() bool {
	switch s.Provenance {
	case clierr.ProvenanceDerived, clierr.ProvenanceLegacySession, clierr.ProvenanceExecLogin:
		return true
	}
	return false
}

// CLIOwned reports whether the entry's current token was created by the
// CLI and is named: a TokenRef is present to revoke it by.
func (s Server) CLIOwned() bool { return s.CLICreated() && s.TokenRef != nil }

// HoldsCLITokens reports whether the durable surfaces list this entry: it
// names a CLI-created token (an earlier version's logout leaves one beside
// its LoggedOutAt stamp), it holds a login the CLI created without naming
// its token (reported as "cannot revoke by name", never hidden), or it
// carries superseded records. An entry logged out with its TokenRef
// cleared is listed for its records only; a SAML legacy entry, whose token
// was never named, is not listed once logged out - the logout reported
// that token once.
func (s Server) HoldsCLITokens() bool {
	return s.CLIOwned() || (s.CLICreated() && s.LoggedOutAt == "") || len(s.SupersededTokens) > 0
}

// Clone returns a copy sharing no storage with s, for snapshots.
func (s Server) Clone() Server {
	c := s
	if s.TokenRef != nil {
		ref := *s.TokenRef
		c.TokenRef = &ref
	}
	c.SupersededTokens = slices.Clone(s.SupersededTokens)
	for i, r := range c.SupersededTokens {
		if r.LastAttempt != nil {
			la := *r.LastAttempt
			c.SupersededTokens[i].LastAttempt = &la
		}
	}
	return c
}

// OwnerState is how a superseded record's owner compares with the entry's
// current owner. Derived at display time, never persisted.
type OwnerState string

const (
	OwnerSame      OwnerState = "sameOwner"
	OwnerDifferent OwnerState = "differentOwner"
	OwnerUnknown   OwnerState = "unknownOwner"
)

// RecordOwnerState compares a record's owner with the current credential's:
// same only when both are known and equal; an empty owner on either side is
// unknown, because a delete with the wrong bearer answers NotFound for a
// live token and must never be attempted on a guess.
func RecordOwnerState(rec SupersededToken, currentUserID string) OwnerState {
	switch {
	case rec.UserID == "" || currentUserID == "":
		return OwnerUnknown
	case rec.UserID == currentUserID:
		return OwnerSame
	default:
		return OwnerDifferent
	}
}

// Config is the on-disk CLI configuration. Beyond server entries and
// per-machine preferences it holds exactly one piece of session state: the
// current-server pointer. Scope (cluster, project) is never persisted -
// every command takes it per invocation.
type Config struct {
	CurrentServer   string            `json:"currentServer,omitempty"`
	Servers         map[string]Server `json:"servers,omitempty"`
	CredentialStore string            `json:"credentialStore,omitempty"`
	// ReadOnly is one of the three OR'd sources (config/env/flag) that put a
	// session into read-only mode - see runtime.IO.ReadOnly. One-way: there
	// is no per-invocation override to turn this back off from here. Unlike
	// the --read-only flag, which local commands refuse, this source leaves
	// local commands usable.
	ReadOnly bool `json:"readOnly,omitempty"`
	// TokenReuse lets auth get-token create and use one plugin-created login
	// credential per server instead of a per-cluster interactive login.
	// Default off; set via `config set-token-reuse`. Governs only entries
	// whose provenance is execLogin.
	TokenReuse bool `json:"tokenReuse,omitempty"`
	// Completion holds per-machine completion preferences (not user config -
	// live completion is inherently machine-dependent). Stored inline in
	// config.yaml for discoverability; see `config set-completion`.
	// No omit tag: the block is written even when empty so it stays visible.
	Completion CompletionConfig `json:"completion"`
	// BindingStamp is the value cachebinding-stamp held when this file was
	// written. A cache reader compares it with the file before and after
	// its own work, so a load that was overtaken by a login cannot judge a
	// record against entries that have since moved. Written by the config
	// write itself, never by hand.
	BindingStamp string `json:"bindingStamp,omitempty"`
	// CredentialTransitionID names the credential store change this
	// directory has in flight. It is what activates the manifest file:
	// the file is written before the new credential commits and is inert
	// until a config write names its id, so a login that fails before
	// that write leaves nothing to finish. Written by the login, cleared
	// by the first config write after the last step completes.
	CredentialTransitionID string `json:"credentialTransitionId,omitempty"`

	path string
	// loadedInputs is what this config was read with of the inputs a cache
	// record is judged against, or nil for one built in memory. Write
	// compares against it to decide whether the directory needs a fresh
	// binding stamp; a nil baseline is treated as changed, because nothing
	// is known about what was there before.
	loadedInputs *recordInputs
}

// FilePath returns the config-file path inside a config directory.
func FilePath(dir string) string { return filepath.Join(dir, fileName) }

// ParseConfig unmarshals raw config.yaml bytes into a Config, applying the
// same Servers nil-guard Load does. Exported for callers that already hold
// the bytes (config edit's before/after restriction diff) and would
// otherwise have to round-trip through a temp file just to reuse Load.
func ParseConfig(b []byte) (*Config, error) {
	c := &Config{Servers: map[string]Server{}}
	if err := yaml.Unmarshal(b, c); err != nil {
		return c, err
	}
	if c.Servers == nil {
		c.Servers = map[string]Server{}
	}
	return c, nil
}

// Load reads a config file; a missing file yields an empty, usable Config.
//
// Every load reports itself to the operation observer, so the hot-path
// budget - no reader adds a config load, and the one reload a stale view
// costs adds exactly one - is asserted rather than assumed.
func Load(path string) (*Config, error) {
	observeIdentity("configload")
	c := &Config{
		Servers: map[string]Server{},
		path:    path,
	}
	b, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// A missing file has a known prior lookup - the empty one -
			// so the first write in a fresh directory stamps only if it
			// actually adds an entry.
			in := c.recordInputs()
			c.loadedInputs = &in
			return c, nil
		}
		return c, err
	}
	parsed, err := ParseConfig(b)
	if err != nil {
		return c, fmt.Errorf("parsing %s: %w", path, err)
	}
	parsed.path = path
	in := parsed.recordInputs()
	parsed.loadedInputs = &in
	return parsed, nil
}

// LoadAndStore parses config.yaml at dir and returns the loaded Config and
// a lazy credential store for it. It encapsulates the Load + NewLazyStore
// pairing used by both the CLI adapter (internal/cli resolveDeps) and the
// legacy token shim's cache-miss branch, so a future change to either step
// (a migration check, an extra Config field driving store selection) applies
// in one place rather than two.
func LoadAndStore(dir string) (*Config, CredStore, error) {
	cfg, err := Load(FilePath(dir))
	if err != nil {
		return nil, nil, err
	}
	store, err := NewLazyStore(cfg, dir)
	if err != nil {
		return nil, nil, err
	}
	return cfg, store, nil
}

// Write persists the config as 0600 YAML, creating the dir at 0700.
// Write itself is lock-free: any COMMAND doing load-modify-write must run
// the whole span inside config.WithLock (the command sites' lock boundary
// tests are the enforcement point) - a direct load→Write from a new command
// reintroduces the concurrent lost-update the lock exists to prevent.
// A write that changes which entries exist, which URL one answers for,
// which cache binding id one carries, the current server or the
// token-reuse setting also advances the directory's binding stamp: the
// stamp file first, then this file carrying the same value, so a reader
// that saw the old stamp and then loaded the new config never judges a
// cache record against entries that have moved - it re-anchors on the
// newer stamp and judges once more instead. A failure between the two
// leaves them permanently disagreeing, so every reader in the directory
// pays that second look and still misses, on every invocation, until the
// next such write; the old value is never put back.
func (c *Config) Write() error {
	if c.path == "" {
		return fmt.Errorf("config path is empty")
	}
	inputs := c.recordInputs()
	stamped := c.loadedInputs == nil || !c.loadedInputs.equal(inputs)
	if stamped {
		stamp, serr := newBindingStamp()
		if serr != nil {
			return serr
		}
		if werr := writeBindingStamp(filepath.Dir(c.path), stamp); werr != nil {
			return werr
		}
		c.BindingStamp = stamp
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(c.path, b, 0600); err != nil {
		return err
	}
	c.loadedInputs = &inputs
	return nil
}

// Path returns the file path this config was loaded from / writes to.
func (c *Config) Path() string { return c.path }

// dir is the directory this config was loaded from, or empty for one that
// names no file.
func (c *Config) dir() string {
	if c.path == "" {
		return ""
	}
	return filepath.Dir(c.path)
}

// SetPath overrides the write target.
func (c *Config) SetPath(p string) { c.path = p }

// CurrentServerEntry resolves the server the current-server pointer names.
// A dangling pointer errors here, at use time - Load stays lenient so repair
// commands (config use-server, auth logout --all) can load and fix the file.
func (c *Config) CurrentServerEntry() (string, Server, error) {
	if c.CurrentServer == "" {
		return "", Server{}, c.ErrNotLoggedIn()
	}
	s, ok := c.Servers[c.CurrentServer]
	if !ok {
		return "", Server{}, clierr.New(clierr.CodeNotLoggedIn,
			fmt.Sprintf("current server %q not found", c.CurrentServer))
	}
	return c.CurrentServer, s, nil
}
