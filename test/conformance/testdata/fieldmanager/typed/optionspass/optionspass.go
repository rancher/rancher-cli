package optionspass

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

const manager = "cli.rancher.io"

type named string

const typed named = "cli.rancher.io"

var (
	_ = metav1.CreateOptions{FieldManager: kube.FieldManager}
	_ = metav1.UpdateOptions{FieldManager: "cli.rancher.io", DryRun: []string{metav1.DryRunAll}}
	_ = metav1.PatchOptions{FieldManager: manager}
	_ = metav1.ApplyOptions{FieldManager: string(typed), Force: true}
	_ = metav1.ApplyOptions{metav1.TypeMeta{}, nil, false, "cli.rancher.io"}
	_ = metav1.DeleteOptions{}
)
