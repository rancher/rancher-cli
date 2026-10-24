package items

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func run(list *unstructured.UnstructuredList) runtime.Result {
	return runtime.Result{Data: list.Items}
}
