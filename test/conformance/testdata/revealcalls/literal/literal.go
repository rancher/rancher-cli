package literal

import "github.com/rancher/rancher-cli/internal/secret"

// Build calls Reveal in a function literal.
func Build(v secret.Value) string {
	f := func() string { return secret.Reveal(v) }
	return f()
}
