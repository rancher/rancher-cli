package platform

import (
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func quotedOnWindows(name string) string { return strconv.Quote(clierr.ShellQuote(name)) }
