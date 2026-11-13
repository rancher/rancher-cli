// Package fragmentthenraw is a fixture of the exact-value scans.
package fragmentthenraw

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text asks for a flag value through a fragment, then prints a raw value.
func text(v string) string {
	frag, ok := clierr.FlagFragment("--project", v)
	if !ok {
		return clierr.WithLeftOutNote("the value is ambiguous")
	}
	return "the flag is missing; pass " + frag + " " + v // want 4.8
}
