package provider

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	conflictWebhookMessage = `admission webhook "x.example.io" denied the request: duplicate not allowed`
	conflictStaleText      = "the resource changed on the server since it was last read; retry the command"
)

// conflictCall is one call of a client method and whether it sends a
// resourceVersion precondition.
type conflictCall struct {
	name         string
	precondition bool
	call         func(context.Context, engine.ClusterClient, schema.GroupVersionResource) error
}

// conflictCalls has a row for every ClusterClient method; Update and Delete
// have one with and one without a resourceVersion.
var conflictCalls = []conflictCall{
	{"Get", false, clusterClientCalls["Get"]},
	{"List", false, clusterClientCalls["List"]},
	{"Create", false, clusterClientCalls["Create"]},
	{"Update with a resourceVersion", true, func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		obj := guardObject(gvr)
		obj.SetResourceVersion("7")
		_, err := c.Update(ctx, gvr, "default", obj, false)
		return err
	}},
	{"Update with no resourceVersion", false, clusterClientCalls["Update"]},
	{"Delete with a resourceVersion", true, func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		return c.Delete(ctx, gvr, "default", "x", "7", false)
	}},
	{"Delete with an empty resourceVersion", false, clusterClientCalls["Delete"]},
	{"Logs", false, clusterClientCalls["Logs"]},
	{"PostReview", false, clusterClientCalls["PostReview"]},
	{"ServerResources", false, clusterClientCalls["ServerResources"]},
	{"ListObject", false, func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		w, ok := c.(engine.ObjectWatcher)
		if !ok {
			return errors.New("the client does not list one object")
		}
		_, _, err := w.ListObject(ctx, gvr, "default", "x")
		return err
	}},
}

// TestEveryClusterClientWriteSendsAPreconditionOnlyWithAResourceVersion
// asserts a ClusterClient call that gets a 409 returns the retryable message
// that the resource changed since it was last read only for an update whose
// object carries a resourceVersion and a delete given one; every other call
// returns a CONFLICT that is not retryable and keeps the server's message. A
// method added to the interface fails here until it has a row.
func TestEveryClusterClientWriteSendsAPreconditionOnlyWithAResourceVersion(t *testing.T) {
	t.Parallel()
	it := reflect.TypeFor[engine.ClusterClient]()
	for m := range it.Methods() {
		found := slices.ContainsFunc(conflictCalls, func(c conflictCall) bool {
			method, _, _ := strings.Cut(c.name, " ")
			return method == m.Name
		})
		require.True(t, found, "every ClusterClient method has a row: %s", m.Name)
	}
	srv := refusingServer(t, refusal{
		status: http.StatusConflict, contentType: "application/json",
		body: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"` +
			`admission webhook \"x.example.io\" denied the request: duplicate not allowed","reason":"Conflict","code":409}`,
	})
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for _, tc := range conflictCalls {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call(t.Context(), c, gvr)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			if tc.precondition {
				assert.True(t, ce.Retryable)
				assert.Equal(t, conflictStaleText, ce.Message)
				return
			}
			assert.False(t, ce.Retryable)
			assert.Contains(t, ce.Message, conflictWebhookMessage)
		})
	}
}
