package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveDir returns the config directory using precedence:
// flag > RANCHER_CONFIG_DIR > $XDG_CONFIG_HOME/rancher > ~/.config/rancher.
func ResolveDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if env := os.Getenv("RANCHER_CONFIG_DIR"); env != "" {
		return env, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "rancher"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "rancher"), nil
}

// SplitContext splits a "cluster[:project]" scope value into its cluster and
// project axes. hasProject reports whether v contained a colon at all — a
// bare cluster name (no colon) must not clobber an already-resolved project
// axis, so per-axis callers should only assign project when hasProject is
// true; callers initializing a brand-new scope from scratch can ignore it.
func SplitContext(v string) (cluster, project string, hasProject bool) {
	return strings.Cut(v, ":")
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

// Effective is the merged connection info (file + env overrides).
type Effective struct {
	ServerName string
	URL        string
	CA         string
	Token      string
	Cluster    string
	Project    string
}

// Effective merges env vars over the current context/server. Env wins.
// RANCHER_TOKEN is used directly and never read from the store.
func (c *Config) Effective(store CredStore) (Effective, error) {
	var eff Effective

	// Start from file if a current context resolves.
	if name, s, err := c.CurrentServer(); err == nil {
		ctx := c.Contexts[c.CurrentContext]
		eff = Effective{
			ServerName: name, URL: s.URL, CA: s.CA,
			Cluster: ctx.Cluster, Project: ctx.Project,
		}
		tok, terr := store.GetToken(name)
		switch {
		case terr == nil:
			eff.Token = tok
		case errors.Is(terr, ErrNoToken):
			// no stored token: leave empty; RESTConfig reports it with LoginHint
		default:
			return Effective{}, fmt.Errorf("credential store: %w", terr)
		}
	}

	// Env overrides (env > file).
	if v := os.Getenv("RANCHER_URL"); v != "" {
		eff.URL = v
	}
	if v := os.Getenv("RANCHER_CACERT"); v != "" {
		eff.CA = v
	}
	if v := os.Getenv("RANCHER_TOKEN"); v != "" {
		eff.Token = v
	}
	if v := os.Getenv("RANCHER_CONTEXT"); v != "" {
		cl, pr, hasProject := SplitContext(v)
		eff.Cluster = cl
		if hasProject {
			eff.Project = pr
		}
	}

	// Flag override (flag > env > file).
	if v := contextOverride; v != "" {
		cl, pr, hasProject := SplitContext(v)
		eff.Cluster = cl
		if hasProject {
			eff.Project = pr
		}
	}
	return eff, nil
}

// contextOverride carries the global --context flag value, set once by the
// CLI adapter before dispatch. The flag outranks RANCHER_CONTEXT (env), which
// outranks the config file — the same env-over-file rule this block already
// applies, extended one level up. eff.Project is informational only (display,
// e.g. `config current-context`) — no routing or API-call code consumes it.
var contextOverride string

// SetContextOverride records the global --context (cluster[:project]) flag
// value for this process. An empty string clears/skips the override.
func SetContextOverride(contextVal string) {
	contextOverride = contextVal
}

// ContextOverride returns the currently recorded global --context flag value
// (see SetContextOverride). Write-commands whose own local --cluster flag is
// empty must not silently ignore a non-empty value here — the global flag
// scopes reads, not writes, and the two are easy to conflate since they share
// a name with a command's local write flag.
func ContextOverride() string {
	return contextOverride
}
