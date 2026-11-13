package localvar

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func quoted(name string) string {
	cmd := clierr.ShellQuote(name)
	return fmt.Sprintf("run %q", cmd)
}
