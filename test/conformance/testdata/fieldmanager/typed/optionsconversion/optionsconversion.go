package optionsconversion

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type local metav1.CreateOptions

func convert(l local) metav1.CreateOptions {
	return metav1.CreateOptions(l)
}
