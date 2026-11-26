package resources

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
)

// TestReadCommandIsTheGetCommandOfEveryKind asserts, for every kind of the
// table, that kinds.ReadCommand gives the get command that deleteRecovery
// gives for the same object, and that with no name it gives the same command
// without the name. The delete of an NSCluster kind is routed by the
// namespace, which is a cluster id.
func TestReadCommandIsTheGetCommandOfEveryKind(t *testing.T) {
	t.Parallel()
	for _, k := range kinds.All() {
		ns := ""
		if k.Policy != kinds.NSNone {
			ns = "ns-1"
		}
		effect := "c-1"
		if k.Policy == kinds.NSCluster {
			effect = ns
		}
		want := deleteRecovery(k, ns, "obj-1", effect).Field()
		assert.Equal(t, want, kinds.ReadCommand(k, ns, "obj-1", "c-1").Field(), k.Key)
		assert.Equal(t, strings.Replace(want, " 'obj-1'", "", 1), kinds.ReadCommand(k, ns, "", "c-1").Field(), k.Key)
	}
}
