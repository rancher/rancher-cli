package calltyped

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func open(ctx context.Context, cs kubernetes.Interface) {
	_, _ = cs.CoreV1().Pods("ns").Watch(ctx, metav1.ListOptions{})
}
