package authstatusclean

import "github.com/rancher/rancher-cli/internal/clierr"

const words clierr.CommandWord = "auth login"

func hints(e *clierr.Error) *clierr.Error {
	_ = clierr.NewCommand(words)
	return e.WithHint(clierr.Rancher + " config status")
}
