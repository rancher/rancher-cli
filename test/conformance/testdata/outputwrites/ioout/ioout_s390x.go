package ioout

import "github.com/rancher/rancher-cli/internal/runtime"

// s390xWrite is in a file that only s390x compiles.
func s390xWrite(streams runtime.IO) { _, _ = streams.Out.Write(nil) }
