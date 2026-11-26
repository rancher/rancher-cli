package listed

import "github.com/rancher/rancher-cli/internal/secret"

// Build calls Reveal directly.
func Build(v secret.Value) string { return secret.Reveal(v) }
