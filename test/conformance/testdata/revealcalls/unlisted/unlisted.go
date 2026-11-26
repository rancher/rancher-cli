package unlisted

import "github.com/rancher/rancher-cli/internal/secret"

// Other has no row.
func Other(v secret.Value) string { return secret.Reveal(v) }
