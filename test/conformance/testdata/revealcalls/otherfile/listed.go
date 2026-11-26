package otherfile

import "github.com/rancher/rancher-cli/internal/secret"

// Build is listed.
func Build(v secret.Value) string { return secret.Reveal(v) }
