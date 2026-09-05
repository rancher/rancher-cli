package config

import (
	"errors"
	"fmt"
	"strings"
)

// Validate checks a loaded Config's referential integrity - checks Load
// itself never runs (Load only nil-guards the Servers map; a dangling
// current-server pointer otherwise surfaces lazily at
// CurrentServerEntry()/Effective() call time, so repair commands can load a
// broken file and fix it). It is the seam future config-integrity checks hang
// off; `config edit` is its first caller. Returns a single error joining
// every violation found, or nil when cfg is valid.
func Validate(cfg *Config) error {
	var errs []string

	if cfg.CurrentServer != "" {
		if _, ok := cfg.Servers[cfg.CurrentServer]; !ok {
			errs = append(errs, fmt.Sprintf("currentServer %q is not a defined server", cfg.CurrentServer))
		}
	}

	// "" means unset (EffectivePolicy falls back to auto); any other value
	// must satisfy the same allowlist OpenStore/NewLazyStore enforce, so
	// Validate cannot reject a policy the runtime accepts.
	if cfg.CredentialStore != "" {
		if err := checkPolicy(cfg.CredentialStore); err != nil {
			errs = append(errs, fmt.Sprintf("credentialStore: %v", err))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}
