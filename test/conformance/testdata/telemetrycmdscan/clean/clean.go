// Package clean is a fixture for the telemetry command scan: it uses only
// the allowed names of the configuration package, through an alias too.
package clean

import (
	"context"

	conf "github.com/rancher/rancher-cli/internal/config"
)

// Set loads, changes and writes the config under the lock.
func Set(ctx context.Context, dir string) error {
	return conf.WithLock(ctx, dir, func() error {
		var cfg *conf.Config
		cfg, err := conf.Load(conf.FilePath(dir))
		if err != nil {
			return err
		}
		text := "true"
		cfg.Telemetry = &text
		return cfg.Write()
	})
}
