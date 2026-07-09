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
		if cl, pr, ok := strings.Cut(v, ":"); ok {
			eff.Cluster, eff.Project = cl, pr
		} else {
			eff.Cluster = v
		}
	}

	// Flag overrides (flag > env > file).
	if v := contextOverride.context; v != "" {
		if cl, pr, ok := strings.Cut(v, ":"); ok {
			eff.Cluster, eff.Project = cl, pr
		} else {
			eff.Cluster = v
		}
	}
	if v := contextOverride.project; v != "" {
		eff.Project = v
	}
	return eff, nil
}

// contextOverride carries the global --context/--project flag values, set once
// by the CLI adapter before dispatch. Flags outrank RANCHER_CONTEXT (env),
// which outranks the config file — the same env-over-file rule this block
// already applies, extended one level up.
var contextOverride struct{ context, project string }

// SetContextOverride records the global --context (cluster[:project]) and
// --project flag values for this process. Empty strings clear/skip an axis.
func SetContextOverride(contextVal, project string) {
	contextOverride.context, contextOverride.project = contextVal, project
}
