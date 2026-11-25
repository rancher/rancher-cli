package markalias

import c "github.com/rancher/rancher-cli/internal/clierr"

// build names the removal mark through an import alias.
func build(s string) string { return s + c.RemovalMark }
