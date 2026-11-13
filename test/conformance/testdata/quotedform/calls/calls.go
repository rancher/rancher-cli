package calls

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func trimmed(name string) string { return strconv.Quote(strings.TrimSpace(clierr.ShellQuote(name))) }

func nested() string { return fmt.Sprintf("%q", fmt.Sprintf("%s", clierr.Rancher)) }
