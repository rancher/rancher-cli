package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// apiServerStatusBody is se as the JSON body the API server writes for it.
func apiServerStatusBody(t *testing.T, se *apierrors.StatusError) []byte {
	t.Helper()
	st := se.ErrStatus
	st.Kind, st.APIVersion = "Status", "v1"
	b, err := json.Marshal(st)
	require.NoError(t, err)
	return b
}

// TestTheClusterClientErrorsKeepTheAuthorizersRefusalAndTheUIDConflict
// asserts, for the 403 the API server writes when its authorizer refuses a
// GET and the 409 it writes when a uid precondition is not the object's,
// both sent through the cluster client, that clierr.AuthorizerRefused is true
// on the error of Get and clierr.UIDConflict on the error of DeleteWithUID:
// for a ConfigMap in a namespace, the same with a reason after the refusal, a
// user name that makes the message longer than 200 bytes, a namespace, a
// ClusterRole at the cluster scope and a project in its cluster namespace.
func TestTheClusterClientErrorsKeepTheAuthorizersRefusalAndTheUIDConflict(t *testing.T) {
	t.Parallel()
	configMaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	for _, tc := range []struct {
		name    string
		gvr     schema.GroupVersionResource
		ns, obj string
		// scope is the authorizer's words for where the request was sent.
		scope  string
		reason string
		user   string
	}{
		{"a ConfigMap in a namespace", configMaps, "ns1", "obj1", ` in the namespace "ns1"`, "", "u-delete"},
		{"a ConfigMap with a reason after the refusal", configMaps, "ns1", "obj1", ` in the namespace "ns1"`,
			`RBAC: clusterrole.rbac.authorization.k8s.io "gone" not found`, "u-delete"},
		{"a message longer than 200 bytes", configMaps, "ns1", "obj1", ` in the namespace "ns1"`, "", strings.Repeat("x", 220)},
		{"a namespace", schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "obj1", ` in the namespace "obj1"`, "", "u-delete"},
		{"a ClusterRole at the cluster scope", schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			"", "obj1", " at the cluster scope", "", "u-delete"},
		{"a project in its cluster namespace", schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"},
			"c-abc", "p-1", ` in the namespace "c-abc"`, "", "u-delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gr := schema.GroupResource{Group: tc.gvr.Group, Resource: tc.gvr.Resource}
			msg := fmt.Sprintf("User %q cannot get resource %q in API group %q%s", tc.user, tc.gvr.Resource, tc.gvr.Group, tc.scope)
			if tc.reason != "" {
				msg += ": " + tc.reason
			}
			forbidden := apiServerStatusBody(t, apierrors.NewForbidden(gr, tc.obj, errors.New(msg)))
			conflict := apiServerStatusBody(t, apierrors.NewConflict(gr, tc.obj,
				errors.New("the UID in the precondition (u1) does not match the UID in record (u2). The object might have been deleted and then recreated")))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write(forbidden)
				case http.MethodDelete:
					w.WriteHeader(http.StatusConflict)
					_, _ = w.Write(conflict)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(srv.Close)
			c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
			require.NoError(t, err)

			_, err = c.Get(t.Context(), tc.gvr, tc.ns, tc.obj)
			require.Error(t, err)
			assert.True(t, clierr.AuthorizerRefused(err, "get", tc.gvr.Group, tc.gvr.Resource, tc.ns, tc.obj), "Get: %v", err)

			d, ok := c.(engine.UIDDeleter)
			require.True(t, ok, "%T implements engine.UIDDeleter", c)
			_, err = d.DeleteWithUID(t.Context(), tc.gvr, tc.ns, tc.obj, "u1", false)
			require.Error(t, err)
			assert.True(t, clierr.UIDConflict(err), "DeleteWithUID: %v", err)
		})
	}
}
