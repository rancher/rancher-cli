package provisioning

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// controlPlaneClient returns one object for a Get and records what it was asked for.
type controlPlaneClient struct {
	engine.ClusterClient
	obj *unstructured.Unstructured
	err error

	gvr    schema.GroupVersionResource
	ns, nm string
}

func (c *controlPlaneClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	c.gvr, c.ns, c.nm = gvr, ns, name
	return c.obj, c.err
}

// TestFetchControlPlane asserts the read asks for the RKEControlPlane of the
// cluster's namespace and name and returns the object, and that a 404, a 403
// and another error come back unchanged for the caller to decide.
func TestFetchControlPlane(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "prod"}}}
	client := &controlPlaneClient{obj: obj}
	got, err := fetchControlPlane(t.Context(), client, "fleet-default", "prod")
	require.NoError(t, err)
	assert.Same(t, obj, got)
	assert.Equal(t, rkeControlPlaneGVR, client.gvr)
	assert.Equal(t, "rke.cattle.io", client.gvr.Group)
	assert.Equal(t, "rkecontrolplanes", client.gvr.Resource)
	assert.Equal(t, "fleet-default", client.ns)
	assert.Equal(t, "prod", client.nm)

	for name, readErr := range map[string]error{
		"not found": clierr.New(clierr.CodeNotFound, "missing"),
		"forbidden": clierr.New(clierr.CodeRBAC, "forbidden"),
		"other":     errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := fetchControlPlane(t.Context(), &controlPlaneClient{err: readErr}, "fleet-default", "prod")
			assert.Nil(t, got)
			assert.Same(t, readErr, err)
		})
	}
}
