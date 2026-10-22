package optionstypearg

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func zeroOf[T any]() T {
	var v T
	return v
}

var _ = zeroOf[metav1.CreateOptions]()
