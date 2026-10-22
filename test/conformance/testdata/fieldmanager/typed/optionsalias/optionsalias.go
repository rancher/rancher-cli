package optionsalias

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type updateOptions = metav1.UpdateOptions

var _ = updateOptions{}
