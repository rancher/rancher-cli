package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDescriptionPathsAreTheHandleLookupPaths asserts, for every row, that
// the description paths are the Handle.Lookup paths in Lookup order, and
// that a row without a Handle has none.
func TestDescriptionPathsAreTheHandleLookupPaths(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		got := k.DescriptionPaths()
		if k.Handle == nil {
			assert.Nil(t, got, k.Name)
			continue
		}
		want := make([][]string, 0, len(k.Handle.Lookup))
		for _, f := range k.Handle.Lookup {
			want = append(want, f.Path)
		}
		assert.Equal(t, want, got, k.Name)
	}
}
