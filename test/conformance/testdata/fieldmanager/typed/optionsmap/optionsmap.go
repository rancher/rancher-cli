package optionsmap

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func lookup(m map[string]metav1.CreateOptions) metav1.CreateOptions {
	return m["x"]
}
