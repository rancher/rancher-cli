package escapes

import "github.com/rancher/rancher-cli/internal/clierr"

func hint() *clierr.Error {
	return clierr.New(clierr.CodeGeneric, "failed").WithHint("\xe2\x81" + "\xa3rancher auth login")
}
