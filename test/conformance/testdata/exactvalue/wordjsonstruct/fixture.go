// Package wordjsonstruct is a fixture of the exact-value scans.
package wordjsonstruct

import (
	"encoding/json"
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// words is a struct that holds a command word.
type words struct {
	Verb clierr.CommandWord
}

// build decodes a struct that holds command words.
func build(data []byte) clierr.Command {
	var v words
	_ = json.Unmarshal(data, &v) // want 4.1
	return clierr.NewCommand(v.Verb)
}
