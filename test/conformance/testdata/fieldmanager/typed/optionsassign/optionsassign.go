package optionsassign

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

var manager = "cli.rancher.io"

func wipe() {
	opts := metav1.UpdateOptions{FieldManager: kube.FieldManager}
	opts.FieldManager = ""
	opts.FieldManager = manager
	opts.FieldManager += "x"
	p := &opts
	p.FieldManager = "other"
}

func multi() {
	opts := metav1.CreateOptions{FieldManager: kube.FieldManager}
	var name string
	opts.FieldManager, name = manager, "x"
	_ = name
}
