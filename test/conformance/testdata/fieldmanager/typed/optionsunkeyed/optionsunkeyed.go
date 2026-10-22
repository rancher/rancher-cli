package optionsunkeyed

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

var _ = metav1.ApplyOptions{metav1.TypeMeta{}, nil, false, "other"}
