package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// setsCreatorIDAllowlist maps the key of a row that sets the creator
// annotation although it is not a Project to the reason. Empty.
var setsCreatorIDAllowlist = map[string]string{}

// TestEveryProjectKindRowSetsTheCreatorID asserts every row that is a
// management.cattle.io/v3 Project has SetsCreatorID, and no other row has it
// without an allowlist reason.
func TestEveryProjectKindRowSetsTheCreatorID(t *testing.T) {
	t.Parallel()
	projects := schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"}
	found := 0
	for _, k := range All() {
		isProject := k.K8sKind == "Project" || k.GVR == projects
		if isProject {
			found++
			assert.True(t, k.SetsCreatorID, "row %q is a Project and must set the creator annotation", k.Key)
			continue
		}
		if k.SetsCreatorID {
			assert.Contains(t, setsCreatorIDAllowlist, k.Key, "row %q sets the creator annotation and is not a Project", k.Key)
		}
	}
	assert.Positive(t, found, "the project row exists")
	for key := range setsCreatorIDAllowlist {
		row, ok := Lookup(key)
		assert.True(t, ok, "allowlist entry %q names no row", key)
		assert.True(t, row.SetsCreatorID, "allowlist entry %q names a row that does not set the annotation", key)
	}
}
