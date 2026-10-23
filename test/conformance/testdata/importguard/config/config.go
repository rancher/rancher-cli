// Package config is a fixture for the import guard: its closure holds the
// configuration package, which reaches credentials.
package config

import "github.com/rancher/rancher-cli/internal/config"

// Dir is a use of the import.
func Dir() (string, error) { return config.ResolveDir("") }
