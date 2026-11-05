package exemptedfile

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/tools/watch"
)

func open(ctx context.Context, d dynamic.Interface, gvr schema.GroupVersionResource) {
	_, _ = d.Resource(gvr).Watch(ctx, metav1.ListOptions{})
}
