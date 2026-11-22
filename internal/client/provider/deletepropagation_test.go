package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// deleteRequest is one DELETE a test server received.
type deleteRequest struct {
	path    string
	rawBody string
	options metav1.DeleteOptions
}

// propagationCall is one call of a method that sends a DELETE: Delete with
// or without a resourceVersion precondition, or DeleteWithUID with or without
// a uid precondition.
type propagationCall struct {
	// name is the call in the subtest name, after the kind.
	name string
	// field is the precondition field the call fills.
	field string
	// precondition is the value the call sends; "" sends none.
	precondition string
	call         func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, ns string, dryRun bool) error
	// sent is the precondition the server read from the body.
	sent func(*metav1.Preconditions) *string
}

// propagationCalls are the calls of Delete and DeleteWithUID, each with and
// without its precondition.
func propagationCalls() []propagationCall {
	var calls []propagationCall
	for _, precondition := range []string{"", "7"} {
		calls = append(calls, propagationCall{"", "resourceVersion", precondition, func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, ns string, dryRun bool) error {
			return c.Delete(ctx, gvr, ns, "x", precondition, dryRun)
		}, func(p *metav1.Preconditions) *string { return p.ResourceVersion }})
	}
	for _, precondition := range []string{"", "u-7"} {
		calls = append(calls, propagationCall{" with DeleteWithUID", "uid", precondition, func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource, ns string, dryRun bool) error {
			d, ok := c.(engine.UIDDeleter)
			if !ok {
				return fmt.Errorf("%T does not delete with a uid precondition", c)
			}
			_, err := d.DeleteWithUID(ctx, gvr, ns, "x", precondition, dryRun)
			return err
		}, func(p *metav1.Preconditions) *string {
			if p.UID == nil {
				return nil
			}
			uid := string(*p.UID)
			return &uid
		}})
	}
	return calls
}

// TestEveryClusterClientDeleteSendsTheBackgroundPropagationPolicy asserts
// the DELETE of a namespaced kind, of a cluster-scoped kind and of a kind
// whose version is found by discovery carries propagationPolicy Background in
// its DeleteOptions, sent by Delete or by DeleteWithUID: as a dry-run preview
// with or without a precondition, and with a precondition (a resourceVersion
// for Delete, a uid for DeleteWithUID) when it is not one. The dry-run value
// and the precondition stay in the same body. A delete that is not a dry run
// and has no precondition sends no request
// (TestEveryDeleteIsBoundByAPrecondition). Without a policy the server
// applies the default for that kind, which keeps the pods of a Job. Building
// the DeleteOptions of either method without PropagationPolicy fails its
// rows.
func TestEveryClusterClientDeleteSendsTheBackgroundPropagationPolicy(t *testing.T) {
	t.Parallel()
	kinds := []struct {
		name      string
		gvr       schema.GroupVersionResource
		namespace string
		path      string
	}{
		{"a namespaced kind", schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "default", "/apis/batch/v1/namespaces/default/jobs/x"},
		{"a cluster-scoped kind", schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "/api/v1/namespaces/x"},
		{"a kind whose version is found by discovery", schema.GroupVersionResource{Group: "ext.cattle.io", Resource: "tokens"}, "", "/apis/ext.cattle.io/v1/tokens/x"},
	}
	for _, kind := range kinds {
		for _, dryRun := range []bool{false, true} {
			for _, call := range propagationCalls() {
				if !dryRun && call.precondition == "" {
					continue
				}
				name := kind.name + call.name
				if dryRun {
					name += " as a dry-run preview"
				}
				if call.precondition != "" {
					name += " with a " + call.field
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					var (
						mu       sync.Mutex
						requests []deleteRequest
					)
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						assert.NoError(t, err)
						if r.Method == http.MethodGet && r.URL.Path == "/apis" {
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"ext.cattle.io","versions":[{"groupVersion":"ext.cattle.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"ext.cattle.io/v1","version":"v1"}}]}`))
							return
						}
						if r.Method == http.MethodDelete {
							got := deleteRequest{path: r.URL.Path, rawBody: string(body)}
							assert.NoError(t, json.Unmarshal(body, &got.options), "the body of the DELETE is not DeleteOptions JSON: %s", body)
							mu.Lock()
							requests = append(requests, got)
							mu.Unlock()
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
					}))
					t.Cleanup(srv.Close)
					c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
					require.NoError(t, err)
					require.NoError(t, call.call(t.Context(), c, kind.gvr, kind.namespace, dryRun))
					mu.Lock()
					defer mu.Unlock()
					require.Len(t, requests, 1)
					got := requests[0]
					assert.Equal(t, kind.path, got.path)
					require.NotNil(t, got.options.PropagationPolicy, "no propagationPolicy in %s", got.rawBody)
					assert.Equal(t, metav1.DeletePropagationBackground, *got.options.PropagationPolicy)
					assert.Equal(t, dryRun, len(got.options.DryRun) == 1 && got.options.DryRun[0] == metav1.DryRunAll, "the dry-run value of the DELETE body %s", got.rawBody)
					if call.precondition == "" {
						assert.Nil(t, got.options.Preconditions)
						return
					}
					require.NotNil(t, got.options.Preconditions)
					sent := call.sent(got.options.Preconditions)
					require.NotNil(t, sent, "no %s precondition in %s", call.field, got.rawBody)
					assert.Equal(t, call.precondition, *sent)
				})
			}
		}
	}
}
