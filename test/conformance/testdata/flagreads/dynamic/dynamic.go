package dynamic

import "github.com/rancher/rancher-cli/internal/runtime"

// Describe declares a Secret flag and a plain flag.
func Describe() runtime.Descriptor {
	return runtime.Descriptor{Flags: []runtime.ArgSpec{
		{Name: "from-file", Type: "stringSlice", Secret: true},
		{Name: "label", Type: "stringSlice"},
	}}
}

// Read reads by a name that is not a constant.
func Read(in runtime.Input, name string) {
	_ = in.Strings(name)
	_ = in.Secrets(name)
}
