package literal

import "github.com/rancher/rancher-cli/internal/clierr"

func hint() *clierr.Error {
	return clierr.New(clierr.CodeGeneric, "failed").WithHint("\u2063rancher auth login")
}
