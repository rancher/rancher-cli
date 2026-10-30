package ioout

import "github.com/rancher/rancher-cli/internal/runtime"

// windowsWrite is in a file that only windows compiles.
func windowsWrite(streams runtime.IO) { _, _ = streams.Out.Write(nil) }
