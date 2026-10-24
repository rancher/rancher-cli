package listcontent

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func run(list *unstructured.UnstructuredList) runtime.Result {
	var res runtime.Result
	res.Data = list.Object
	res.Data = list.UnstructuredContent()
	return res
}
