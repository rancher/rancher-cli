package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"sigs.k8s.io/yaml"
)

const fileName = "config.yaml"

// LoginHint is the canonical remediation hint pointing users at auth login.
const LoginHint = "run: rancher auth login --server <url>"

// LoginOrEnvHint is LoginHint's counterpart for connection resolution, which
// also accepts an env-only session (no stored server needed).
const LoginOrEnvHint = "run: rancher auth login --server <url>, or set " + EnvURL + " and " + EnvToken

// ErrNotLoggedIn builds the standard not-logged-in error for callers that
// HAVE the loaded config and can see stored servers: after a logout the
// current-server pointer is cleared but server entries survive, so the
// answer to "am I logged in" is "no — and here is the shortcut back", never
// a hint that implies a blank machine. With no stored servers it falls back
// to the blank-machine wording, which is then accurate.
func (c *Config) ErrNotLoggedIn() *clierr.Error {
	servers := make(map[string]string, len(c.Servers))
	for n, s := range c.Servers {
		servers[n] = s.URL
	}
	return ErrNotLoggedInFor(servers)
}

// ErrNotLoggedInFor is ErrNotLoggedIn's core over a name→URL map, usable by
// callers that carry Effective.KnownServers instead of a loaded Config
// (kube.RESTConfig — the shared client factory behind nearly every server
// command). The hinted command is complete and valid verbatim: auth login
// requires --server, so a bare --name suggestion would fail validation.
func ErrNotLoggedInFor(servers map[string]string) *clierr.Error {
	if len(servers) == 0 {
		return clierr.New(clierr.CodeNotLoggedIn,
			"not logged in and no "+EnvURL+" set").WithHint(LoginOrEnvHint)
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	hint := fmt.Sprintf("run: rancher auth login --server %s --name %s", servers[names[0]], names[0])
	if len(names) > 1 {
		hint = "run: rancher auth login --server <url> --name <name> — configured servers: " +
			strings.Join(names, ", ")
	}
	return clierr.New(clierr.CodeNotLoggedIn, "not logged in — no current server").WithHint(hint)
}

// CompletionConfig holds the shell-completion preferences written by
// `config set-completion` (AC-1, AC-2).
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
	// or unparseable (AC-2).
	CompletionTimeoutDefault = 500 * time.Millisecond
	// CompletionTimeoutMax is the hard cap for per-TAB HTTP budgets (AC-2).
	CompletionTimeoutMax = 2 * time.Second
)

// EffectiveCompletionTimeout returns the per-TAB HTTP budget from cfg,
// applying default/floor/cap semantics (AC-2):
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

// Server is a Rancher endpoint (secrets live in the credential store, not here).
type Server struct {
	URL string `json:"url"`
	CA  string `json:"ca,omitempty"`
}

// Config is the on-disk CLI configuration. Beyond server entries and
// per-machine preferences it holds exactly one piece of session state: the
// current-server pointer. Scope (cluster, project) is never persisted —
// every command takes it per invocation.
type Config struct {
	CurrentServer   string            `json:"currentServer,omitempty"`
	Servers         map[string]Server `json:"servers,omitempty"`
	CredentialStore string            `json:"credentialStore,omitempty"`
	// ReadOnly is one of the three OR'd sources (config/env/flag) that put a
	// session into read-only mode — see runtime.IO.ReadOnly. One-way: there
	// is no per-invocation override to turn this back off from here.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Completion holds per-machine completion preferences (not user config —
	// live completion is inherently machine-dependent). Stored inline in
	// config.yaml for discoverability; see `config set-completion`.
	Completion CompletionConfig `json:"completion,omitempty"`

	path string
}

// FilePath returns the config-file path inside a config directory.
func FilePath(dir string) string { return filepath.Join(dir, fileName) }

// Load reads a config file; a missing file yields an empty, usable Config.
func Load(path string) (*Config, error) {
	c := &Config{
		Servers: map[string]Server{},
		path:    path,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := yaml.Unmarshal(b, c); err != nil {
		return c, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.Servers == nil {
		c.Servers = map[string]Server{}
	}
	c.path = path
	return c, nil
}

// LoadAndStore parses config.yaml at dir and returns the loaded Config and
// a lazy credential store for it. It encapsulates the Load + NewLazyStore
// pairing used by both the CLI adapter (surface/cli resolveDeps) and the
// legacy token shim's cache-miss branch, so a future change to either step
// (a migration check, an extra Config field driving store selection) applies
// in one place rather than two.
func LoadAndStore(dir string) (*Config, CredStore, error) {
	cfg, err := Load(FilePath(dir))
	if err != nil {
		return nil, nil, err
	}
	store, err := NewLazyStore(cfg.CredentialStore, dir)
	if err != nil {
		return nil, nil, err
	}
	return cfg, store, nil
}

// Write persists the config as 0600 YAML, creating the dir at 0700.
// Write itself is lock-free: any COMMAND doing load-modify-write must run
// the whole span inside config.WithLock (the command sites' lock boundary
// tests are the enforcement point) — a direct load→Write from a new command
// reintroduces the concurrent lost-update the lock exists to prevent.
func (c *Config) Write() error {
	if c.path == "" {
		return fmt.Errorf("config path is empty")
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return atomicWriteFile(c.path, b, 0600)
}

// Path returns the file path this config was loaded from / writes to.
func (c *Config) Path() string { return c.path }

// SetPath overrides the write target.
func (c *Config) SetPath(p string) { c.path = p }

// CurrentServerEntry resolves the server the current-server pointer names.
// A dangling pointer errors here, at use time — Load stays lenient so repair
// commands (config use-server, auth logout --all) can load and fix the file.
func (c *Config) CurrentServerEntry() (string, Server, error) {
	if c.CurrentServer == "" {
		return "", Server{}, clierr.New(clierr.CodeNotLoggedIn,
			"no current server set").WithHint(LoginHint)
	}
	s, ok := c.Servers[c.CurrentServer]
	if !ok {
		return "", Server{}, clierr.New(clierr.CodeNotLoggedIn,
			fmt.Sprintf("current server %q not found", c.CurrentServer))
	}
	return c.CurrentServer, s, nil
}
