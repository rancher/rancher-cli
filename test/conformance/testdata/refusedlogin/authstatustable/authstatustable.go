package authstatustable

import "github.com/rancher/rancher-cli/internal/clierr"

var table = []struct {
	Name string
	Hint string
}{
	{Name: "a", Hint: clierr.Rancher + " auth status"},
}
