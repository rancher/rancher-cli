package config

import (
	"os"
	"path/filepath"
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
