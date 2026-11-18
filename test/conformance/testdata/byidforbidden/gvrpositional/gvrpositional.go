package gvrpositional

import "k8s.io/apimachinery/pkg/runtime/schema"

func projects() schema.GroupVersionResource {
	return schema.GroupVersionResource{"management.cattle.io", "v3", "projects"}
}
