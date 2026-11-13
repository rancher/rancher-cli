package twovalues

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func two(name string) string {
	return fmt.Sprintf("%q %q %q", clierr.ShellQuote(name), name, clierr.Rancher)
}
