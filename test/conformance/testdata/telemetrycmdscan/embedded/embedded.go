// Package embedded is a fixture for the telemetry command scan: a struct
// embeds the config and a call reaches a method other than Write through it.
package embedded

import "github.com/rancher/rancher-cli/internal/config"

type wrapped struct{ *config.Config }

// Where loads the config and asks for its path through the embedded field.
func Where(dir string) (string, error) {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return "", err
	}
	w := wrapped{cfg}
	return w.Path(), nil
}
