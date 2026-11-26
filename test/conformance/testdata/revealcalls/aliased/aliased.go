package aliased

import s "github.com/rancher/rancher-cli/internal/secret"

// Build calls Reveal through an import alias.
func Build(v s.Value) string { return s.Reveal(v) }
