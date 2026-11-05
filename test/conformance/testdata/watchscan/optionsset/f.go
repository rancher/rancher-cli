package optionsset

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func options() metav1.ListOptions {
	var o metav1.ListOptions
	o.Watch = true
	return o
}
