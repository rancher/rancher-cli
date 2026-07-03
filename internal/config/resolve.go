package config

import (
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
		if tok, terr := store.GetToken(name); terr == nil {
			eff.Token = tok
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
	return eff, nil
}
