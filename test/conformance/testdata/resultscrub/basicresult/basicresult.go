package basicresult

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func nameOf(obj *unstructured.Unstructured) string { return obj.GetName() }

func count(obj *unstructured.Unstructured) int { return len(obj.Object) }

// run puts a value in Data that is built from an object but is a string,
// a number or an assertion to one of them.
func run(created *unstructured.Unstructured) runtime.Result {
	var res runtime.Result
	res.Data = map[string]any{"name": nameOf(created), "n": count(created)}
	res.Data = created.Object["description"].(string)
	res.Data = []any{created.Object["enabled"].(bool)}
	return res
}
