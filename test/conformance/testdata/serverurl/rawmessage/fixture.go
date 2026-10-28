// Reads a stored server URL into a message: the scan reports it.
package rawmessage

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/config"
)

func Message(entry config.Server) string {
	return fmt.Sprintf("registered at %s", entry.URL)
}
