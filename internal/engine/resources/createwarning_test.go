package resources

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createWarningManifest returns a minimal manifest of row k and the input
// whose routing the row's namespace policy and scope need. A day-2 operation
// row gets the manifest its object checks accept: the namespace is the id of
// the cluster that spec.clusterRef names, spec.ttl is 60, and a restore names an
// etcd snapshot. The checks that read the server do not run here.
func createWarningManifest(k kinds.Kind) ([]byte, input) {
	apiVersion := k.GVR.Version
	if k.GVR.Group != "" {
		apiVersion = k.GVR.Group + "/" + k.GVR.Version
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: %s\nkind: %s\nmetadata:\n  name: x\n", apiVersion, k.K8sKind)
	in := input{s: map[string]string{"filename": "m.yaml"}}
	dayTwo := k.GVR.Group == "operation.cattle.io"
	if k.Policy != kinds.NSNone {
		ns := "ns1"
		if k.DefaultNamespace != "" {
			ns = k.DefaultNamespace
		}
		if dayTwo {
			ns = "c-abc"
		}
		fmt.Fprintf(&b, "  namespace: %s\n", ns)
	}
	if dayTwo {
		b.WriteString("spec:\n  ttl: 60\n  clusterRef:\n    apiVersion: management.cattle.io/v3\n    kind: Cluster\n    name: c-abc\n")
		if k.Key == "etcdsnapshotrestore" {
			b.WriteString("  args:\n    name: c-abc-local-etcd-snapshot\n")
		}
	}
	if k.Scope == kinds.Downstream {
		in.s["cluster"] = "prod"
	}
	return []byte(b.String()), in
}

// TestCreateWarningRowsAddItToASuccessfulPreviewOnly asserts, for every row
// with a CreateWarning, that the Effect of a create resource of a minimal
// manifest of the kind carries the warning (and is Destructive for a
// Sensitive kind), and that a create whose dry-run the server refuses returns
// an error that does not carry it. It also asserts the managedchart row is
// among the rows that set one.
func TestCreateWarningRowsAddItToASuccessfulPreviewOnly(t *testing.T) {
	t.Parallel()
	var keys []string
	for _, k := range kinds.All() {
		if k.CreateWarning == "" {
			continue
		}
		keys = append(keys, k.Key)
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			manifest, in := createWarningManifest(k)

			client := &createClient{}
			fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
			eff, _, err := planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": manifest}), in)
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, k.CreateWarning)
			assert.Equal(t, k.Sensitive, eff.Destructive)

			refused := &createClient{createErr: clierr.New(clierr.CodeValidation, "the server refused the object")}
			fac = &recFactory{client: refused, resolve: map[string]string{"prod": "c-abc"}}
			_, _, err = planCreate(t, NewCreate(fac, mapFileSource{"m.yaml": manifest}), in)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "want a clierr.Error, got %v", err)
			assert.NotContains(t, ce.Warnings(), k.CreateWarning)
		})
	}
	assert.Contains(t, keys, "managedchart")
}
