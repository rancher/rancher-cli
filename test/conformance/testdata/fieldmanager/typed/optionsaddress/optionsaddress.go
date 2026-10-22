package optionsaddress

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

var sink any

func wipe() {
	opts := metav1.UpdateOptions{FieldManager: kube.FieldManager}
	p := &opts.FieldManager
	*p = ""
	sink = opts
}
