package variableformat

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func show(format string, n string) string { return fmt.Sprintf(format, clierr.ShellQuote(n)) }
