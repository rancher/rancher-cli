package optionsfield

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type holder struct {
	create metav1.CreateOptions
}
