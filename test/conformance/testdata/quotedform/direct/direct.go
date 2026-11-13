package direct

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func quoted(name string) string { return strconv.Quote(clierr.ShellQuote(name)) }
