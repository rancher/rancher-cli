package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

var watchGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}

const (
	watchCollection = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	watchObjectPath = watchCollection + "/x"
	goneStatus      = `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"too old resource version: 1 (100)","reason":"Expired","code":410}`
)

// clusterJSON is the object the streaming server serves, in the state and
// with its own resourceVersion.
func clusterJSON(state, rv string) map[string]any {
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "x", "resourceVersion": rv},
		"status":   map[string]any{"state": state},
	}
}

func listJSON(rv string, items ...map[string]any) string {
	if items == nil {
		items = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "ClusterList",
		"metadata": map[string]any{"resourceVersion": rv},
		"items":    items,
	})
	return string(b)
}

// bookmarkJSON is the object of a BOOKMARK event as an API server sends it:
// the kind, the apiVersion and the resourceVersion, and no name or state.
func bookmarkJSON(rv string) map[string]any {
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"resourceVersion": rv},
	}
}

// errorBody is an answer body for a refused request: what the first rows
// used (an empty object), the API server's Status, and Rancher's own error
// object.
type errorBody struct {
	name string
	body func(status int) string
	// verb is whether the message of the classified error names the verb.
	verb bool
}

var statusReasons = map[int]metav1.StatusReason{
	http.StatusBadRequest:          metav1.StatusReasonBadRequest,
	http.StatusUnauthorized:        metav1.StatusReasonUnauthorized,
	http.StatusForbidden:           metav1.StatusReasonForbidden,
	http.StatusNotFound:            metav1.StatusReasonNotFound,
	http.StatusMethodNotAllowed:    metav1.StatusReasonMethodNotAllowed,
	http.StatusInternalServerError: metav1.StatusReasonInternalError,
}

var errorBodies = []errorBody{
	{"empty object", func(int) string { return `{}` }, true},
	{"API server Status", func(status int) string {
		st := metav1.Status{
			Status:  metav1.StatusFailure,
			Message: `clusters.management.cattle.io "x" ` + strings.ToLower(http.StatusText(status)),
			Reason:  statusReasons[status], Code: int32(status),
		}
		st.Kind, st.APIVersion = "Status", "v1"
		b, _ := json.Marshal(st)
		return string(b)
	}, false},
	{"Rancher error object", func(status int) string {
		b, _ := json.Marshal(map[string]any{
			"type": "error", "links": map[string]any{}, "baseType": "error",
			"code": http.StatusText(status), "message": http.StatusText(status), "status": status,
		})
		return string(b)
	}, false},
}

func eventJSON(typ string, object any) string {
	b, _ := json.Marshal(map[string]any{"type": typ, "object": object})
	return string(b) + "\n"
}

// serverCall is one request the streaming server received.
type serverCall struct {
	kind string // "list", "get" or "watch"
	at   time.Duration
	rv   string // a watch's resourceVersion, a list's answer
}

// streamServer is a TLS test server that answers a List by name, a Get and a
// watch from scripts, logs every request, and checks the shape of every wait
// request: a List by name carries the field selector and no resourceVersion,
// and a watch carries timeoutSeconds from 1 to 60, a resourceVersion, the
// field selector and allowWatchBookmarks. A request that breaks a rule is
// answered 400 and recorded, and the test fails when any is.
type streamServer struct {
	t     *testing.T
	srv   *httptest.Server
	start time.Time

	// onList, onGet and onWatch answer the n-th request of their kind (from
	// 0). The defaults: a List of one idle object, a Get of it, and a stream
	// that stays open until the client goes.
	onList  func(n int, w http.ResponseWriter, r *http.Request)
	onGet   func(n int, w http.ResponseWriter, r *http.Request)
	onWatch func(n int, w http.ResponseWriter, r *http.Request)

	mu         sync.Mutex
	calls      []serverCall
	violations []string
	lists      int
	gets       int
	watches    int
	open       int
	maxOpen    int
	lastRV     string
}

func newStreamServer(t *testing.T) *streamServer {
	t.Helper()
	s := &streamServer{t: t, start: time.Now()}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		assert.Empty(t, s.violations, "a wait request broke a rule of the request hook")
	})
	return s
}

func (s *streamServer) violate(w http.ResponseWriter, rule string) {
	s.mu.Lock()
	s.violations = append(s.violations, rule)
	s.mu.Unlock()
	writeAnswer(w, http.StatusBadRequest, "", `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"`+rule+`","reason":"BadRequest","code":400}`)
}

func (s *streamServer) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	at := time.Since(s.start)
	switch {
	case r.URL.Path == watchCollection && q.Get("watch") == "true":
		secs := q.Get("timeoutSeconds")
		n := 0
		_, _ = fmt.Sscanf(secs, "%d", &n)
		switch {
		case n < 1 || n > 60:
			s.violate(w, "a watch carries timeoutSeconds from 1 to 60")
			return
		case q.Get("resourceVersion") == "":
			s.violate(w, "a watch carries a resourceVersion")
			return
		case q.Get("fieldSelector") != "metadata.name=x":
			s.violate(w, "a watch carries fieldSelector=metadata.name=<name>")
			return
		case q.Get("allowWatchBookmarks") != "true":
			s.violate(w, "a watch carries allowWatchBookmarks=true")
			return
		}
		s.mu.Lock()
		i := s.watches
		s.watches++
		s.open++
		s.maxOpen = max(s.maxOpen, s.open)
		s.calls = append(s.calls, serverCall{kind: "watch", at: at, rv: q.Get("resourceVersion")})
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.open--
			s.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "application/json")
		if s.onWatch != nil {
			s.onWatch(i, w, r)
			return
		}
		testtransport.HoldStreamAndAbort(w, r)
	case r.URL.Path == watchCollection:
		if q.Get("fieldSelector") != "metadata.name=x" || q.Get("resourceVersion") != "" {
			s.violate(w, "a List of a wait carries fieldSelector=metadata.name=<name> and no resourceVersion")
			return
		}
		s.mu.Lock()
		i := s.lists
		s.lists++
		s.lastRV = fmt.Sprintf("%d", 100+i)
		s.calls = append(s.calls, serverCall{kind: "list", at: at, rv: s.lastRV})
		s.mu.Unlock()
		if s.onList != nil {
			s.onList(i, w, r)
			return
		}
		s.writeList(w, "idle")
	case r.URL.Path == watchObjectPath:
		s.mu.Lock()
		i := s.gets
		s.gets++
		s.calls = append(s.calls, serverCall{kind: "get", at: at})
		s.mu.Unlock()
		if s.onGet != nil {
			s.onGet(i, w, r)
			return
		}
		b, _ := json.Marshal(clusterJSON("idle", "50"))
		writeAnswer(w, http.StatusOK, "", string(b))
	default:
		http.NotFound(w, r)
	}
}

// writeList answers a List with one object in the state; its own
// resourceVersion is 50, below the version of the list.
func (s *streamServer) writeList(w http.ResponseWriter, state string) {
	s.mu.Lock()
	rv := s.lastRV
	s.mu.Unlock()
	writeAnswer(w, http.StatusOK, "", listJSON(rv, clusterJSON(state, "50")))
}

// writeEmptyList answers a List with no item.
func (s *streamServer) writeEmptyList(w http.ResponseWriter) {
	s.mu.Lock()
	rv := s.lastRV
	s.mu.Unlock()
	writeAnswer(w, http.StatusOK, "", listJSON(rv))
}

func (s *streamServer) snapshot() []serverCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]serverCall(nil), s.calls...)
}

func (s *streamServer) sequence() string {
	var out []string
	for _, c := range s.snapshot() {
		out = append(out, c.kind)
	}
	return strings.Join(out, " ")
}

// requireInvariants asserts what holds for every wait run against the
// server: the calls sent by any time T are at most floor(T / interval) + 2,
// every watch starts from the version of the list before it, and at most one
// stream was open at a time.
func (s *streamServer) requireInvariants(interval time.Duration) {
	s.t.Helper()
	sent, lastList := 0, ""
	for _, c := range s.snapshot() {
		sent++
		require.LessOrEqual(s.t, sent, int(c.at/interval)+2, "calls sent by %v: %s", c.at, s.sequence())
		switch c.kind {
		case "list":
			lastList = c.rv
		case "watch":
			require.Equal(s.t, lastList, c.rv, "a watch starts from the version of the list before it: %s", s.sequence())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.LessOrEqual(s.t, s.maxOpen, 1, "at most one stream is open at a time")
}

// watchingClient is a cluster client against srv.
func watchingClient(t *testing.T, srv *httptest.Server, mod func(rc *rest.Config)) engine.ClusterClient {
	t.Helper()
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, "local")
	require.NoError(t, err)
	rc.Transport = srv.Client().Transport
	if mod != nil {
		mod(rc)
	}
	c, err := NewClusterClient(rc, "local")
	require.NoError(t, err)
	return c
}

func watcher(t *testing.T, c engine.ClusterClient) engine.ObjectWatcher {
	t.Helper()
	w, ok := c.(engine.ObjectWatcher)
	require.True(t, ok, "the cluster client offers engine.ObjectWatcher")
	return w
}

// receive reads one event, failing when none comes in time.
func receive(t *testing.T, ch <-chan engine.ObjectEvent) (engine.ObjectEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no event and no close in 10 seconds")
		return engine.ObjectEvent{}, false
	}
}

func objectState(obj *unstructured.Unstructured) string {
	s, _, _ := unstructured.NestedString(obj.Object, "status", "state")
	return s
}

// TestNewClusterClientOffersObjectWatcherAndTableReader asserts the client
// NewClusterClient builds has ListObject and WatchObject as well as the table
// reader. NewClusterClient returns the interface, so a wrapper that hides a
// method fails here, beside the compile-time assertions on the concrete type.
func TestNewClusterClientOffersObjectWatcherAndTableReader(t *testing.T) {
	srv := newStreamServer(t)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.srv.URL, Token: "id:secret"}, "local")
	require.NoError(t, err)
	rc.Transport = srv.srv.Client().Transport

	c, err := NewClusterClient(rc, "local")
	require.NoError(t, err)
	_, watches := c.(engine.ObjectWatcher)
	assert.True(t, watches)
	_, tables := c.(engine.TableReader)
	assert.True(t, tables)
}

// waitResult is what one engine.WaitObject run gave.
type waitResult struct {
	err     error
	done    *unstructured.Unstructured
	elapsed time.Duration
}

// timedOutError is the error the test OnTimeout returns.
type timedOutError struct{ last *unstructured.Unstructured }

func (*timedOutError) Error() string { return "timed out" }

// waitFor runs engine.WaitObject against the cluster object x: a state of
// "done" ends the wait.
func waitFor(t *testing.T, c engine.ClusterClient, timeout, interval time.Duration) waitResult {
	t.Helper()
	var res waitResult
	start := time.Now()
	res.err = engine.WaitObject(t.Context(), c, engine.WaitParams{
		GVR: watchGVR, Name: "x", Timeout: timeout, Interval: interval,
		Observe: func(_ context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			if objectState(obj) == "done" {
				res.done = obj
				return engine.WaitDone, nil
			}
			return engine.WaitContinue, nil
		},
		RecordErr: func(ctx context.Context, _ error) bool { return ctx.Err() == nil },
		OnAbort:   func(err error) error { return err },
		OnCancel:  func(ctx context.Context, _ *unstructured.Unstructured, _ *clierr.Error) error { return ctx.Err() },
		OnTimeout: func(last *unstructured.Unstructured, _ *clierr.Error) error { return &timedOutError{last: last} },
	})
	res.elapsed = time.Since(start)
	return res
}

// TestListObjectSendsTheNameSelectorAndReturnsTheItemAndVersion asserts the
// List carries the field selector on the name and no resourceVersion, goes
// with the caller's bearer and the client's User-Agent, and returns the item
// and the version of the list, which is not the item's own.
func TestListObjectSendsTheNameSelectorAndReturnsTheItemAndVersion(t *testing.T) {
	srv := newStreamServer(t)
	var got http.Header
	srv.onList = func(_ int, w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		srv.writeList(w, "idle")
	}
	obj, rv, err := watcher(t, watchingClient(t, srv.srv, nil)).ListObject(t.Context(), watchGVR, "", "x")
	require.NoError(t, err)
	require.NotNil(t, obj)
	assert.Equal(t, "x", obj.GetName())
	assert.Equal(t, "50", obj.GetResourceVersion())
	assert.Equal(t, "100", rv, "the version of the list, not of the item")
	assert.Equal(t, "Bearer id:secret", got.Get("Authorization"))
	assert.Equal(t, httpx.UserAgent(), got.Get("User-Agent"))
}

// TestListObjectOfAnEmptyListReturnsNoItem asserts an empty list gives a nil
// item, the list's version and no error.
func TestListObjectOfAnEmptyListReturnsNoItem(t *testing.T) {
	srv := newStreamServer(t)
	srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) { srv.writeEmptyList(w) }
	obj, rv, err := watcher(t, watchingClient(t, srv.srv, nil)).ListObject(t.Context(), watchGVR, "", "x")
	require.NoError(t, err)
	assert.Nil(t, obj)
	assert.Equal(t, "100", rv)
}

// TestListObjectOfSeveralItemsIsAnUnusableList asserts a list of more than
// one item, which a name selector cannot produce, gives no item and an
// engine.ErrListUnusable error, which sends a wait to polling.
func TestListObjectOfSeveralItemsIsAnUnusableList(t *testing.T) {
	srv := newStreamServer(t)
	srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAnswer(w, http.StatusOK, "", listJSON("100", clusterJSON("idle", "50"), clusterJSON("idle", "51")))
	}
	obj, _, err := watcher(t, watchingClient(t, srv.srv, nil)).ListObject(t.Context(), watchGVR, "", "x")
	assert.Nil(t, obj)
	require.ErrorIs(t, err, engine.ErrListUnusable)
}

// TestListObjectMapsErrorsAsAGetDoes asserts the error codes of a refused
// List, for an empty body, the API server's Status and Rancher's error
// object: 400 and 405 with their status, 401 AUTH, 403 RBAC with no hint,
// 404 NOT_FOUND, 500 retryable for the empty body (the classification of a
// Status with a reason is the Get's); and the verb list in the message of the
// empty-body answer.
func TestListObjectMapsErrorsAsAGetDoes(t *testing.T) {
	for _, tt := range []struct {
		status    int
		code      string
		retryable bool
		verb      string
	}{
		{http.StatusBadRequest, "", false, ""},
		{http.StatusUnauthorized, clierr.CodeAuth, false, "list"},
		{http.StatusForbidden, clierr.CodeRBAC, false, "list"},
		{http.StatusNotFound, clierr.CodeNotFound, false, ""},
		{http.StatusMethodNotAllowed, "", false, ""},
		{http.StatusInternalServerError, clierr.CodeGeneric, true, ""},
	} {
		for _, body := range errorBodies {
			t.Run(fmt.Sprint(tt.status, " ", body.name), func(t *testing.T) {
				srv := newStreamServer(t)
				srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) {
					writeAnswer(w, tt.status, "", body.body(tt.status))
				}
				_, _, err := watcher(t, watchingClient(t, srv.srv, nil)).ListObject(t.Context(), watchGVR, "", "x")
				require.Error(t, err)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				if tt.code != "" {
					assert.Equal(t, tt.code, ce.Code)
				}
				if body.verb {
					assert.Equal(t, tt.retryable, ce.Retryable)
				}
				status, answered := clierr.HTTPStatus(err)
				assert.True(t, answered)
				assert.Equal(t, tt.status, status)
				if tt.verb != "" && body.verb {
					assert.Contains(t, ce.Message, tt.verb+" clusters")
				}
				if tt.status == http.StatusForbidden {
					assert.Empty(t, ce.Hint)
				}
			})
		}
	}
}

// TestListObjectOfAListWithNoResourceVersionIsAnUnusableList asserts a list
// the server answers with no resourceVersion, empty or not, is an
// engine.ErrListUnusable error and never a start version for a watch.
func TestListObjectOfAListWithNoResourceVersionIsAnUnusableList(t *testing.T) {
	for name, body := range map[string]string{
		"empty list": listJSON(""),
		"one item":   listJSON("", clusterJSON("idle", "50")),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newStreamServer(t)
			srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) { writeAnswer(w, http.StatusOK, "", body) }
			obj, rv, err := watcher(t, watchingClient(t, srv.srv, nil)).ListObject(t.Context(), watchGVR, "", "x")
			assert.Nil(t, obj)
			assert.Empty(t, rv)
			require.ErrorIs(t, err, engine.ErrListUnusable)
		})
	}
}

// TestListObjectOfATypedListFillsTheKindAndVersionOfItsItems asserts the item of
// a typed list, whose items carry no kind or apiVersion as a pod list's do,
// comes back with both, equal to what a Get of the same object returns.
func TestListObjectOfATypedListFillsTheKindAndVersionOfItsItems(t *testing.T) {
	const pod = `{"metadata":{"name":"x","namespace":"ns","resourceVersion":"50"},"status":{"phase":"Running"}}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/namespaces/ns/pods") && r.URL.Query().Get("fieldSelector") == "metadata.name=x":
			writeAnswer(w, http.StatusOK, "", `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"100"},"items":[`+pod+`]}`)
		case strings.HasSuffix(r.URL.Path, "/namespaces/ns/pods/x"):
			writeAnswer(w, http.StatusOK, "", `{"kind":"Pod","apiVersion":"v1",`+strings.TrimPrefix(pod, "{"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := watchingClient(t, srv, nil)
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	listed, rv, err := watcher(t, c).ListObject(t.Context(), gvr, "ns", "x")
	require.NoError(t, err)
	require.NotNil(t, listed)
	assert.Equal(t, "100", rv)
	assert.Equal(t, "Pod", listed.GetKind())
	assert.Equal(t, "v1", listed.GetAPIVersion())
	got, err := c.Get(t.Context(), gvr, "ns", "x")
	require.NoError(t, err)
	assert.Equal(t, got.Object, listed.Object)
}

// TestWatchObjectSendsTheQuery asserts the watch request: the path of the
// kind, watch=true, the name field selector, the resourceVersion asked for,
// allowWatchBookmarks and timeoutSeconds, with the caller's bearer and the
// client's User-Agent.
func TestWatchObjectSendsTheQuery(t *testing.T) {
	srv := newStreamServer(t)
	var query url.Values
	var header http.Header
	srv.onWatch = func(_ int, w http.ResponseWriter, r *http.Request) {
		query, header = r.URL.Query(), r.Header.Clone()
	}
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	require.NoError(t, err)
	_, ok := receive(t, ch)
	assert.False(t, ok, "the server closed the stream")
	assert.Equal(t, "true", query.Get("watch"))
	assert.Equal(t, "metadata.name=x", query.Get("fieldSelector"))
	assert.Equal(t, "100", query.Get("resourceVersion"))
	assert.Equal(t, "true", query.Get("allowWatchBookmarks"))
	assert.Equal(t, "55", query.Get("timeoutSeconds"))
	assert.Equal(t, "Bearer id:secret", header.Get("Authorization"))
	assert.Equal(t, httpx.UserAgent(), header.Get("User-Agent"))
}

// TestWatchObjectAsksForWholeSecondsFromOneToSixty asserts a timeout is sent
// rounded up to whole seconds, at least 1 and at most 60.
func TestWatchObjectAsksForWholeSecondsFromOneToSixty(t *testing.T) {
	for timeout, want := range map[time.Duration]string{
		100 * time.Millisecond: "1", 1500 * time.Millisecond: "2", 60 * time.Second: "60", 90 * time.Second: "60",
	} {
		t.Run(timeout.String(), func(t *testing.T) {
			srv := newStreamServer(t)
			var got string
			srv.onWatch = func(_ int, _ http.ResponseWriter, r *http.Request) { got = r.URL.Query().Get("timeoutSeconds") }
			ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", timeout)
			require.NoError(t, err)
			receive(t, ch)
			assert.Equal(t, want, got)
		})
	}
}

// TestWatchObjectMapsEvents asserts each event type of a stream: ADDED and
// MODIFIED are Changed with the object, DELETED is Deleted, BOOKMARK is a
// Bookmark, and an ERROR event is Failed with the Status classified, after
// which the stream ends.
func TestWatchObjectMapsEvents(t *testing.T) {
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
		for _, line := range []string{
			eventJSON("ADDED", clusterJSON("one", "101")),
			eventJSON("MODIFIED", clusterJSON("two", "102")),
			eventJSON("BOOKMARK", bookmarkJSON("103")),
			eventJSON("DELETED", clusterJSON("three", "104")),
			eventJSON("ERROR", json.RawMessage(goneStatus)),
		} {
			_, _ = io.WriteString(w, line)
			w.(http.Flusher).Flush()
		}
	}
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	require.NoError(t, err)

	for _, want := range []struct {
		typ   engine.ObjectEventType
		state string
	}{{engine.ObjectChanged, "one"}, {engine.ObjectChanged, "two"}} {
		ev, ok := receive(t, ch)
		require.True(t, ok)
		assert.Equal(t, want.typ, ev.Type)
		require.NotNil(t, ev.Object)
		assert.Equal(t, want.state, objectState(ev.Object))
	}
	ev, ok := receive(t, ch)
	require.True(t, ok)
	assert.Equal(t, engine.ObjectBookmark, ev.Type)
	ev, ok = receive(t, ch)
	require.True(t, ok)
	assert.Equal(t, engine.ObjectDeleted, ev.Type)
	assert.Equal(t, "three", objectState(ev.Object))
	ev, ok = receive(t, ch)
	require.True(t, ok)
	assert.Equal(t, engine.ObjectFailed, ev.Type)
	require.Error(t, ev.Err)
	status, answered := clierr.HTTPStatus(ev.Err)
	assert.True(t, answered)
	assert.Equal(t, http.StatusGone, status)
	_, ok = receive(t, ch)
	assert.False(t, ok, "the stream ends after an ERROR event")
}

// TestWatchObjectMapsOpenStatuses asserts the error of an open the server
// refuses, for an empty body, the API server's Status and Rancher's error
// object: 400, 405 and 500 with their status, 401 AUTH, 403 RBAC with no hint,
// 404 NOT_FOUND; and the verb watch in the message of the empty-body answer
// to a 401 and a 403.
func TestWatchObjectMapsOpenStatuses(t *testing.T) {
	for _, tt := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, ""},
		{http.StatusUnauthorized, clierr.CodeAuth},
		{http.StatusForbidden, clierr.CodeRBAC},
		{http.StatusNotFound, clierr.CodeNotFound},
		{http.StatusMethodNotAllowed, ""},
		{http.StatusInternalServerError, clierr.CodeGeneric},
	} {
		for _, body := range errorBodies {
			t.Run(fmt.Sprint(tt.status, " ", body.name), func(t *testing.T) {
				srv := newStreamServer(t)
				srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
					writeAnswer(w, tt.status, "", body.body(tt.status))
				}
				ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
				assert.Nil(t, ch)
				require.Error(t, err)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				if tt.code != "" {
					assert.Equal(t, tt.code, ce.Code)
				}
				status, answered := clierr.HTTPStatus(err)
				assert.True(t, answered)
				assert.Equal(t, tt.status, status)
				if tt.status == http.StatusForbidden {
					assert.Empty(t, ce.Hint)
				}
				if body.verb && (tt.status == http.StatusUnauthorized || tt.status == http.StatusForbidden) {
					assert.Contains(t, ce.Message, "watch clusters")
				}
			})
		}
	}
}

// TestWatchObjectFailsWhenTheOpenGetsNoHeaders asserts an open that gets no
// response headers fails at requestTimeout with the retryable "server may be
// slow" error and hands out no stream.
func TestWatchObjectFailsWhenTheOpenGetsNoHeaders(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(300 * time.Millisecond))
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, _ http.ResponseWriter, r *http.Request) { hang(r) }
	start := time.Now()
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	assert.Nil(t, ch)
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// lateTransport holds each watch response back until delay has passed, as a
// slow proxy does. It ignores the request's context, so the open timer cancels
// the request while the response is still on its way back, and the body it
// returns fails to read once that has happened, as a body of a cancelled
// request does.
type lateTransport struct {
	base  http.RoundTripper
	delay time.Duration
}

func (l lateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err == nil && req.URL.Query().Get("watch") == "true" {
		time.Sleep(l.delay)
		resp.Body = cutBody{ReadCloser: resp.Body, ctx: req.Context()}
	}
	return resp, err
}

// cutBody fails every read once its request's context has ended.
type cutBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b cutBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}

// lateWatchClient is a watching client whose open answer arrives after the
// open timer has fired, and has been warmed with one List so the answer is not
// held back by the handshake of a new connection: the timer is 600 ms and the
// answer is held until 1 s after it came in.
func lateWatchClient(t *testing.T, srv *streamServer) engine.ObjectWatcher {
	t.Helper()
	c := watcher(t, watchingClient(t, srv.srv, func(rc *rest.Config) {
		rc.Transport = lateTransport{base: rc.Transport, delay: time.Second}
	}))
	_, _, err := c.ListObject(t.Context(), watchGVR, "", "x")
	require.NoError(t, err)
	t.Cleanup(SetRequestTimeoutForTest(600 * time.Millisecond))
	return c
}

// TestWatchObjectHandsOutNoStreamWhenTheOpenTimerFiredAsItReturned asserts
// the race of the open timer and a response that arrives as it fires: the
// open fails, and no stream is handed out with a cancelled context.
func TestWatchObjectHandsOutNoStreamWhenTheOpenTimerFiredAsItReturned(t *testing.T) {
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, eventJSON("ADDED", clusterJSON("one", "101")))
		w.(http.Flusher).Flush()
	}
	ch, err := lateWatchClient(t, srv).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	assert.Nil(t, ch, "no stream is handed out")
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable)
}

// TestWatchObjectReadsAnAnswerThatArrivesAsTheOpenTimerFiresAsAGetDoes asserts
// what an open whose answer comes back as the timer fires gives: a 401 keeps
// its AUTH error, which ends a wait, and any other refusal, a 403 or a 429,
// reads as a Get reads a request the deadline cut after an answer: retryable,
// naming the status and that the request ran out of time.
func TestWatchObjectReadsAnAnswerThatArrivesAsTheOpenTimerFiresAsAGetDoes(t *testing.T) {
	for _, tt := range []struct {
		status int
		stops  bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, false},
		{http.StatusTooManyRequests, false},
	} {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			srv := newStreamServer(t)
			srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeAnswer(w, tt.status, "", errorBodies[1].body(tt.status))
			}
			ch, err := lateWatchClient(t, srv).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
			assert.Nil(t, ch)
			require.Error(t, err)
			status, answered := clierr.HTTPStatus(err)
			assert.True(t, answered, "the status of the answer is kept: %v", err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.stops, engine.WaitShouldStop(err), "%v", err)
			if !tt.stops {
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				assert.True(t, ce.Retryable)
				assert.Contains(t, ce.Message, "the request ran out of time")
			}
		})
	}
}

// TestWatchObjectEndsAStreamThatGoesSilentAtTheTimeoutPlusTheGrace asserts a
// stream that sent its headers and then nothing is ended by the client at
// timeoutSeconds plus the grace, and its channel closes.
func TestWatchObjectEndsAStreamThatGoesSilentAtTheTimeoutPlusTheGrace(t *testing.T) {
	setStreamGraceForTest(t, 300*time.Millisecond)
	srv := newStreamServer(t)
	start := time.Now()
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", time.Second)
	require.NoError(t, err)
	_, ok := receive(t, ch)
	assert.False(t, ok)
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 1300*time.Millisecond, "a deadline never fires early")
	assert.Less(t, elapsed, 5*time.Second)
}

// TestWatchObjectClosesTheChannelWhenTheContextEnds asserts the channel closes
// when the caller cancels its context, with the server still holding the
// stream.
func TestWatchObjectClosesTheChannelWhenTheContextEnds(t *testing.T) {
	srv := newStreamServer(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(ctx, watchGVR, "", "x", "100", 55*time.Second)
	require.NoError(t, err)
	cancel()
	_, ok := receive(t, ch)
	assert.False(t, ok)
}

// TestWatchObjectOfAResetConnectionEndsTheStream asserts what a connection
// reset during the open gives. client-go repeats the open on a reset ten times
// and then returns an empty watch and no error, so the call returns a closed
// channel at once and no error; on Windows the reset may read as an error
// client-go does not repeat, and the call then returns an error that is not an
// authentication or certificate failure. Either way no wait ends on it. The
// repeats take about ten seconds.
func TestWatchObjectOfAResetConnectionEndsTheStream(t *testing.T) {
	t.Parallel()
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, _ http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) }
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	if testfs.OnWindows() && err != nil {
		assert.Nil(t, ch)
		assert.False(t, engine.WaitShouldStop(err), "a reset is not an authentication or certificate failure: %v", err)
		return
	}
	require.NoError(t, err)
	_, ok := receive(t, ch)
	assert.False(t, ok, "the channel is closed")
}

// TestWatchObjectOpenHasTheBudgetOfAGetFor429 asserts the open waits for 429
// answers within the call's budget as a Get does, then fails with the 429
// named.
func TestWatchObjectOpenHasTheBudgetOfAGetFor429(t *testing.T) {
	t.Cleanup(kube.SetRetryLimitsForTest(time.Second, time.Second))
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAnswer(w, http.StatusTooManyRequests, "120", cloudflareBody)
	}
	ch, err := watcher(t, watchingClient(t, srv.srv, nil)).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	assert.Nil(t, ch)
	_ = assertAnswered(t, err, http.StatusTooManyRequests)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.Equal(t, 2, srv.watches, "one request and one retry within the budget")
}

// recordingWarnings is a client-go warning handler that keeps the text.
type recordingWarnings struct {
	mu   sync.Mutex
	text []string
}

func (r *recordingWarnings) HandleWarningHeader(_ int, _ string, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text = append(r.text, text)
}

// TestWatchObjectAndListObjectHandleWarningsAsAGetDoes asserts a Warning
// header on the List and on the watch open goes to the same handler of the
// client's configuration as one on a Get.
func TestWatchObjectAndListObjectHandleWarningsAsAGetDoes(t *testing.T) {
	srv := newStreamServer(t)
	warn := func(w http.ResponseWriter) { w.Header().Set("Warning", `299 - "deprecated version"`) }
	srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) { warn(w); srv.writeList(w, "idle") }
	srv.onGet = func(_ int, w http.ResponseWriter, _ *http.Request) {
		warn(w)
		b, _ := json.Marshal(clusterJSON("idle", "50"))
		writeAnswer(w, http.StatusOK, "", string(b))
	}
	srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) { warn(w) }
	rec := &recordingWarnings{}
	c := watchingClient(t, srv.srv, func(rc *rest.Config) { rc.WarningHandler = rec })

	_, err := c.Get(t.Context(), watchGVR, "", "x")
	require.NoError(t, err)
	_, _, err = watcher(t, c).ListObject(t.Context(), watchGVR, "", "x")
	require.NoError(t, err)
	ch, err := watcher(t, c).WatchObject(t.Context(), watchGVR, "", "x", "100", 55*time.Second)
	require.NoError(t, err)
	receive(t, ch)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, []string{"deprecated version", "deprecated version", "deprecated version"}, rec.text)
}

// setStreamGraceForTest sets the time a stream may outlive its timeoutSeconds
// until the test ends.
func setStreamGraceForTest(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamGraceNanos.Swap(int64(d))
	t.Cleanup(func() { streamGraceNanos.Store(old) })
}

// shortStreams shortens the streams of a wait run to 1 second and the grace
// to the same, so a run against the server takes seconds.
func shortStreams(t *testing.T) {
	t.Helper()
	t.Cleanup(engine.SetStreamSecondsForTest(1, 1))
}

// holdFor sends the response headers, holds the stream open for d and then
// returns, so the handler goes on to write; it aborts the connection when the
// client has gone before.
func holdFor(d time.Duration, w http.ResponseWriter, r *http.Request) {
	w.(http.Flusher).Flush()
	release := make(chan struct{})
	timer := time.AfterFunc(d, func() { close(release) })
	defer timer.Stop()
	testtransport.HangUntilReleased(r, release)
}

// TestWaitObjectAgainstTheServerStreamEndings asserts the calls of a wait
// against a real server for the rows that matter at the wire, with an
// interval of 200 ms and streams of 1 second: the calls stay within the
// bound, at most one stream is open, each watch starts from the version of
// the list before it, and after the third failed watch the wait sends only
// Gets, an interval apart.
func TestWaitObjectAgainstTheServerStreamEndings(t *testing.T) {
	// The subtests run in parallel, each against its own server; the parent
	// restores the stream length after the last of them ends.
	shortStreams(t)
	const interval = 200 * time.Millisecond
	tests := []struct {
		name  string
		setup func(s *streamServer)
		// pollsAfter is whether the wait ends up polling.
		pollsAfter bool
	}{
		{"200, one ERROR, close", func(s *streamServer) {
			s.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, eventJSON("ERROR", json.RawMessage(goneStatus)))
			}
		}, true},
		{"open 500", func(s *streamServer) {
			s.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeAnswer(w, http.StatusInternalServerError, "", `{}`)
			}
		}, true},
		{"200, one bookmark, close", func(s *streamServer) {
			s.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, eventJSON("BOOKMARK", bookmarkJSON("101")))
			}
		}, true},
		{"idle object, a watch from the item's own version answers 410", func(s *streamServer) {
			s.onWatch = func(_ int, w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("resourceVersion") == "50" {
					_, _ = io.WriteString(w, eventJSON("ERROR", json.RawMessage(goneStatus)))
					return
				}
				holdFor(time.Second, w, r)
				_, _ = io.WriteString(w, eventJSON("BOOKMARK", bookmarkJSON("999")))
			}
		}, false},
		{"healthy streams", func(s *streamServer) {
			s.onWatch = func(_ int, w http.ResponseWriter, r *http.Request) {
				holdFor(time.Second, w, r)
				_, _ = io.WriteString(w, eventJSON("BOOKMARK", bookmarkJSON("999")))
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newStreamServer(t)
			tt.setup(srv)
			res := waitFor(t, watchingClient(t, srv.srv, nil), 3500*time.Millisecond, interval)
			var to *timedOutError
			require.ErrorAs(t, res.err, &to, "the wait ends by its timeout: %v", res.err)
			srv.requireInvariants(interval)

			calls := srv.snapshot()
			gets := 0
			for _, c := range calls {
				if c.kind == "get" {
					gets++
				}
			}
			if !tt.pollsAfter {
				assert.Zero(t, gets, "the wait never falls back to polling: %s", srv.sequence())
				assert.GreaterOrEqual(t, srv.watches, 2, "streams follow one another: %s", srv.sequence())
				return
			}
			require.Equal(t, 3, srv.watches, "three failed watches: %s", srv.sequence())
			assert.Positive(t, gets, "the wait polls after the third failed watch: %s", srv.sequence())
			var polls []serverCall
			for i, c := range calls {
				if i >= len(calls)-gets && c.kind == "get" {
					polls = append(polls, c)
				}
			}
			for i := 1; i < len(polls); i++ {
				assert.GreaterOrEqual(t, polls[i].at-polls[i-1].at, interval-20*time.Millisecond, "the Gets are an interval apart")
			}
		})
	}
}

// TestWaitObjectAgainstTheServerAfterADelete asserts a DELETED event at once
// ends the stream, and the next cycle's empty List and the Get's 404 end the
// wait with NOT_FOUND.
func TestWaitObjectAgainstTheServerAfterADelete(t *testing.T) {
	shortStreams(t)
	srv := newStreamServer(t)
	srv.onList = func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			srv.writeList(w, "idle")
			return
		}
		srv.writeEmptyList(w)
	}
	srv.onGet = func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAnswer(w, http.StatusNotFound, "", notFoundBody)
	}
	srv.onWatch = func(_ int, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, eventJSON("DELETED", clusterJSON("idle", "101")))
	}
	res := waitFor(t, watchingClient(t, srv.srv, nil), 10*time.Second, 200*time.Millisecond)
	ce, ok := errors.AsType[*clierr.Error](res.err)
	require.True(t, ok, "%v", res.err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "list watch list get", srv.sequence())
	srv.requireInvariants(200 * time.Millisecond)
}

// TestWaitObjectAgainstTheServerReturnsOnAnEvent asserts a satisfying event
// ends the wait at once with the event's object.
func TestWaitObjectAgainstTheServerReturnsOnAnEvent(t *testing.T) {
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, eventJSON("MODIFIED", clusterJSON("done", "101")))
		testtransport.HoldStreamAndAbort(w, r)
	}
	res := waitFor(t, watchingClient(t, srv.srv, nil), 30*time.Second, 200*time.Millisecond)
	require.NoError(t, res.err)
	require.NotNil(t, res.done)
	assert.Equal(t, "101", res.done.GetResourceVersion())
	assert.Less(t, res.elapsed, 5*time.Second)
	assert.Equal(t, "list watch", srv.sequence())
}

// TestWaitObjectAgainstTheServerOpenThatNeverAnswersEndsInTime asserts an
// open that gets no response headers fails at requestTimeout and the wait
// still ends by its own timeout, not later, with the third failed open
// turning the wait to polling.
func TestWaitObjectAgainstTheServerOpenThatNeverAnswersEndsInTime(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(300 * time.Millisecond))
	shortStreams(t)
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, _ http.ResponseWriter, r *http.Request) { hang(r) }
	res := waitFor(t, watchingClient(t, srv.srv, nil), 3*time.Second, 200*time.Millisecond)
	var to *timedOutError
	require.ErrorAs(t, res.err, &to)
	assert.Less(t, res.elapsed, 3*time.Second+2*time.Second, "the wait ends by its timeout")
	srv.requireInvariants(200 * time.Millisecond)
}

// TestWaitObjectAgainstTheServerStreamThatStallsEndsInTime asserts a stream
// that sent its headers and then nothing is ended by the client at its
// timeoutSeconds plus the grace, so the next cycle starts: the wait opens
// more than one stream before its timeout.
func TestWaitObjectAgainstTheServerStreamThatStallsEndsInTime(t *testing.T) {
	setStreamGraceForTest(t, 200*time.Millisecond)
	shortStreams(t)
	srv := newStreamServer(t)
	res := waitFor(t, watchingClient(t, srv.srv, nil), 4*time.Second, 200*time.Millisecond)
	var to *timedOutError
	require.ErrorAs(t, res.err, &to)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.GreaterOrEqual(t, srv.watches, 2, "the client ended the stalled stream: %d watches", srv.watches)
}

// TestWaitObjectAgainstTheServerEventAfterTheRequestTimeoutStillSucceeds
// asserts a stream is not bound by the 30 second request timeout: an event
// that comes after requestTimeout, here shortened, still ends the wait with
// success.
func TestWaitObjectAgainstTheServerEventAfterTheRequestTimeoutStillSucceeds(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(300 * time.Millisecond))
	srv := newStreamServer(t)
	srv.onWatch = func(_ int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		holdFor(900*time.Millisecond, w, r)
		_, _ = io.WriteString(w, eventJSON("MODIFIED", clusterJSON("done", "101")))
		w.(http.Flusher).Flush()
		testtransport.HoldStreamAndAbort(w, r)
	}
	res := waitFor(t, watchingClient(t, srv.srv, nil), 30*time.Second, 200*time.Millisecond)
	require.NoError(t, res.err)
	require.NotNil(t, res.done)
	assert.Equal(t, "list watch", srv.sequence())
}

// TestWaitObjectAgainstTheServerPollsWhenTheListIsUnusable asserts a wait
// whose List holds no resourceVersion, or several items, opens no watch and
// takes the object from the Get that follows in the same cycle, as it does
// after a refused List.
func TestWaitObjectAgainstTheServerPollsWhenTheListIsUnusable(t *testing.T) {
	for name, body := range map[string]string{
		"no resourceVersion": listJSON("", clusterJSON("idle", "50")),
		"several items":      listJSON("100", clusterJSON("idle", "50"), clusterJSON("idle", "51")),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newStreamServer(t)
			srv.onList = func(_ int, w http.ResponseWriter, _ *http.Request) { writeAnswer(w, http.StatusOK, "", body) }
			srv.onGet = func(_ int, w http.ResponseWriter, _ *http.Request) {
				b, _ := json.Marshal(clusterJSON("done", "60"))
				writeAnswer(w, http.StatusOK, "", string(b))
			}
			res := waitFor(t, watchingClient(t, srv.srv, nil), 3*time.Second, time.Second)
			require.NoError(t, res.err)
			require.NotNil(t, res.done)
			assert.Equal(t, "60", res.done.GetResourceVersion(), "the object is the Get's")
			assert.Equal(t, "list get", srv.sequence())
		})
	}
}
