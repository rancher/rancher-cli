package kinds

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOnlyClusterAndProjectRowsReadThroughSteve asserts the rows that set
// SteveReadOnForbidden are exactly the cluster and project rows, the two
// that have a Steve reader for a get a Kubernetes 403 refused.
func TestOnlyClusterAndProjectRowsReadThroughSteve(t *testing.T) {
	t.Parallel()
	want := []string{"managementcluster", "project"}
	var got []string
	for _, k := range All() {
		if k.SteveReadOnForbidden {
			got = append(got, k.Key)
		}
	}
	slices.Sort(got)
	for _, key := range want {
		assert.Contains(t, got, key, "row %s must set SteveReadOnForbidden: it has a Steve reader", key)
	}
	for _, key := range got {
		assert.Contains(t, want, key, "row %s sets SteveReadOnForbidden but has no Steve reader; a 403 on its get is a real refusal", key)
	}
}
