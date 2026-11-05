package methodvalue

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func open(ctx context.Context, d dynamic.Interface, gvr schema.GroupVersionResource) {
	watch := d.Resource(gvr).Watch
	_, _ = watch(ctx, metav1.ListOptions{})
}
