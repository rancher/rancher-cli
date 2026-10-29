package unprefixed

import "github.com/rancher/rancher-cli/internal/clierr"

func plain(s string) string {
	_ = clierr.ExpandRemedies(s+"x", "")
	return clierr.ExpandRemedies(s, "")
}
