package markdot

import . "github.com/rancher/rancher-cli/internal/clierr"

// build names the removal mark through a dot import.
func build(s string) string { return s + RemovalMark }
