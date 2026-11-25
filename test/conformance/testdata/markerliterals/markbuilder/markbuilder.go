package markbuilder

import "github.com/rancher/rancher-cli/internal/clierr"

// build writes the removal mark into a text, which a builder must not do.
func build(s string) string { return s + clierr.RemovalMark }
