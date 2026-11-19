package authstatusliteral

import "github.com/rancher/rancher-cli/internal/clierr"

func hint() clierr.Command { return clierr.NewCommand("auth status") }
