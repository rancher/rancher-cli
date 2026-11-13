// Package wordjson is a fixture of the exact-value scans.
package wordjson

import (
	"encoding/json"
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// build decodes command words.
func build(data []byte) clierr.Command {
	var w clierr.CommandWord
	_ = json.Unmarshal(data, &w) // want 4.1
	return clierr.NewCommand(w)
}
