package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Validate checks a loaded Config's referential integrity — checks Load
// itself never runs (Load only nil-guards the Contexts/Servers maps; a
// dangling reference otherwise surfaces lazily at CurrentServer()/Effective()
// call time). It is the seam future config-integrity checks hang off; `config
// edit` is its first caller. Returns a single error joining every violation
// found, or nil when cfg is valid.
func Validate(cfg *Config) error {
	var errs []string

	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		server := cfg.Contexts[n].Server
		if _, ok := cfg.Servers[server]; !ok {
			errs = append(errs, fmt.Sprintf("context %q references unknown server %q", n, server))
		}
	}

	if cfg.CurrentContext != "" {
		if _, ok := cfg.Contexts[cfg.CurrentContext]; !ok {
			errs = append(errs, fmt.Sprintf("currentContext %q is not a defined context", cfg.CurrentContext))
		}
	}

	switch cfg.CredentialStore {
	case "", "auto", "file", "keychain":
		// legal: the four policies OpenStore/EffectivePolicy accept.
	default:
		errs = append(errs, fmt.Sprintf("credentialStore %q is not one of: auto, file, keychain", cfg.CredentialStore))
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}
