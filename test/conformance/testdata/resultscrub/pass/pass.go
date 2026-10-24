package pass

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type other struct{ Data any }

// run reads server objects and builds results from names and plain values.
func run(created *unstructured.Unstructured, o *other) runtime.Result {
	res := runtime.Result{Kind: "Thing", Data: map[string]any{"name": created.GetName()}}
	res.Data = map[string]string{"deleted": created.GetName()}
	res.Text = created.GetName()
	o.Data = created.Object
	_ = other{Data: created}
	name, _, _ := unstructured.NestedString(created.Object, "metadata", "name")
	res.Data = name
	res.Data = created.GetAnnotations()["owner"]
	res.Data = created.GetNamespace()
	return res
}
