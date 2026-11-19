package authstatusconcat

import "github.com/rancher/rancher-cli/internal/clierr"

func hint(e *clierr.Error) *clierr.Error { return e.WithHint(clierr.Rancher + " auth status") }
