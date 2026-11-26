package results

import "github.com/rancher/rancher-cli/internal/secret"

// Build returns a string where its row says bool.
func Build(v secret.Value) string { return secret.Reveal(v) }
