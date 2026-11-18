package lookupnoget

import "github.com/rancher/rancher-cli/internal/engine/kinds"

func plan() bool {
	_, ok := kinds.Lookup("project")
	return ok
}
