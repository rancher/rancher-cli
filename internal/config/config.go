package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rancher/rancher-cli/internal/clierr"
	"sigs.k8s.io/yaml"
)

const fileName = "config.yaml"

// LoginHint is the canonical remediation hint pointing users at auth login.
const LoginHint = "run: rancher auth login --server <url>"

// LoginOrEnvHint is LoginHint's counterpart for connection resolution, which
// also accepts an env-only session (no stored context needed).
const LoginOrEnvHint = "run: rancher auth login --server <url>, or set RANCHER_URL and RANCHER_TOKEN"

// ErrContextNotSet builds the standard error for the choke point where
// connection resolution found no effective server URL at all — no stored
// context and no RANCHER_URL. Every caller of Effective that reaches this
// point (kube.RESTConfig, kubeconfig) shares one error so a future wording
// change lands once. Returns a fresh *clierr.Error each call.
func ErrContextNotSet() *clierr.Error {
	return clierr.New(clierr.CodeContextNotSet, "no current context and no RANCHER_URL set").WithHint(LoginOrEnvHint)
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

// SetPath overrides the write target (used by migration).
func (c *Config) SetPath(p string) { c.path = p }

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
