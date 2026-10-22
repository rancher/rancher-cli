package optionsother

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

var _ = metav1.CreateOptions{FieldManager: "kubectl"}
