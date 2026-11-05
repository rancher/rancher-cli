package calldynamic

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func open(ctx context.Context, d dynamic.Interface, gvr schema.GroupVersionResource) {
	_, _ = d.Resource(gvr).Watch(ctx, metav1.ListOptions{})
}
