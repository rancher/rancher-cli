package otherfile

import "github.com/rancher/rancher-cli/internal/secret"

// helper is a function of another file with no row.
func helper(v secret.Value) string { return secret.Reveal(v) }
