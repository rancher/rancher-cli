package namebuilder

import "github.com/rancher/rancher-cli/internal/clierr"

func errors(id string) []error {
	return []error{
		clierr.New(clierr.CodeNotFound, "get users"),
		clierr.New(clierr.CodeNotFound, "x").WithHint("access clusters"),
		clierr.New(clierr.CodeNotFound, "x").WithHint(clierr.Rancher + " access clusters"),
	}
}

var step = probe{Name: "delete user " + "x"}
