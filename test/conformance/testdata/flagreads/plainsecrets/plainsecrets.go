package plainsecrets

import "github.com/rancher/rancher-cli/internal/runtime"

// Describe declares a Secret flag and a plain flag.
func Describe() runtime.Descriptor {
	return runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "from-file", Type: "stringSlice", Secret: true},
		{Name: "label", Type: "stringSlice"},
	}}
}

// Read reads the plain flag through Secrets.
func Read(in runtime.Input) { _ = in.Secrets("label") }
