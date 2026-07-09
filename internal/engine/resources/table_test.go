package resources

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestTableMarshalJSONSingleInvariant drives the real output.Render path (not
// a direct MarshalJSON call) so a regression here fails the same way it would
// in production: get.go always passes exactly 1 item for a named get, so this
// is unreached today, but a single result with 0 or 2+ items must error
// loudly rather than silently render null or drop items.
func TestTableMarshalJSONSingleInvariant(t *testing.T) {
	k := mustKind(t, "cluster")
	tests := []struct {
		name  string
		items []unstructured.Unstructured
	}{
		{"zero items", nil},
		{"two items", []unstructured.Unstructured{
			clusterObj("a", "a", "active"),
			clusterObj("b", "b", "active"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := output.NewEnvelope("Cluster", newTable(k, tt.items, true))
			err := output.Render(&buf, "json", env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("got %d", len(tt.items)))
		})
	}
}
