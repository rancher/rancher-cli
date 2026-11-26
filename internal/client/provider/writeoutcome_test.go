package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// outcomeGVR is the resource the write calls of this file write: configmaps,
// a Downstream kind of the table, whose read command names --cluster and
// --namespace.
var outcomeGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// outcomeObject is the configmap web in team-a, at resourceVersion 7.
func outcomeObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "web", "namespace": "team-a", "resourceVersion": "7"},
	}}
}

// writeCall is one write method of the client: the verb its request names,
// whether its caller cannot get the object, and the call.
type writeCall struct {
	verb       string
	unreadable bool
	call       func(ctx context.Context, c engine.ClusterClient, dryRun bool) error
}

// writeCalls has a row for every write method of engine.ClusterClient and
// engine.UIDDeleter.
var writeCalls = map[string]writeCall{
	"Create": {"create", false, func(ctx context.Context, c engine.ClusterClient, dryRun bool) error {
		_, err := c.Create(ctx, outcomeGVR, "team-a", outcomeObject(), dryRun)
		return err
	}},
	"Update": {"update", false, func(ctx context.Context, c engine.ClusterClient, dryRun bool) error {
		_, err := c.Update(ctx, outcomeGVR, "team-a", outcomeObject(), dryRun)
		return err
	}},
	"Delete": {"delete", false, func(ctx context.Context, c engine.ClusterClient, dryRun bool) error {
		return c.Delete(ctx, outcomeGVR, "team-a", "web", "7", dryRun)
	}},
	"DeleteWithUID": {"delete", true, func(ctx context.Context, c engine.ClusterClient, dryRun bool) error {
		d, ok := c.(engine.UIDDeleter)
		if !ok {
			return errors.New("the client does not delete with a uid precondition")
		}
		_, err := d.DeleteWithUID(ctx, outcomeGVR, "team-a", "web", "u-1", dryRun)
		return err
	}},
}

// readMethods are the methods of engine.ClusterClient that change nothing on
// the server, each with the reason.
var readMethods = map[string]string{
	"Get":             "reads an object",
	"List":            "reads objects",
	"Logs":            "reads a container's log",
	"PostReview":      "posts a review, which the server responds to without storing an object",
	"ServerResources": "reads the discovery documents",
}

// TestEveryClientMethodIsAReadOrHasAnOutcomeRow asserts, by reflection,
// that every method of engine.ClusterClient and engine.UIDDeleter is either
// in readMethods or has a row in writeCalls, and that both lists name only
// methods that exist. A method added to either interface fails here until it
// is put in one of them.
func TestEveryClientMethodIsAReadOrHasAnOutcomeRow(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, it := range []reflect.Type{reflect.TypeFor[engine.ClusterClient](), reflect.TypeFor[engine.UIDDeleter]()} {
		for m := range it.Methods() {
			seen[m.Name] = true
			_, read := readMethods[m.Name]
			_, write := writeCalls[m.Name]
			assert.True(t, read != write, "%s.%s is in exactly one of readMethods and writeCalls", it.Name(), m.Name)
		}
	}
	for name := range readMethods {
		assert.True(t, seen[name], "readMethods names %s, which is no method", name)
	}
	for name := range writeCalls {
		assert.True(t, seen[name], "writeCalls names %s, which is no method", name)
	}
}

// closeConnection hijacks the connection of the request and closes it
// without a response; with reset it closes it with a TCP reset. It runs on
// the server's goroutine, which cannot fail the test: a hijack that fails
// aborts the request, which the client also sees as a connection closed
// without a response.
func closeConnection(w http.ResponseWriter, reset bool) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if tcp, ok := conn.(*net.TCPConn); ok && reset {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

// respondAfterBody reads the request body, then responds with status, content
// type and body.
func respondAfterBody(status int, contentType, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// kubernetesStatus is the JSON of a metav1.Status with status code, reason
// and message.
func kubernetesStatus(code int, reason, message string) string {
	return `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":` + strconv.Quote(message) +
		`,"reason":"` + reason + `","code":` + strconv.Itoa(code) + `}`
}

// kubernetesStatusWithCause is kubernetesStatus with the cause client-go
// records for a body it did not decode as a Status, which an API server built
// on k8s.io/apiserver sends unchanged when it returns such an error.
func kubernetesStatusWithCause(code int, reason, message string) string {
	return strings.Replace(kubernetesStatus(code, reason, message), `"reason"`,
		`"details":{"causes":[{"reason":"UnexpectedServerResponse","message":"`+message+`"}]},"reason"`, 1)
}

// statusOnly reads the request body and responds with status, no header but
// the status line and no body.
func statusOnly(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
	}
}

// outcomeServers are the ways a server ends a write, by name, each with what
// the server responds (status is the status the cause names: the HTTP status,
// or the code of a Kubernetes Status body, which wins; 0 for a connection that
// closes) and the cause the error words.
var outcomeServers = map[string]struct {
	handler http.HandlerFunc
	status  int
	cause   string
}{
	"the connection closes before the body is read": {func(w http.ResponseWriter, _ *http.Request) { closeConnection(w, false) }, 0, "the connection failed"},
	"the connection closes after the body is read": {func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		closeConnection(w, false)
	}, 0, "the connection failed"},
	"the connection is reset after the body is read": {func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		closeConnection(w, true)
	}, 0, "the connection failed"},
	"a proxy responds 502 with text": {respondAfterBody(502, "text/plain", "bad gateway\n"),
		502, "the server responded with 502: bad gateway"},
	"a proxy responds 504 with text": {respondAfterBody(504, "text/plain", "gateway timeout\n"),
		504, "the server responded with 504: gateway timeout"},
	"a proxy responds 502 with no text": {respondAfterBody(502, "text/plain", ""),
		502, "the server responded with 502"},
	"the API server responds with a Status 502": {respondAfterBody(502, "application/json", kubernetesStatus(502, "BadGateway", "the upstream closed the connection")),
		502, "the server responded with 502: the upstream closed the connection"},
	"the API server responds with a Status 504": {respondAfterBody(504, "application/json", kubernetesStatus(504, "Timeout", "Timeout: request did not complete within the requested timeout")),
		504, "the server responded with 504: Timeout: request did not complete within the requested timeout"},
	"a proxy responds 502 with a Status that names 503": {respondAfterBody(502, "application/json", kubernetesStatus(503, "ServiceUnavailable", "the upstream is unavailable")),
		503, "the server responded with 503: the upstream is unavailable"},
	"a proxy responds 504 with a Status that names 500": {respondAfterBody(504, "application/json", kubernetesStatus(500, "InternalError", "the upstream failed")),
		500, "the server responded with 500: the upstream failed"},
	"the API server responds with a Status 500 that carries the unexpected-server-response cause": {respondAfterBody(500, "application/json", kubernetesStatusWithCause(500, "InternalError", "the upstream failed")),
		500, "the server responded with 500: the upstream failed"},
	"the API server responds with a Status 503 that carries the unexpected-server-response cause": {respondAfterBody(503, "application/json", kubernetesStatusWithCause(503, "ServiceUnavailable", "the upstream is unavailable")),
		503, "the server responded with 503: the upstream is unavailable"},
	"a proxy responds 500 with an empty body": {respondAfterBody(500, "text/plain", ""),
		500, "the server responded with 500"},
	"a proxy responds 503 with an empty body": {respondAfterBody(503, "text/plain", ""),
		503, "the server responded with 503"},
	"a proxy responds 500 with an HTML page": {respondAfterBody(500, "text/html", "<html><body>Internal Server Error</body></html>\n"),
		500, "the server responded with 500: <html><body>Internal Server Error</body></html>"},
	"a proxy responds 503 with an HTML page": {respondAfterBody(503, "text/html", "<html><body>Service Unavailable</body></html>\n"),
		503, "the server responded with 503: <html><body>Service Unavailable</body></html>"},
	"a proxy responds 500 with no body and no content type": {statusOnly(500),
		500, "the server responded with 500"},
	"a proxy responds 503 with no body and no content type": {statusOnly(503),
		503, "the server responded with 503"},
	"a proxy responds 500 with text": {respondAfterBody(500, "text/plain", "Post \"https://10.0.0.1/api\": EOF\n"),
		500, "the server responded with 500: Post \"https://10.0.0.1/api\": EOF"},
	"a proxy responds 503 with text": {respondAfterBody(503, "text/plain", "error trying to reach service: EOF\n"),
		503, "the server responded with 503: error trying to reach service: EOF"},
	"Rancher responds 500 with a JSON object that is not a Status": {respondAfterBody(500, "application/json", `{"type":"error","status":500,"message":"proxy failed"}`),
		500, "the server responded with 500"},
	"Rancher responds 503 with a JSON object that is not a Status": {respondAfterBody(503, "application/json", `{"type":"error","status":503,"message":"service unavailable"}`),
		503, "the server responded with 503"},
}

// outcomeClient is a client of cluster for srv.
func outcomeClient(t *testing.T, srv *httptest.Server, cluster string) engine.ClusterClient {
	t.Helper()
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, cluster)
	require.NoError(t, err)
	rc.Transport = srv.Client().Transport
	c, err := NewClusterClient(rc, cluster)
	require.NoError(t, err)
	return c
}

// agentClause is what the cause of a response with status through a
// downstream cluster adds: for a 502 or 504 the words clierr's status table
// gives such a response, for any other status nothing.
func agentClause(cluster string, status int) string {
	if status != http.StatusBadGateway && status != http.StatusGatewayTimeout {
		return ""
	}
	return " through cluster '" + cluster + "': its agent may not be registered yet"
}

// assertOutcomeUnknown asserts err is the error of the write wc through
// cluster whose request was sent and whose result is unknown, worded with
// cause: the get command of the object in the message and the hint, or, for
// a write of an object the caller cannot get, the list of its kind in the
// message and no hint.
func assertOutcomeUnknown(t *testing.T, err error, wc writeCall, cluster, cause string) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.True(t, ce.OutcomeUnknown, ce.Message)
	assert.False(t, ce.Retryable)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	head := "the request to " + wc.verb + " configmaps 'web' was sent, but whether the server applied the change is unknown (" + cause + "); "
	if wc.unreadable {
		assert.Equal(t, head+"you cannot get the object: before you run the command again, ask a user who can get it to check whether the change is on the server, or list the objects of its kind if you can: "+
			clierr.Rancher+" get configmap --cluster '"+cluster+"' --namespace 'team-a'", ce.Message)
		assert.Empty(t, ce.Hint)
		return
	}
	read := clierr.Rancher + " get configmap 'web' --cluster '" + cluster + "' --namespace 'team-a'"
	assert.Equal(t, head+"before you run the command again, check with: "+read, ce.Message)
	assert.Equal(t, read, ce.Hint)
}

// TestAWriteWhoseResponseIsLostSaysItMayHaveBeenApplied asserts, for every
// write method: when the server reads the request and then closes or resets
// the connection, or responds 502 or 504 with any body (a Kubernetes Status
// that names another code too: the status line decides), or 500 or 503 with a
// body that is not a Kubernetes Status (text, an empty body, an HTML page, no
// body, a JSON object that is not a Status, or a Status that carries
// client-go's unexpected-server-response cause, which reads the same, on the
// cautious side), the error is OutcomeUnknown, GENERIC and not retryable, and names
// the get command of the object (the list of its kind for a delete with a uid
// precondition, whose caller cannot get it); a 502 or 504 through a
// downstream cluster also names the cluster and says its agent may not be
// registered yet, and a 500 or 503 says nothing about the cluster. The same
// response to a dry run is classified as before, without OutcomeUnknown.
func TestAWriteWhoseResponseIsLostSaysItMayHaveBeenApplied(t *testing.T) {
	t.Parallel()
	for method, wc := range writeCalls {
		for name, s := range outcomeServers {
			for _, cluster := range []string{"c-1", engine.LocalCluster} {
				t.Run(method+": "+name+" through "+cluster, func(t *testing.T) {
					t.Parallel()
					srv := httptest.NewServer(s.handler)
					t.Cleanup(srv.Close)
					c := outcomeClient(t, srv, cluster)
					cause := s.cause
					if cluster != engine.LocalCluster {
						cause += agentClause(cluster, s.status)
					}
					assertOutcomeUnknown(t, wc.call(t.Context(), c, false), wc, cluster, cause)

					dry := wc.call(t.Context(), c, true)
					require.Error(t, dry)
					assert.False(t, clierr.From(dry).OutcomeUnknown, "a dry run changes nothing: %v", dry)
				})
			}
		}
	}
}

// TestAWriteWhoseResponseIsAKubernetesStatusKeepsItsError asserts, for every
// write method, that a 500 or 503 whose body is a Kubernetes Status (a failed
// admission webhook call, the API server's own 503) is the API server's own
// response: the error is not OutcomeUnknown and keeps the classification of
// clierr's status table, through the local cluster and through c-1.
func TestAWriteWhoseResponseIsAKubernetesStatusKeepsItsError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		retryable bool
		message   string
	}{
		{"a Status 500", 500, kubernetesStatus(500, "InternalError", `Internal error occurred: failed calling webhook "check.example.com": failed to call webhook: connection refused`), false, "failed calling webhook"},
		{"a Status 503", 503, kubernetesStatus(503, "ServiceUnavailable", "etcdserver: request timed out"), true, "etcdserver: request timed out"},
	} {
		for method, wc := range writeCalls {
			for _, cluster := range []string{"c-1", engine.LocalCluster} {
				t.Run(tc.name+": "+method+" through "+cluster, func(t *testing.T) {
					t.Parallel()
					srv := httptest.NewServer(respondAfterBody(tc.status, "application/json", tc.body))
					t.Cleanup(srv.Close)
					err := wc.call(t.Context(), outcomeClient(t, srv, cluster), false)
					require.Error(t, err)
					ce := clierr.From(err)
					assert.False(t, ce.OutcomeUnknown, ce.Message)
					assert.Equal(t, clierr.CodeGeneric, ce.Code)
					assert.Equal(t, tc.retryable, ce.Retryable, ce.Message)
					assert.Contains(t, ce.Message, tc.message)
					assert.NotContains(t, ce.Message, "was sent")
					status, ok := clierr.HTTPStatus(err)
					assert.True(t, ok)
					assert.Equal(t, tc.status, status)
				})
			}
		}
	}
}

// TestAWriteThatWasNotSentKeepsItsError asserts, for every write method, that
// a request whose connection was refused is not OutcomeUnknown: nothing was
// written.
func TestAWriteThatWasNotSentKeepsItsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	c := outcomeClient(t, srv, "c-1")
	srv.Close()
	for method, wc := range writeCalls {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			err := wc.call(t.Context(), c, false)
			require.Error(t, err)
			ce := clierr.From(err)
			assert.False(t, ce.OutcomeUnknown, ce.Message)
			assert.NotContains(t, ce.Message, "was sent")
		})
	}
}

// TestMapWriteErrOfARequestThatWasNotWrittenKeepsItsError asserts the
// mapping of a write whose request the transport did not write: a timeout
// stays the retryable timeout of a read, and a failed connection stays
// GENERIC; neither is OutcomeUnknown.
func TestMapWriteErrOfARequestThatWasNotWrittenKeepsItsError(t *testing.T) {
	t.Parallel()
	r := &dynamicClient{clusterID: "c-1"}
	w := write{method: http.MethodPost, gvr: outcomeGVR, ns: "team-a", name: "web", written: func() bool { return false }}
	q := r.request("create", outcomeGVR, "team-a", "web")
	timeout := r.mapWriteErr(t.Context(), w, &url.Error{Op: "Post", URL: "https://h", Err: context.DeadlineExceeded}, q)
	ce := clierr.From(timeout)
	assert.False(t, ce.OutcomeUnknown, ce.Message)
	assert.True(t, ce.Retryable, ce.Message)

	lost := clierr.From(r.mapWriteErr(t.Context(), w, &url.Error{Op: "Post", URL: "https://h", Err: io.EOF}, q))
	assert.False(t, lost.OutcomeUnknown, lost.Message)
}

// TestMapWriteErrKeepsAnErrorAlreadyClassified asserts that a written
// request whose error is already a *clierr.Error, such as a certificate the
// transport refused, keeps that error.
func TestMapWriteErrKeepsAnErrorAlreadyClassified(t *testing.T) {
	t.Parallel()
	r := &dynamicClient{clusterID: "c-1"}
	w := write{method: http.MethodPost, gvr: outcomeGVR, ns: "team-a", name: "web", written: func() bool { return true }}
	classified := clierr.New(clierr.CodeTLSTrust, "the server's certificate is not trusted")
	got := r.mapWriteErr(t.Context(), w, classified, r.request("create", outcomeGVR, "team-a", "web"))
	assert.Same(t, classified, got)
}

// TestALostResponseToACreateThatStoresNothingKeepsItsError asserts that a
// SelfUser create, which the server responds to without storing an object,
// is not OutcomeUnknown when its response is lost.
func TestALostResponseToACreateThatStoresNothingKeepsItsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(outcomeServers["the connection closes after the body is read"].handler)
	t.Cleanup(srv.Close)
	_, err := outcomeClient(t, srv, "c-1").Create(t.Context(), selfuser.GVR, "", selfuser.New(), false)
	require.Error(t, err)
	ce := clierr.From(err)
	assert.False(t, ce.OutcomeUnknown, ce.Message)
	assert.NotContains(t, ce.Message, "was sent")
}

// TestAWriteThatRunsOutOfTimeAfterItWasSentSaysItMayHaveBeenApplied asserts,
// for every write method, that a request the server read and never responded
// to is OutcomeUnknown and not retryable once the request timeout ends it,
// and that a dry run keeps the retryable timeout. It does not run in
// parallel: it shrinks the request timeout.
func TestAWriteThatRunsOutOfTimeAfterItWasSentSaysItMayHaveBeenApplied(t *testing.T) {
	t.Cleanup(SetRequestTimeoutForTest(500 * time.Millisecond))
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hang(r) }))
	t.Cleanup(srv.Close)
	c := outcomeClient(t, srv, "c-1")
	for method, wc := range writeCalls {
		t.Run(method, func(t *testing.T) {
			assertOutcomeUnknown(t, wc.call(t.Context(), c, false), wc, "c-1", "the request timed out")

			ce := clierr.From(wc.call(t.Context(), c, true))
			assert.False(t, ce.OutcomeUnknown, ce.Message)
			assert.True(t, ce.Retryable, ce.Message)
		})
	}
}

// TestACancelledWriteSaysItMayHaveBeenApplied asserts that a write stopped
// after the server read its request is OutcomeUnknown with the stop as its
// cause, and that the cancellation is still seen as one.
func TestACancelledWriteSaysItMayHaveBeenApplied(t *testing.T) {
	t.Parallel()
	read := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(read)
		testtransport.HangAndAbort(r)
	}))
	t.Cleanup(srv.Close)
	c := outcomeClient(t, srv, "c-1")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-read
		cancel()
	}()
	err := writeCalls["Create"].call(ctx, c, false)
	assertOutcomeUnknown(t, err, writeCalls["Create"], "c-1", "the command was stopped")
	assert.ErrorIs(t, err, context.Canceled)
}

// http2Server starts a TLS server for handler that speaks HTTP/2, with the
// header limit maxHeader when it is not zero, and returns it with a count of
// the requests that did not come over HTTP/2.
func http2Server(t *testing.T, handler http.HandlerFunc, maxHeader int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var notH2 atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			notH2.Add(1)
		}
		handler(w, r)
	}))
	srv.EnableHTTP2 = true
	if maxHeader > 0 {
		srv.Config.MaxHeaderBytes = maxHeader
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &notH2
}

// TestAnHTTP2WriteSaysItMayHaveBeenAppliedOnlyOnceItsBodyWasSent asserts the
// HTTP/2 cases. A stream the server resets after it read the body, and a 504
// after it read the body, are OutcomeUnknown. A 504 that arrives while the
// client is still sending a body the server never read stops the body: the
// request was not sent whole, and the error is the retryable 504. Headers
// larger than the limit the server announced are never written, and the
// error is not OutcomeUnknown.
func TestAnHTTP2WriteSaysItMayHaveBeenAppliedOnlyOnceItsBodyWasSent(t *testing.T) {
	t.Parallel()
	readThen := func(after func(w http.ResponseWriter)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			after(w)
		}
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		cause   string
	}{
		{"a stream reset after the body is read", readThen(func(http.ResponseWriter) { panic(http.ErrAbortHandler) }), "the connection failed"},
		{"a 504 after the body is read", readThen(func(w http.ResponseWriter) {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		}), "the server responded with 504: gateway timeout" + agentClause("c-1", http.StatusGatewayTimeout)},
	} {
		for _, method := range []string{"Create", "Delete"} {
			t.Run(tc.name+": "+method, func(t *testing.T) {
				t.Parallel()
				srv, notH2 := http2Server(t, tc.handler, 0)
				assertOutcomeUnknown(t, writeCalls[method].call(t.Context(), outcomeClient(t, srv, "c-1"), false), writeCalls[method], "c-1", tc.cause)
				assert.Zero(t, notH2.Load(), "every request came over HTTP/2")
			})
		}
	}

	t.Run("a 504 before a large body is read", func(t *testing.T) {
		t.Parallel()
		srv, notH2 := http2Server(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		}, 0)
		big := outcomeObject()
		big.Object["data"] = map[string]any{"blob": strings.Repeat("a", 8<<20)}
		_, err := outcomeClient(t, srv, "c-1").Create(t.Context(), outcomeGVR, "team-a", big, false)
		ce := clierr.From(err)
		assert.False(t, ce.OutcomeUnknown, ce.Message)
		assert.True(t, ce.Retryable, ce.Message)
		status, ok := clierr.HTTPStatus(err)
		assert.True(t, ok)
		assert.Equal(t, http.StatusGatewayTimeout, status)
		assert.Zero(t, notH2.Load(), "every request came over HTTP/2")
	})

	t.Run("headers larger than the server's limit", func(t *testing.T) {
		t.Parallel()
		var seen atomic.Int32
		srv, notH2 := http2Server(t, func(w http.ResponseWriter, r *http.Request) {
			seen.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			http.NotFound(w, r)
		}, 1024)
		c := outcomeClient(t, srv, "c-1")
		// A first request reads the server's settings, its header limit among
		// them.
		_, _ = c.Get(t.Context(), outcomeGVR, "team-a", "web")
		err := c.Delete(t.Context(), outcomeGVR, "team-a", strings.Repeat("w", 4000), "7", false)
		require.Error(t, err)
		ce := clierr.From(err)
		assert.False(t, ce.OutcomeUnknown, ce.Message)
		assert.NotContains(t, ce.Message, "was sent")
		assert.Equal(t, int32(1), seen.Load(), "the server saw the first request only: the delete was never written")
		assert.Zero(t, notH2.Load(), "every request came over HTTP/2")
	})
}
