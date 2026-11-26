package ambiguous

import "github.com/rancher/rancher-cli/internal/runtime"

// DescribeSecret declares set as a Secret flag.
func DescribeSecret() runtime.Descriptor {
	return runtime.Descriptor{Flags: []runtime.ArgSpec{{Name: "set", Type: "stringSlice", Secret: true}}}
}
