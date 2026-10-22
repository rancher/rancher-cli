package optionsrange

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

var sink any

func wipe(names map[string]int) {
	opts := metav1.UpdateOptions{FieldManager: kube.FieldManager}
	for opts.FieldManager = range names {
	}
	sink = opts
}
