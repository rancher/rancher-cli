package authstatusrenamed

import ce "github.com/rancher/rancher-cli/internal/clierr"

func hint() ce.Command { return ce.NewCommand("auth status") }
