package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// refusal is the answer a guard server gives every request.
type refusal struct {
	name        string
	status      int
	contentType string
	body        string
	code        string
	// noReason marks a 401 or 403 whose body gives no reason beyond the
	// status: the message is the status table's fixed text, whatever the body
	// says.
	noReason bool
}

// noReasonText is the text a refusal that gave no reason contains.
func (r refusal) noReasonText() string {
	if r.status == http.StatusForbidden {
		return "(403) and gave no reason"
	}
	return "the server rejected the credential (401)"
}

// refusals are the 401 and 403 bodies a Rancher server or a cluster
// behind its proxy gives: no body, a Kubernetes Status, Rancher's JSON
// error object (its authentication filter writes the 401, steve writes
// the 403 on /v1; on these typed requests client-go drops a JSON body
// that is not a Status, so the row covers the no-reason text), the
// proxy's error object, which it writes as text, and plain text. A 401
// Status whose message is only "Unauthorized" is Rancher's own answer to a
// credential it rejects.
var refusals = []refusal{
	{"401 empty", http.StatusUnauthorized, "", "", clierr.CodeAuth, true},
	{"401 Status", http.StatusUnauthorized, "application/json",
		`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`, clierr.CodeAuth, true},
	{"Rancher 401 Status", http.StatusUnauthorized, "application/json",
		`{"apiVersion":"v1","code":401,"kind":"Status","message":"Unauthorized","metadata":{},"reason":"Unauthorized","status":"Failure"}`, clierr.CodeAuth, true},
	{"401 Rancher", http.StatusUnauthorized, "application/json",
		`{"type":"error","status":"401","message":"Unauthorized 401: must authenticate"}`, clierr.CodeAuth, false},
	{"403 empty", http.StatusForbidden, "", "", clierr.CodeRBAC, true},
	{"403 empty application/json", http.StatusForbidden, "application/json", "", clierr.CodeRBAC, true},
	{"403 Status", http.StatusForbidden, "application/json",
		`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"forbidden","reason":"Forbidden","code":403}`, clierr.CodeRBAC, false},
	{"403 Rancher", http.StatusForbidden, "application/json",
		`{"type":"error","links":{},"code":"PermissionDenied","message":"can not get management.cattle.io.clusters","status":403}`, clierr.CodeRBAC, false},
	{"403 proxy object", http.StatusForbidden, "text/plain; charset=utf-8",
		`{"Code":{"Code":"Forbidden","Status":403},"Message":"clusters.management.cattle.io \"c-1\" is forbidden","Cause":null,"FieldName":""}`, clierr.CodeRBAC, false},
	{"401 text", http.StatusUnauthorized, "text/plain", "Unauthorized", clierr.CodeAuth, true},
}

// refuse answers every request with r.
func refuse(r refusal) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r.contentType != "" {
			w.Header().Set("Content-Type", r.contentType)
		}
		w.WriteHeader(r.status)
		_, _ = w.Write([]byte(r.body))
	})
}

// refusingServer answers every request with r.
func refusingServer(t *testing.T, r refusal) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(refuse(r))
	t.Cleanup(srv.Close)
	return srv
}

// refusingTLSServer answers every request with r over TLS, for the calls
// that take a config.Effective: the CA it returns makes client-go build its
// own transport instead of using http.DefaultTransport.
func refusingTLSServer(t *testing.T, r refusal) (srv *httptest.Server, caPEM string) {
	t.Helper()
	srv = httptest.NewTLSServer(refuse(r))
	t.Cleanup(srv.Close)
	return srv, serverCA(srv)
}

// clusterClientCalls makes one call of each ClusterClient method for gvr;
// PostReview's refusal is its Mapped error.
var clusterClientCalls = map[string]func(context.Context, engine.ClusterClient, schema.GroupVersionResource) error{
	"Get": func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		_, err := c.Get(ctx, gvr, "default", "x")
		return err
	},
	"List": func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		_, err := c.List(ctx, gvr, "default", "")
		return err
	},
	"Create": func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		_, err := c.Create(ctx, gvr, "default", guardObject(gvr), false)
		return err
	},
	"Update": func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		_, err := c.Update(ctx, gvr, "default", guardObject(gvr), false)
		return err
	},
	"Delete": func(ctx context.Context, c engine.ClusterClient, gvr schema.GroupVersionResource) error {
		return c.Delete(ctx, gvr, "default", "x", "", false)
	},
	"Logs": func(ctx context.Context, c engine.ClusterClient, _ schema.GroupVersionResource) error {
		_, err := c.Logs(ctx, "default", "x", engine.LogOptions{})
		return err
	},
	"PostReview": func(ctx context.Context, c engine.ClusterClient, _ schema.GroupVersionResource) error {
		res, err := c.PostReview(ctx, engine.ReviewRequest{Kind: engine.ReviewSelfSubjectIdentity, Object: reviewObj("SelfSubjectReview")})
		if err != nil {
			return err
		}
		if res.Mapped == nil {
			return nil
		}
		return res.Mapped
	},
	"ServerResources": func(ctx context.Context, c engine.ClusterClient, _ schema.GroupVersionResource) error {
		_, err := c.ServerResources(ctx)
		return err
	},
}

// gvrMethods take a GVR; each runs once for a kind with a fixed API
// version and once for a kind with no fixed API version, which
// discovery fills in.
var gvrMethods = map[string]bool{"Get": true, "List": true, "Create": true, "Update": true, "Delete": true}

func guardObject(gvr schema.GroupVersionResource) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(gvr.GroupVersion().String())
	o.SetKind("Guard")
	o.SetName("x")
	return o
}

// TestEveryClusterClientMethodClassifiesARefusal asserts that every
// ClusterClient method, for a kind with a fixed API version and one
// without, returns a 401 as AUTH and a 403 as RBAC with the HTTP status readable
// from the error, whatever the body. A method added to the interface
// fails here until it has a row.
func TestEveryClusterClientMethodClassifiesARefusal(t *testing.T) {
	t.Parallel()
	it := reflect.TypeFor[engine.ClusterClient]()
	for m := range it.Methods() {
		require.Contains(t, clusterClientCalls, m.Name, "every ClusterClient method has a row")
	}
	gvrs := []schema.GroupVersionResource{
		{Version: "v1", Resource: "namespaces"},
		{Group: "ext.cattle.io", Resource: "tokens"},
	}
	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			srv := refusingServer(t, r)
			c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
			require.NoError(t, err)
			for name, call := range clusterClientCalls {
				rows := gvrs[:1]
				if gvrMethods[name] {
					rows = gvrs
				}
				for _, gvr := range rows {
					label := name + " " + gvr.String()
					err := call(t.Context(), c, gvr)
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok, "%s: got %v", label, err)
					assert.Equal(t, r.code, ce.Code, "%s: %s", label, ce.Message)
					assert.NotEmpty(t, ce.Message, label)
					if r.noReason {
						assert.Contains(t, ce.Message, r.noReasonText(), label)
					}
					// apimachinery names the request after the text of a typed pod
					// request; the table words the request itself.
					assert.NotContains(t, ce.Message, " (get pods x)", label)
					assert.NotContains(t, ce.Message, `{"`, "%s: no raw JSON in the message", label)
					status, ok := clierr.HTTPStatus(err)
					assert.True(t, ok, "%s: the status is readable", label)
					assert.Equal(t, r.status, status, label)
				}
			}
		})
	}
}

// providerCalls are this package's requests outside ClusterClient, as the
// commands that make them call them.
var providerCalls = map[string]func(context.Context, config.Effective) error{
	"Preflight": func(ctx context.Context, eff config.Effective) error { _, err := Preflight(ctx, eff); return err },
	"Owner":     func(ctx context.Context, eff config.Effective) error { _, err := Owner(ctx, eff); return err },
	"Identity":  func(ctx context.Context, eff config.Effective) error { _, err := Identity(ctx, eff); return err },
	"EffectiveTokenMetadata": func(ctx context.Context, eff config.Effective) error {
		_, err := EffectiveTokenMetadata(ctx, eff, "token-x")
		return err
	},
	"V3TokenMetadata": func(ctx context.Context, eff config.Effective) error {
		_, err := V3TokenMetadata(ctx, eff, "token-x")
		return err
	},
}

// TestEveryProviderRequestClassifiesARefusal asserts the package's other
// requests return a 401 as AUTH and a 403 as RBAC with the status readable,
// and that the version probe, whose callers fold every error into a
// warning, keeps the status readable.
func TestEveryProviderRequestClassifiesARefusal(t *testing.T) {
	t.Parallel()
	for _, r := range refusals {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			srv, caPEM := refusingTLSServer(t, r)
			eff := config.Effective{URL: srv.URL, CA: caPEM, Token: "token-x:y"}
			for name, call := range providerCalls {
				err := call(t.Context(), eff)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%s: got %v", name, err)
				assert.Equal(t, r.code, ce.Code, "%s: %s", name, ce.Message)
				if r.noReason {
					assert.Contains(t, ce.Message, r.noReasonText(), name)
				}
				status, ok := clierr.HTTPStatus(err)
				assert.True(t, ok, "%s: the status is readable", name)
				assert.Equal(t, r.status, status, name)
			}
			_, err := RancherVersion(t.Context(), eff)
			status, ok := clierr.HTTPStatus(err)
			assert.True(t, ok, "RancherVersion: the status is readable")
			assert.Equal(t, r.status, status)
		})
	}
}

// TestCuratedNotFoundKeepsTheStatus asserts that the NOT_FOUND a known
// kind's Get, Update and Delete curate keeps the 404 readable.
func TestCuratedNotFoundKeepsTheStatus(t *testing.T) {
	t.Parallel()
	srv := refusingServer(t, refusal{status: http.StatusNotFound})
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "local")
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	for name, call := range map[string]func() error{
		"Get":    func() error { _, err := c.Get(t.Context(), gvr, "", "x"); return err },
		"Update": func() error { _, err := c.Update(t.Context(), gvr, "", guardObject(gvr), false); return err },
		"Delete": func() error { return c.Delete(t.Context(), gvr, "", "x", "", false) },
	} {
		err := call()
		assert.True(t, clierr.IsNotFound(err), "%s: %v", name, err)
		status, ok := clierr.HTTPStatus(err)
		assert.True(t, ok, "%s: the status is readable", name)
		assert.Equal(t, http.StatusNotFound, status, name)
	}
}
