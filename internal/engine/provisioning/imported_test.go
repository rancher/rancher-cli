package provisioning

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIsImported(t *testing.T) {
	t.Parallel()
	obj := func(spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	}
	assert.True(t, IsImported(obj(map[string]any{})), "absent rkeConfig is imported")
	assert.True(t, IsImported(obj(map[string]any{"rkeConfig": nil})), "explicit null rkeConfig is imported")
	assert.False(t, IsImported(obj(map[string]any{"rkeConfig": map[string]any{}})), "empty rkeConfig object is custom, not imported")
	assert.False(t, IsImported(obj(map[string]any{"rkeConfig": map[string]any{"machinePools": []any{}}})), "populated rkeConfig is not imported")
}
