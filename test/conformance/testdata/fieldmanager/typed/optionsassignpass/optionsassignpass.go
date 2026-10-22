package optionsassignpass

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

var sink any

func build(dryRun bool) {
	opts := metav1.UpdateOptions{FieldManager: kube.FieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	opts.FieldManager = kube.FieldManager
	opts.FieldManager = "cli.rancher.io"
	sink = opts
}
