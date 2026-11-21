package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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

// TestEveryDeleteTheClusterClientSendsStatesTheBackgroundPropagationPolicy
// asserts that the DELETE of every namespaced and cluster-scoped kind, as a
// dry run or not and with or without a resourceVersion, carries the
// propagationPolicy Background in its DeleteOptions, next to the dry run and
// the resourceVersion precondition it already sent. Without a policy the
// server applies the default of the kind, which keeps the pods of a Job.
// Building the DeleteOptions without PropagationPolicy fails every row.
func TestEveryDeleteTheClusterClientSendsStatesTheBackgroundPropagationPolicy(t *testing.T) {
	t.Parallel()
	kinds := []struct {
		name      string
		gvr       schema.GroupVersionResource
		namespace string
		path      string
	}{
		{"a namespaced kind", schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, "default", "/apis/batch/v1/namespaces/default/jobs/x"},
		{"a cluster-scoped kind", schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "/api/v1/namespaces/x"},
	}
	for _, kind := range kinds {
		for _, dryRun := range []bool{false, true} {
			for _, resourceVersion := range []string{"", "7"} {
				name := kind.name
				if dryRun {
					name += " as a dry run"
				}
				if resourceVersion != "" {
					name += " with a resourceVersion"
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
						if r.Method == http.MethodDelete {
							got := deleteRequest{path: r.URL.Path, rawBody: string(body)}
							assert.NoError(t, json.Unmarshal(body, &got.options), "the body is DeleteOptions: %s", body)
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
					require.NoError(t, c.Delete(t.Context(), kind.gvr, kind.namespace, "x", resourceVersion, dryRun))
					mu.Lock()
					defer mu.Unlock()
					require.Len(t, requests, 1)
					got := requests[0]
					assert.Equal(t, kind.path, got.path)
					require.NotNil(t, got.options.PropagationPolicy, "no propagationPolicy in %s", got.rawBody)
					assert.Equal(t, metav1.DeletePropagationBackground, *got.options.PropagationPolicy)
					assert.Equal(t, dryRun, len(got.options.DryRun) == 1 && got.options.DryRun[0] == metav1.DryRunAll)
					if resourceVersion == "" {
						assert.Nil(t, got.options.Preconditions)
						return
					}
					require.NotNil(t, got.options.Preconditions)
					require.NotNil(t, got.options.Preconditions.ResourceVersion)
					assert.Equal(t, resourceVersion, *got.options.Preconditions.ResourceVersion)
				})
			}
		}
	}
}
