package gvrliteral

import "k8s.io/apimachinery/pkg/runtime/schema"

var clusters = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}
