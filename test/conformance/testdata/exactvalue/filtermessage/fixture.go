// Package filtermessage is a fixture of the exact-value scans.
package filtermessage

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// relayed filters a relayed error message, which can hold a command.
func relayed(err *clierr.Error) string {
	msg := "failed: " + err.Message
	return output.Printable(msg) // want 3b
}
