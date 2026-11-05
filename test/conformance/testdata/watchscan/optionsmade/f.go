package optionsmade

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

func options() metav1.ListOptions {
	return metav1.ListOptions{Watch: true}
}
