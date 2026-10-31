package aliaspairs

import "github.com/rancher/rancher-cli/internal/clierr"

func errors() []error {
	return []error{
		clierr.New(clierr.CodeNotFound, "x").WithHint("get users"),
		clierr.New(clierr.CodeNotFound, "x; use delete tokens"),
		clierr.New(clierr.CodeNotFound, "x").WithHint("get user u-1 -o json"),
	}
}
