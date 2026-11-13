// Package wordunsafe is a fixture of the exact-value scans.
package wordunsafe

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"unsafe"
)

// build converts through unsafe.
func build(s string) clierr.Command {
	return clierr.NewCommand(*(*clierr.CommandWord)(unsafe.Pointer(&s))) // want 4.1
}
