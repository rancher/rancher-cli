// Package othermethod is a fixture for the telemetry command scan: it calls
// a method of the config other than Write.
package othermethod

import "github.com/rancher/rancher-cli/internal/config"

// Where loads the config and asks for its path.
func Where(dir string) (string, error) {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return "", err
	}
	return cfg.Path(), nil
}
