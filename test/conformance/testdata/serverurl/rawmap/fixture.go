// Stores an effective URL and a superseded record's server in a map that a
// command returns as result data: the scan reports both reads.
package rawmap

import "github.com/rancher/rancher-cli/internal/config"

func Data(eff config.Effective, rec config.SupersededToken) map[string]any {
	return map[string]any{"url": eff.URL, "server": rec.Server}
}
