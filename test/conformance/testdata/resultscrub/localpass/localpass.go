package localpass

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type holder struct{ f any }

// build holds results made from names and basic values, a chain of locals
// that ends in a basic-type local (at the first local, and after a local of
// interface type), and a field assignment, which the rule does not follow.
func build(created *unstructured.Unstructured, param map[string]any) runtime.Result {
	name, _, _ := unstructured.NestedString(created.Object, "metadata", "name")
	data := map[string]any{"user": map[string]any{"name": created.GetName()}}
	data["secret"] = created.GetName()
	data["owner"] = created.GetAnnotations()["owner"]
	_ = name
	res := runtime.Result{Data: data}
	res.Data = param
	h := holder{}
	h.f = created.Object
	res.Data = h
	alias := name
	again := alias
	res.Data = again
	var boxed any = name
	later := boxed
	res.Data = later
	return res
}
