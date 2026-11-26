package noreason

import "github.com/rancher/rancher-cli/internal/secret"

// Build is listed with no reason.
func Build(v secret.Value) string { return secret.Reveal(v) }
