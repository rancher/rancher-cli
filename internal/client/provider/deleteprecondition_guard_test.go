package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// boundDelete is one request a deleteServer received.
type boundDelete struct {
	method, path string
	body         map[string]any
}

// deleteServer records every request and responds to each with status and body.
type deleteServer struct {
	mu     sync.Mutex
	got    []boundDelete
	status int
	body   string
}

// newDeleteServer starts a server that responds to every request with status and
// body and records each request.
func newDeleteServer(t *testing.T, status int, body string) (*deleteServer, engine.ClusterClient) {
	t.Helper()
	s := &deleteServer{status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := boundDelete{method: r.Method, path: r.URL.Path}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &req.body)
		}
		s.mu.Lock()
		s.got = append(s.got, req)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
	require.NoError(t, err)
	return s, c
}

func (s *deleteServer) requests() []boundDelete {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]boundDelete(nil), s.got...)
}

// deleteCall is one way of sending a delete: its method, the precondition
// field its precondition argument fills, and the call.
type deleteCall struct {
	field string
	call  func(ctx context.Context, c engine.ClusterClient, precondition string, dryRun bool) error
}

// deleteCalls has a row for every method whose name starts with Delete on
// engine.ClusterClient and engine.UIDDeleter.
var deleteCalls = map[string]deleteCall{
	"Delete": {"resourceVersion", func(ctx context.Context, c engine.ClusterClient, precondition string, dryRun bool) error {
		return c.Delete(ctx, deleteGuardGVR, "default", "x", precondition, dryRun)
	}},
	"DeleteWithUID": {"uid", func(ctx context.Context, c engine.ClusterClient, precondition string, dryRun bool) error {
		d, ok := c.(engine.UIDDeleter)
		if !ok {
			return errors.New("the client does not delete with a uid precondition")
		}
		_, err := d.DeleteWithUID(ctx, deleteGuardGVR, "default", "x", precondition, dryRun)
		return err
	}},
}

var deleteGuardGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// deleteStatus is the Status the API server returns for a delete.
const deleteStatus = `{"kind":"Status","apiVersion":"v1","status":"Success","details":{"name":"x","kind":"configmaps","uid":"u-status"}}`

// TestEveryDeleteIsBoundByAPrecondition asserts, for every method whose name
// starts with Delete on engine.ClusterClient and engine.UIDDeleter, found by
// reflection: a delete that is not a dry run with an empty precondition sends
// no request and returns an internal error; one with a precondition sends
// exactly that precondition in DeleteOptions and no dryRun; a dry run with no
// precondition sends dryRun All and no precondition. A Delete method added to
// either interface fails here until it has a row.
func TestEveryDeleteIsBoundByAPrecondition(t *testing.T) {
	t.Parallel()
	var methods []string
	for _, it := range []reflect.Type{reflect.TypeFor[engine.ClusterClient](), reflect.TypeFor[engine.UIDDeleter]()} {
		for m := range it.Methods() {
			if strings.HasPrefix(m.Name, "Delete") {
				methods = append(methods, m.Name)
				require.Contains(t, deleteCalls, m.Name, "every Delete method of %s has a row", it.Name())
			}
		}
	}
	assert.ElementsMatch(t, []string{"Delete", "DeleteWithUID"}, methods)
	for name, dc := range deleteCalls {
		t.Run(name+" with no precondition", func(t *testing.T) {
			t.Parallel()
			s, c := newDeleteServer(t, http.StatusOK, deleteStatus)
			err := dc.call(t.Context(), c, "", false)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			require.ErrorContains(t, err, "internal error: a delete that is not a dry run has no "+dc.field+" precondition; it was not sent")
			assert.Empty(t, s.requests(), "no request reaches the server")
		})
		t.Run(name+" with a precondition", func(t *testing.T) {
			t.Parallel()
			s, c := newDeleteServer(t, http.StatusOK, deleteStatus)
			require.NoError(t, dc.call(t.Context(), c, "7", false))
			reqs := s.requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, http.MethodDelete, reqs[0].method)
			assert.Equal(t, "/api/v1/namespaces/default/configmaps/x", reqs[0].path)
			assert.Equal(t, map[string]any{dc.field: "7"}, reqs[0].body["preconditions"], "exactly the one precondition")
			assert.NotContains(t, reqs[0].body, "dryRun")
		})
		t.Run(name+" dry run", func(t *testing.T) {
			t.Parallel()
			s, c := newDeleteServer(t, http.StatusOK, deleteStatus)
			require.NoError(t, dc.call(t.Context(), c, "", true))
			reqs := s.requests()
			require.Len(t, reqs, 1)
			assert.Equal(t, http.MethodDelete, reqs[0].method)
			assert.Equal(t, []any{"All"}, reqs[0].body["dryRun"])
			assert.NotContains(t, reqs[0].body, "preconditions")
		})
	}
}

// TestDeleteWithUIDReturnsOnlyTheUIDOfTheDryRunResponse asserts the uid a
// dry run returns for each response the API server gives a delete:
// details.uid of a Status, metadata.uid of an object, a Secret's included,
// whose data is not kept, and "" with no error for a response with no uid or
// a body that is not JSON; a delete that is not a dry run returns "". An
// error response is classified as Delete classifies it.
func TestDeleteWithUIDReturnsOnlyTheUIDOfTheDryRunResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, uid string
	}{
		{"a Status", deleteStatus, "u-status"},
		{"a Pod", `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"x","uid":"u-pod","deletionTimestamp":"2026-11-21T00:00:00Z"}}`, "u-pod"},
		{"a Secret", `{"kind":"Secret","apiVersion":"v1","metadata":{"name":"x","uid":"u-secret","finalizers":["example.io/keep"]},"data":{"password":"c2VjcmV0"}}`, "u-secret"},
		{"a Status with no details", `{"kind":"Status","apiVersion":"v1","status":"Success"}`, ""},
		{"an object with no uid", `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"x"}}`, ""},
		{"a body that is not JSON", `deleted`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, c := newDeleteServer(t, http.StatusOK, tc.body)
			d, ok := c.(engine.UIDDeleter)
			require.True(t, ok)
			uid, err := d.DeleteWithUID(t.Context(), deleteGuardGVR, "default", "x", "", true)
			require.NoError(t, err)
			assert.Equal(t, tc.uid, uid)
			uid, err = d.DeleteWithUID(t.Context(), deleteGuardGVR, "default", "x", "u1", false)
			require.NoError(t, err)
			assert.Empty(t, uid, "a delete that is not a dry run returns no uid")
		})
	}
	t.Run("a refusal", func(t *testing.T) {
		t.Parallel()
		_, c := newDeleteServer(t, http.StatusForbidden,
			`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,"message":"configmaps \"x\" is forbidden: User \"u\" cannot delete resource \"configmaps\" in API group \"\" in the namespace \"default\"","details":{"name":"x","kind":"configmaps"}}`)
		uid, err := c.(engine.UIDDeleter).DeleteWithUID(t.Context(), deleteGuardGVR, "default", "x", "", true)
		assert.Empty(t, uid)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		status, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "the status is readable")
		assert.Equal(t, http.StatusForbidden, status)
	})
	t.Run("a curated not found", func(t *testing.T) {
		t.Parallel()
		_, c := newDeleteServer(t, http.StatusNotFound, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"namespaces \"x\" not found"}`)
		_, err := c.(engine.UIDDeleter).DeleteWithUID(t.Context(), schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "x", "u1", false)
		assert.True(t, clierr.IsNotFound(err), "%v", err)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, "namespace not found: x", ce.Message)
	})
	t.Run("no name", func(t *testing.T) {
		t.Parallel()
		s, c := newDeleteServer(t, http.StatusOK, deleteStatus)
		_, err := c.(engine.UIDDeleter).DeleteWithUID(t.Context(), deleteGuardGVR, "default", "", "u1", false)
		require.ErrorContains(t, err, "name is required")
		assert.Empty(t, s.requests(), "a delete of the whole collection is never sent")
	})
}
