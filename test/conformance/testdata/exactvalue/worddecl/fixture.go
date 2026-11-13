// Package worddecl is a fixture of the exact-value scans.
package worddecl

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// W is declared from the command word type.
type W clierr.CommandWord // want 4.1
