// A UserAttribute path joined by path.Join with no leading slash.
package uapathjoin

import (
	"path"

	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func Path(id string) string {
	return path.Join("k8s/clusters/local/v1", steve.UserAttributesType, id)
}
