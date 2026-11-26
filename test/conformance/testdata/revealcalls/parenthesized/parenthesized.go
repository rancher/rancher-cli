package parenthesized

import "github.com/rancher/rancher-cli/internal/secret"

// Build calls Reveal through parentheses.
func Build(v secret.Value) string { return (secret.Reveal)(v) }
