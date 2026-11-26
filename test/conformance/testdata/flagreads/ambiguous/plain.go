package ambiguous

import "github.com/rancher/rancher-cli/internal/runtime"

// DescribePlain declares set as a plain flag.
func DescribePlain() runtime.Descriptor {
	return runtime.Descriptor{Flags: []runtime.ArgSpec{{Name: "set", Type: "stringSlice"}}}
}
