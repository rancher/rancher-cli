package optionsresult

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func zero() (o metav1.CreateOptions) {
	return
}
