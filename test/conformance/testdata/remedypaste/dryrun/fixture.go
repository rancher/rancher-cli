package fixture

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// texts holds the commands the dry-run check reads: a line marked hit must be
// reported and every other line must pass.
func texts() []string {
	return []string{
		"preview with: rancher delete token x --dry-run",
		"preview with: rancher get users",
		"run: rancher delete token x",                 // hit
		"run: rancher delete token x --apply --yes",   // hit
		"run: rancher delete token x --dry-run --yes", // hit
		clierr.Rancher + " delete token 'x' --dry-run",
		clierr.Rancher + " project remove-namespace 'n' --cluster 'c' --dry-run",
		clierr.Rancher + " project remove-namespace 'n' --cluster 'c'", // hit
	}
}

// gated is a site whose command keeps the gates of its command.
func gated(name string) string {
	return clierr.Rancher + " delete token " + clierr.ShellQuote(name) + " --apply --yes"
}

// ungated writes the same text and is not in the gated list.
func ungated(name string) string {
	return clierr.Rancher + " delete token " + clierr.ShellQuote(name) + " --apply --yes" // hit
}

// withheld is a site in the gated list whose text lost a gate.
func withheld(name string) string {
	return clierr.Rancher + " delete token " + clierr.ShellQuote(name) + " --apply" // hit
}
