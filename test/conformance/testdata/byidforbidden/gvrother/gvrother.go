package gvrother

import "k8s.io/apimachinery/pkg/runtime/schema"

var (
	pods   = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	tokens = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "tokens"}
)
