package optionsvar

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func zero() metav1.UpdateOptions {
	var opts metav1.UpdateOptions
	return opts
}
