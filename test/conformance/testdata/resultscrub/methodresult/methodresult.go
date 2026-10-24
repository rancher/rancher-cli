package methodresult

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func raw(b []byte, err error) any {
	if err != nil {
		return nil
	}
	return b
}

func run(created *unstructured.Unstructured) runtime.Result {
	var res runtime.Result
	res.Data = map[string]any{"a": created.GetAnnotations()}
	res.Data = map[string]any{"l": created.GetLabels()}
	res.Data = raw(created.MarshalJSON())
	res.Data = created.DeepCopyObject()
	return res
}
