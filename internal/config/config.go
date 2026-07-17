package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"sigs.k8s.io/yaml"
)

const fileName = "config.yaml"

// LoginHint is the canonical remediation hint pointing users at auth login.
const LoginHint = "run: rancher auth login --server <url>"

// LoginOrEnvHint is LoginHint's counterpart for connection resolution, which
// also accepts an env-only session (no stored context needed).
const LoginOrEnvHint = "run: rancher auth login --server <url>, or set " + EnvURL + " and " + EnvToken

// ErrContextNotSet builds the standard error for the choke point where
// connection resolution found no effective server URL at all — no stored
// context and no RANCHER_CLI_URL. Every caller of Effective that reaches this
// point (kube.RESTConfig, kubeconfig) shares one error so a future wording
// change lands once. Returns a fresh *clierr.Error each call.
func ErrContextNotSet() *clierr.Error {
	return clierr.New(clierr.CodeContextNotSet, "no current context and no "+EnvURL+" set").WithHint(LoginOrEnvHint)
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
	if cfg == nil || cfg.Completion.Timeout == "" {
		return CompletionTimeoutDefault
	}
	d, err := time.ParseDuration(cfg.Completion.Timeout)
	if err != nil || d <= 0 {
		return CompletionTimeoutDefault
	}
	if d > CompletionTimeoutMax {
		return CompletionTimeoutMax
	}
	return d
}

// Server is a Rancher endpoint (secrets live in the credential store, not here).
type Server struct {
	URL string `json:"url"`
	CA  string `json:"ca,omitempty"`
}

// Context binds a server to a cluster+project scope.
type Context struct {
	Server  string `json:"server"`
	Cluster string `json:"cluster,omitempty"`
	Project string `json:"project,omitempty"`
}

// Config is the on-disk CLI configuration (kubeconfig-shaped).
type Config struct {
	CurrentContext  string             `json:"current-context,omitempty"`
	Contexts        map[string]Context `json:"contexts,omitempty"`
	Servers         map[string]Server  `json:"servers,omitempty"`
	CredentialStore string             `json:"credentialStore,omitempty"`
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
		Contexts: map[string]Context{},
		Servers:  map[string]Server{},
		path:     path,
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
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	if c.Servers == nil {
		c.Servers = map[string]Server{}
	}
	c.path = path
	return c, nil
}

// Write persists the config as 0600 YAML, creating the dir at 0700.
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

// CurrentServerLookup returns the current context's server name, Server
// value, and any error. Unlike CurrentServer which returns (name, Server,
// error), this is a direct alias kept for call sites that need the name.
func (c *Config) CurrentServerLookup() (name string, srv Server, err error) {
	return c.CurrentServer()
}

// CurrentServer resolves the server referenced by the current context.
func (c *Config) CurrentServer() (string, Server, error) {
	if c.CurrentContext == "" {
		return "", Server{}, clierr.New(clierr.CodeContextNotSet,
			"no current context set").WithHint(LoginHint)
	}
	ctx, ok := c.Contexts[c.CurrentContext]
	if !ok {
		return "", Server{}, clierr.New(clierr.CodeContextNotSet,
			fmt.Sprintf("current context %q not found", c.CurrentContext))
	}
	s, ok := c.Servers[ctx.Server]
	if !ok {
		return "", Server{}, clierr.New(clierr.CodeContextNotSet,
			fmt.Sprintf("server %q for context %q not found", ctx.Server, c.CurrentContext))
	}
	return ctx.Server, s, nil
}
