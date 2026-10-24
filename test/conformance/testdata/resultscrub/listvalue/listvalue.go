package listvalue

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func run(list *unstructured.UnstructuredList, plain unstructured.UnstructuredList) runtime.Result {
	var res runtime.Result
	res.Data = list
	res.Data = plain
	return runtime.Result{Data: list}
}
