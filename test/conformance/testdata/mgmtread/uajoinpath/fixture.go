// A UserAttribute path joined by url.JoinPath with no leading slash.
package uajoinpath

import (
	"net/url"

	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func Path(base *url.URL, id string) *url.URL {
	return base.JoinPath("k8s", "clusters", "local", "v1", steve.UserAttributesType, id)
}
