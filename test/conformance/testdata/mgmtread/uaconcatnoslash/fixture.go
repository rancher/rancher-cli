// A UserAttribute path concatenated with no leading slash.
package uaconcatnoslash

import "github.com/rancher/rancher-cli/internal/engine/steve"

func Path(id string) string {
	return "k8s/clusters/local/v1/" + steve.UserAttributesType + id
}
