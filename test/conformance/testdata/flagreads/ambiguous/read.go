package ambiguous

import "github.com/rancher/rancher-cli/internal/runtime"

// Read reads set in a file that declares neither: the scan cannot tell which
// command's declaration applies.
func Read(in runtime.Input) { _ = in.Strings("set") }
