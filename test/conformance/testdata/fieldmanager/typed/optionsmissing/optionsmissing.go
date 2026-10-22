package optionsmissing

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

var (
	_ = metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	_ = metav1.UpdateOptions{}
)
