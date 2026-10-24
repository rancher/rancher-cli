// Package methodexpr is a fixture for the telemetry command scan: a method
// expression calls a method of the config other than Write.
package methodexpr

import "github.com/rancher/rancher-cli/internal/config"

// Where loads the config and asks for its path through a method expression.
func Where(dir string) (string, error) {
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return "", err
	}
	return (*config.Config).Path(cfg), nil
}
