// Package fragmentword is a fixture of the exact-value scans.
package fragmentword

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text asks for a flag with a variable spelling and a raw value.
func text(flag clierr.CommandWord, value string) string {
	return "the flag is missing; pass " + string(flag) + " " + value // want 4.8
}
