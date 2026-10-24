package content

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func run(created *unstructured.Unstructured) runtime.Result {
	var res runtime.Result
	res.Data = created.UnstructuredContent()
	return res
}
