package funcvalue

import "github.com/rancher/rancher-cli/internal/secret"

// Build keeps the function in a variable.
func Build() {
	r := secret.Reveal
	_ = r
}
