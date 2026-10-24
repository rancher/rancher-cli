package receivers

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type first struct{}

type second struct{}

// Execute here shows a name only.
func (first) Execute(created *unstructured.Unstructured) runtime.Result {
	return runtime.Result{Data: created.GetName()}
}

// Execute here shows the object.
func (*second) Execute(created *unstructured.Unstructured) runtime.Result {
	return runtime.Result{Data: created.Object}
}
