package localassign

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func viaDefine(created *unstructured.Unstructured) runtime.Result {
	d := created.Object
	var res runtime.Result
	res.Data = d
	return res
}

func viaAssign(created *unstructured.Unstructured) runtime.Result {
	var d map[string]any
	d = created.Object
	return runtime.Result{Data: d}
}

func viaVar(created *unstructured.Unstructured) runtime.Result {
	var d = created.UnstructuredContent()
	return runtime.Result{Data: d}
}

func viaIndex(created *unstructured.Unstructured) runtime.Result {
	d := map[string]any{"name": created.GetName()}
	d["object"] = created.Object
	return runtime.Result{Data: d}
}

func viaCall(created *unstructured.Unstructured) runtime.Result {
	b, _ := created.MarshalJSON()
	return runtime.Result{Data: b}
}

func viaChain(created *unstructured.Unstructured) runtime.Result {
	d := created.Object
	e := d
	var res runtime.Result
	res.Data = e
	return res
}

func viaLoop(created *unstructured.Unstructured) runtime.Result {
	a := map[string]any{}
	b := a
	a = b
	b = created.Object
	return runtime.Result{Data: a}
}

func viaTwoSites(created *unstructured.Unstructured) runtime.Result {
	d := map[string]any{}
	var res runtime.Result
	res.Data = d
	d["o"] = created.Object
	res.Data = d
	return res
}

func viaCommaOk(created *unstructured.Unstructured) runtime.Result {
	m, _ := created.Object["m"].(map[string]any)
	return runtime.Result{Data: m}
}
