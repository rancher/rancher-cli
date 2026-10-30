package ioerr

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/runtime"
)

// windowsWrite is in a file that only windows compiles.
func windowsWrite(streams runtime.IO) { _, _ = fmt.Fprint(streams.Err, "x") }

// windowsAssigned is in a file that only windows compiles.
func windowsAssigned(streams runtime.IO) {
	w := streams.Err
	_ = w
}
