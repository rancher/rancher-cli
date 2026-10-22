package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const kubectlAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json;as=Table;v=v1beta1;g=meta.k8s.io,application/json"

var (
	podsGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	nodesGVR       = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

const (
	nodeTableV1 = `{"kind":"Table","apiVersion":"meta.k8s.io/v1","metadata":{"resourceVersion":"15"},` +
		`"columnDefinitions":[{"name":"Name","type":"string","format":"name","priority":0},` +
		`{"name":"Status","type":"string","priority":0},{"name":"OS-Image","type":"string","priority":1}],` +
		`"rows":[{"cells":["node-1","Ready","K3s"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",` +
		`"metadata":{"name":"node-1","uid":"u-1","creationTimestamp":"2026-01-02T03:04:05Z","labels":{"a":"b"}}}}]}`
	nodeTableV1Beta1 = `{"kind":"Table","apiVersion":"meta.k8s.io/v1beta1","metadata":{"resourceVersion":"15"},` +
		`"columnDefinitions":[{"name":"Name","type":"string","format":"name","priority":0}],` +
		`"rows":[{"cells":["node-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1beta1","metadata":{"name":"node-1"}}}]}`
	numbersTable = `{"kind":"Table","apiVersion":"meta.k8s.io/v1","metadata":{},` +
		`"columnDefinitions":[{"name":"Name","type":"string","priority":0},{"name":"Count","type":"integer","priority":0},` +
		`{"name":"Ratio","type":"number","priority":0},{"name":"Big","type":"integer","priority":0},{"name":"Flag","type":"boolean","priority":0},` +
		`{"name":"None","type":"string","priority":0}],` +
		`"rows":[{"cells":["a",3,0.5,9007199254740993,true,null],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{"name":"a"}}}]}`
	plainNodeList = `{"kind":"NodeList","apiVersion":"v1","metadata":{"resourceVersion":"9"},"items":[` +
		`{"metadata":{"name":"node-1","creationTimestamp":"2026-01-02T03:04:05Z"}},{"metadata":{"name":"node-2"}}]}`
	plainNode = `{"kind":"Node","apiVersion":"v1","metadata":{"name":"node-1","creationTimestamp":"2026-01-02T03:04:05Z"}}`
)

// seenRequest is one request a table test server received.
type seenRequest struct {
	Path, Query, Accept, UserAgent string
}

type tableServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

func (s *tableServer) requests() []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seenRequest(nil), s.seen...)
}

func (s *tableServer) last(t *testing.T) seenRequest {
	t.Helper()
	got := s.requests()
	require.NotEmpty(t, got)
	return got[len(got)-1]
}

// newTableServer starts a server that records every request and answers
// with handler.
func newTableServer(t *testing.T, handler http.HandlerFunc) *tableServer {
	t.Helper()
	ts := &tableServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.seen = append(ts.seen, seenRequest{r.URL.Path, r.URL.RawQuery, r.Header.Get("Accept"), r.Header.Get("User-Agent")})
		ts.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func answer(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func tableClientFor(t *testing.T, srv *httptest.Server) engine.ClusterClient {
	t.Helper()
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t", Transport: srv.Client().Transport}, "c-1")
	require.NoError(t, err)
	return c
}

func tableReader(t *testing.T, c engine.ClusterClient) engine.TableReader {
	t.Helper()
	tr, ok := c.(engine.TableReader)
	require.True(t, ok, "the provider client reads tables")
	return tr
}

// TestTableRequestsSendKubectlsAcceptAndKeepTheSelector asserts the request
// of ListTable and GetTable: kubectl's Accept value, the group-version path
// with the namespace when there is one, the label selector, and no
// includeObject; and that List sends plain JSON, so a table request never
// changes the other reads.
func TestTableRequestsSendKubectlsAcceptAndKeepTheSelector(t *testing.T) {
	t.Parallel()
	srv := newTableServer(t, answer(200, "application/json", nodeTableV1))
	c := tableClientFor(t, srv.Server)
	tr := tableReader(t, c)

	_, err := tr.ListTable(t.Context(), podsGVR, "ns-1", "app=web,tier!=cache")
	require.NoError(t, err)
	got := srv.last(t)
	assert.Equal(t, "/api/v1/namespaces/ns-1/pods", got.Path)
	assert.Equal(t, kubectlAccept, got.Accept)
	assert.Equal(t, "labelSelector=app%3Dweb%2Ctier%21%3Dcache", got.Query)
	assert.NotContains(t, got.Query, "includeObject")

	_, err = tr.ListTable(t.Context(), nodesGVR, "", "")
	require.NoError(t, err)
	got = srv.last(t)
	assert.Equal(t, "/api/v1/nodes", got.Path)
	assert.Empty(t, got.Query)
	assert.Equal(t, kubectlAccept, got.Accept)

	_, err = tr.ListTable(t.Context(), deploymentsGVR, "", "")
	require.NoError(t, err)
	assert.Equal(t, "/apis/apps/v1/deployments", srv.last(t).Path)

	_, err = tr.GetTable(t.Context(), deploymentsGVR, "ns-1", "web")
	require.NoError(t, err)
	got = srv.last(t)
	assert.Equal(t, "/apis/apps/v1/namespaces/ns-1/deployments/web", got.Path)
	assert.Equal(t, kubectlAccept, got.Accept)
	assert.Empty(t, got.Query)

	_, err = c.List(t.Context(), podsGVR, "ns-1", "app=web")
	require.NoError(t, err)
	got = srv.last(t)
	assert.Equal(t, "application/json", got.Accept)
	assert.Equal(t, "/api/v1/namespaces/ns-1/pods", got.Path)
}

// TestTableAnswersDecode asserts what a table read returns for each answer
// the server can give: a Table of either version with its row objects
// decoded, whole numbers as int64, and, for a body that is no Table, the
// ordinary list or object.
func TestTableAnswersDecode(t *testing.T) {
	t.Parallel()
	type want func(t *testing.T, a engine.TableAnswer)
	cases := []struct {
		name  string
		body  string
		named bool
		check want
	}{
		{"v1 table", nodeTableV1, false, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			require.NotNil(t, a.Table)
			assert.Empty(t, a.Items)
			require.Len(t, a.Table.ColumnDefinitions, 3)
			assert.Equal(t, "OS-Image", a.Table.ColumnDefinitions[2].Name)
			assert.Equal(t, int32(1), a.Table.ColumnDefinitions[2].Priority)
			require.Len(t, a.Table.Rows, 1)
			assert.Equal(t, []any{"node-1", "Ready", "K3s"}, a.Table.Rows[0].Cells)
			meta, ok := a.Table.Rows[0].Object.Object.(*metav1.PartialObjectMetadata)
			require.True(t, ok, "the row's object is decoded to its metadata")
			assert.Equal(t, "node-1", meta.Name)
			assert.Equal(t, map[string]string{"a": "b"}, meta.Labels)
		}},
		{"v1beta1 table", nodeTableV1Beta1, false, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			require.NotNil(t, a.Table)
			require.Len(t, a.Table.Rows, 1)
			assert.Equal(t, []any{"node-1"}, a.Table.Rows[0].Cells)
			meta, ok := a.Table.Rows[0].Object.Object.(*metav1.PartialObjectMetadata)
			require.True(t, ok)
			assert.Equal(t, "node-1", meta.Name)
		}},
		{"integer and number cells", numbersTable, false, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			require.NotNil(t, a.Table)
			require.Len(t, a.Table.Rows, 1)
			assert.Equal(t, []any{"a", int64(3), 0.5, int64(9007199254740993), true, nil}, a.Table.Rows[0].Cells)
		}},
		{"plain list", plainNodeList, false, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			assert.Nil(t, a.Table)
			require.Len(t, a.Items, 2)
			assert.Equal(t, "node-1", a.Items[0].GetName())
			assert.Equal(t, "node-2", a.Items[1].GetName())
		}},
		{"plain object", plainNode, true, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			assert.Nil(t, a.Table)
			require.Len(t, a.Items, 1)
			assert.Equal(t, "node-1", a.Items[0].GetName())
		}},
		{"named get is one row", nodeTableV1, true, func(t *testing.T, a engine.TableAnswer) {
			t.Helper()
			require.NotNil(t, a.Table)
			assert.Len(t, a.Table.Rows, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newTableServer(t, answer(200, "application/json", tc.body))
			tr := tableReader(t, tableClientFor(t, srv.Server))
			var (
				a   engine.TableAnswer
				err error
			)
			if tc.named {
				a, err = tr.GetTable(t.Context(), nodesGVR, "", "node-1")
			} else {
				a, err = tr.ListTable(t.Context(), nodesGVR, "", "")
			}
			require.NoError(t, err)
			tc.check(t, a)
		})
	}
}

func requireSameCLIError(t *testing.T, want, got error) {
	t.Helper()
	require.Error(t, want)
	require.Error(t, got)
	w, ok := errors.AsType[*clierr.Error](want)
	require.True(t, ok, "want a structured error, got %v", want)
	g, ok := errors.AsType[*clierr.Error](got)
	require.True(t, ok, "want a structured error, got %v", got)
	assert.Equal(t, w.Code, g.Code)
	assert.Equal(t, w.Message, g.Message)
	assert.Equal(t, w.Hint, g.Hint)
	assert.Equal(t, w.Retryable, g.Retryable)
}

// statusBody is a JSON Status answer with the given code and reason.
func statusBody(code int, reason, message string) string {
	return fmt.Sprintf(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":%q,"reason":%q,"code":%d}`, message, reason, code)
}

const (
	notFoundStatus  = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"pods \"web\" not found","reason":"NotFound","details":{"name":"web","kind":"pods"},"code":404}`
	forbiddenStatus = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"pods is forbidden: User \"u-1\" cannot get resource \"pods\" in API group \"\" in the namespace \"ns-1\"","reason":"Forbidden","details":{"kind":"pods"},"code":403}`
)

// TestTableReadsFailLikeListAndGet asserts a table read gives the same
// structured error as the dynamic client's List and Get for each answer
// and failure: a 404, 401, 403, 409, 429 and 503 Status (the last two
// without a Retry-After header, which client-go would retry on), Rancher's
// plain text 403, a 500 without a Content-Type, a 200 with an empty body
// and a 200 with JSON that has no kind, the remedy hint of the management
// cluster kind, a group whose version is empty and resolved by discovery,
// a timeout, an invalid name and namespace, and a certificate failure.
// Mutations: skip curateNotFound on GetTable; skip opCtx on ListTable; skip
// forbiddenHint on ListTable or on GetTable; skip resolveVersion on
// ListTable.
func TestTableReadsFailLikeListAndGet(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"404 status", answer(404, "application/json", notFoundStatus)},
		{"403 status", answer(403, "application/json", forbiddenStatus)},
		{"403 text", answer(403, "text/plain; charset=utf-8", "Forbidden\n")},
		{"500 without content type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header()["Content-Type"] = nil
			w.WriteHeader(500)
			_, _ = w.Write([]byte("boom"))
		}},
		{"401 status", answer(401, "application/json", statusBody(401, "Unauthorized", "Unauthorized"))},
		{"409 status", answer(409, "application/json", statusBody(409, "Conflict", "the object has been modified"))},
		{"429 status without Retry-After", answer(429, "application/json", statusBody(429, "TooManyRequests", "too many requests"))},
		{"503 status", answer(503, "application/json", statusBody(503, "ServiceUnavailable", "the server is unavailable"))},
		{"200 with an empty body", answer(200, "application/json", "")},
		{"200 without a kind", answer(200, "application/json", `{"metadata":{}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTableServer(t, tc.handler)
			c := tableClientFor(t, srv.Server)
			tr := tableReader(t, c)

			_, want := c.List(t.Context(), podsGVR, "ns-1", "")
			_, got := tr.ListTable(t.Context(), podsGVR, "ns-1", "")
			requireSameCLIError(t, want, got)

			_, want = c.Get(t.Context(), podsGVR, "ns-1", "web")
			_, got = tr.GetTable(t.Context(), podsGVR, "ns-1", "web")
			requireSameCLIError(t, want, got)
		})
	}

	t.Run("management cluster kind hints", func(t *testing.T) {
		k, ok := kinds.ByKey("managementcluster")
		require.True(t, ok)
		srv := newTableServer(t, answer(403, "application/json", forbiddenStatus))
		c := tableClientFor(t, srv.Server)
		tr := tableReader(t, c)

		_, want := c.List(t.Context(), k.GVR, "", "")
		_, got := tr.ListTable(t.Context(), k.GVR, "", "")
		requireSameCLIError(t, want, got)
		ce, ok := errors.AsType[*clierr.Error](got)
		require.True(t, ok)
		assert.Equal(t, kinds.ForbiddenHint(k, "list", "", ""), ce.Hint)
		assert.NotEmpty(t, ce.Hint)

		_, want = c.Get(t.Context(), k.GVR, "", "c-m-1")
		_, got = tr.GetTable(t.Context(), k.GVR, "", "c-m-1")
		requireSameCLIError(t, want, got)
		ce, ok = errors.AsType[*clierr.Error](got)
		require.True(t, ok)
		assert.Equal(t, kinds.ForbiddenHint(k, "get", "", "c-m-1"), ce.Hint)
		assert.NotEmpty(t, ce.Hint)
	})

	t.Run("an empty version is resolved by discovery", func(t *testing.T) {
		gvr := schema.GroupVersionResource{Group: "cluster.x-k8s.io", Resource: "machines"}
		const listPath = "/apis/cluster.x-k8s.io/v1beta1/namespaces/fleet-default/machines"
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/apis":
				writeJSON(w, "application/json", metav1.APIGroupList{Kind: "APIGroupList", Groups: []metav1.APIGroup{{
					Name:             "cluster.x-k8s.io",
					PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "cluster.x-k8s.io/v1beta1", Version: "v1beta1"},
				}}})
			case listPath, listPath + "/m-1":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(nodeTableV1))
			default:
				http.NotFound(w, r)
			}
		}
		t.Run("ListTable", func(t *testing.T) {
			srv := newTableServer(t, handler)
			tr := tableReader(t, tableClientFor(t, srv.Server))
			ans, err := tr.ListTable(t.Context(), gvr, "fleet-default", "")
			require.NoError(t, err)
			assert.NotNil(t, ans.Table)
			assert.Equal(t, listPath, srv.last(t).Path)
		})
		t.Run("GetTable", func(t *testing.T) {
			srv := newTableServer(t, handler)
			tr := tableReader(t, tableClientFor(t, srv.Server))
			ans, err := tr.GetTable(t.Context(), gvr, "fleet-default", "m-1")
			require.NoError(t, err)
			assert.NotNil(t, ans.Table)
			assert.Equal(t, listPath+"/m-1", srv.last(t).Path)
		})
	})

	t.Run("not found names the object", func(t *testing.T) {
		srv := newTableServer(t, answer(404, "application/json", notFoundStatus))
		_, err := tableReader(t, tableClientFor(t, srv.Server)).GetTable(t.Context(), podsGVR, "ns-1", "web")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeNotFound, ce.Code)
		assert.NotEmpty(t, ce.Hint, "a named miss carries the list hint like Get")
	})

	t.Run("forbidden carries the hint", func(t *testing.T) {
		srv := newTableServer(t, answer(403, "application/json", forbiddenStatus))
		_, err := tableReader(t, tableClientFor(t, srv.Server)).ListTable(t.Context(), podsGVR, "ns-1", "")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
	})

	t.Run("timeout", func(t *testing.T) {
		restore := SetRequestTimeoutForTest(50 * time.Millisecond)
		defer restore()
		srv := newTableServer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		c := tableClientFor(t, srv.Server)
		tr := tableReader(t, c)
		_, want := c.List(t.Context(), podsGVR, "ns-1", "")
		_, got := tr.ListTable(t.Context(), podsGVR, "ns-1", "")
		requireSameCLIError(t, want, got)
		_, want = c.Get(t.Context(), podsGVR, "ns-1", "web")
		_, got = tr.GetTable(t.Context(), podsGVR, "ns-1", "web")
		requireSameCLIError(t, want, got)
		ce, ok := errors.AsType[*clierr.Error](got)
		require.True(t, ok)
		assert.True(t, ce.Retryable)
	})

	t.Run("invalid names", func(t *testing.T) {
		srv := newTableServer(t, answer(200, "application/json", nodeTableV1))
		c := tableClientFor(t, srv.Server)
		tr := tableReader(t, c)
		_, want := c.List(t.Context(), podsGVR, "a/b", "")
		_, got := tr.ListTable(t.Context(), podsGVR, "a/b", "")
		requireSameCLIError(t, want, got)
		require.ErrorContains(t, got, `invalid namespace "a/b"`)
		_, want = c.Get(t.Context(), podsGVR, "ns-1", "a/b")
		_, got = tr.GetTable(t.Context(), podsGVR, "ns-1", "a/b")
		requireSameCLIError(t, want, got)
		require.ErrorContains(t, got, `invalid resource name "a/b"`)
		assert.Empty(t, srv.requests(), "an invalid name sends no request")
	})

	t.Run("untrusted certificate", func(t *testing.T) {
		rc, err := kube.RESTConfig(untrustedEffective(t), "c-1")
		require.NoError(t, err)
		c, err := NewClusterClient(rc, "c-1")
		require.NoError(t, err)
		tr := tableReader(t, c)
		_, err = c.List(t.Context(), podsGVR, "ns-1", "")
		requireClassified(t, err)
		_, err = tr.ListTable(t.Context(), podsGVR, "ns-1", "")
		requireClassified(t, err)
		_, err = c.Get(t.Context(), podsGVR, "ns-1", "web")
		requireClassified(t, err)
		_, err = tr.GetTable(t.Context(), podsGVR, "ns-1", "web")
		requireClassified(t, err)
	})
}

// TestTableReadsShareUserAgentAndRedirectPolicy asserts a table read sends
// the same User-Agent as List and Get, and refuses a redirect to another
// origin as they do, without sending the credential there. Mutation: build
// the table client over rest.HTTPClientFor instead of kube.HTTPClient.
func TestTableReadsShareUserAgentAndRedirectPolicy(t *testing.T) {
	t.Parallel()
	t.Run("user agent", func(t *testing.T) {
		t.Parallel()
		srv := newTableServer(t, answer(200, "application/json", plainNodeList))
		c := tableClientFor(t, srv.Server)
		tr := tableReader(t, c)
		_, err := c.List(t.Context(), nodesGVR, "", "")
		require.NoError(t, err)
		listAgent := srv.last(t).UserAgent
		_, err = tr.ListTable(t.Context(), nodesGVR, "", "")
		require.NoError(t, err)
		assert.Equal(t, httpx.UserAgent(), srv.last(t).UserAgent)
		assert.Equal(t, listAgent, srv.last(t).UserAgent)
		_, err = tr.GetTable(t.Context(), nodesGVR, "", "node-1")
		require.NoError(t, err)
		assert.Equal(t, listAgent, srv.last(t).UserAgent)
	})
	t.Run("cross origin redirect", func(t *testing.T) {
		t.Parallel()
		other := newTableServer(t, answer(200, "application/json", plainNodeList))
		srv := newTableServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		})
		c := tableClientFor(t, srv.Server)
		tr := tableReader(t, c)
		_, want := c.List(t.Context(), nodesGVR, "", "")
		_, got := tr.ListTable(t.Context(), nodesGVR, "", "")
		requireSameCLIError(t, want, got)
		require.ErrorContains(t, got, "credentialed redirects are not followed across origins")
		_, want = c.Get(t.Context(), nodesGVR, "", "node-1")
		_, got = tr.GetTable(t.Context(), nodesGVR, "", "node-1")
		requireSameCLIError(t, want, got)
		assert.Empty(t, other.requests(), "the other origin receives no request")
	})
}
