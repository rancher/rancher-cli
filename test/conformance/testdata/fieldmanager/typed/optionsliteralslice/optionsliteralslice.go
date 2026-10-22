package optionsliteralslice

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

var _ = []metav1.CreateOptions{{FieldManager: kube.FieldManager}}
