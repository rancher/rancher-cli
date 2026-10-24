package nestedliteral

import (
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func run(created *unstructured.Unstructured) runtime.Result {
	return runtime.Result{Data: map[string]any{"o": created.Object}}
}
