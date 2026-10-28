// Reads of the URL fields the scan accepts: the display function's argument,
// the identity function's argument and an assignment target.
package wrapped

import "github.com/rancher/rancher-cli/internal/config"

func Show(entry config.Server, eff config.Effective) (string, string) {
	entry.URL = "https://example.com"
	return config.DisplayServerURL(entry.URL), config.CanonicalServerIdentity(eff.URL)
}
