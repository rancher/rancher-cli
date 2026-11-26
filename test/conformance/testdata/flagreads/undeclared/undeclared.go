package undeclared

import "github.com/rancher/rancher-cli/internal/runtime"

// Read reads a name that no file marks Secret through Secrets.
func Read(in runtime.Input) { _ = in.Secrets("nobody-marks-this") }
