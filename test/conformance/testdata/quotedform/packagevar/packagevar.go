package packagevar

import (
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var loginCommand = clierr.Rancher + " auth login"

var later string

func setLater() { later = clierr.ShellQuote("x") }

func initializer() string { return strconv.Quote(loginCommand) }

func assigned() string { return fmt.Sprintf("%q", later) }
