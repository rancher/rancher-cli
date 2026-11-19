package authstatusconstant

import "github.com/rancher/rancher-cli/internal/clierr"

const words clierr.CommandWord = "auth status"

func hint() clierr.Command { return clierr.NewCommand(words) }
