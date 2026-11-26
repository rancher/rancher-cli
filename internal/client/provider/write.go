package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// traceWrite returns ctx with a trace that records whether net/http finished
// writing a whole request of the call, its body included, and the function
// that reports it. client-go sends a write once: it repeats only a GET after
// a connection error. An error after the request was written may come
// after the server applied the change. The hook runs when the request is
// written into net/http's buffer, before the buffer is flushed to the
// connection: a flush that fails is treated as written, and the result is
// reported as unknown. A request whose write failed was not sent whole, and
// the server cannot apply it; net/http may send it again on a new connection,
// and that write is recorded too. Over HTTP/2 the hook is not called when the headers
// could not be written, and is called with an error when a response that
// arrives first stops the body: both read as not written. It runs after the
// body is written, and a response can arrive before it: a status can be
// mapped before the hook runs, whatever the status, and then reads as not
// written.
func traceWrite(ctx context.Context) (context.Context, func() bool) {
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				written.Store(true)
			}
		},
	})
	return ctx, written.Load
}

// write is one write call of the client, as mapWriteErr words its error:
// the object it writes, whether it is a dry run, whether the transport
// wrote its request, and whether the caller cannot get the object (a delete
// with a uid precondition), which leaves no get command to name.
type write struct {
	method     string
	gvr        schema.GroupVersionResource
	ns, name   string
	dryRun     bool
	written    func() bool
	unreadable bool
}

// storesNothing is the set of resources whose create the server responds to
// without storing an object: a SelfUser create returns the user the request
// runs as and keeps nothing. A lost response to such a create changed
// nothing.
var storesNothing = map[schema.GroupResource]bool{selfuser.GVR.GroupResource(): true}

// mapWriteErr is mapRequestErr for a write. It returns clierr.UnknownOutcome
// for a write that is not a dry run, writes a resource the server stores,
// and whose request was written, when the call ended without a response that
// says whether the server applied it: the connection failed, the call timed
// out or was stopped, or the server responded 502 or 504, or 500 or 503 with
// a body that is not a Kubernetes Status. A proxy between the CLI and the
// API server sends such a response when the request may have reached the API
// server. Every other error keeps mapRequestErr's classification: a dry run
// changes nothing, and a request that was not written cannot have reached the
// server.
func (r *dynamicClient) mapWriteErr(ctx context.Context, w write, err error, q *clierr.Request) error {
	if err == nil {
		return nil
	}
	if w.dryRun || storesNothing[w.gvr.GroupResource()] || !w.written() || !outcomeUnknown(ctx, w.method, err, q) {
		return mapRequestErr(ctx, w.method, err, q)
	}
	cause := clierr.OutcomeCause(err)
	if status, ok := clierr.HTTPStatus(err); ok && gatewayStatus(status) && q.Cluster != "" && q.Cluster != engine.LocalCluster {
		// clierr words a 502 or 504 through a downstream cluster with "its
		// agent may not be registered yet"; the cause keeps those words for
		// those two statuses and makes no other claim about the agent.
		cause += " through cluster " + clierr.ShellQuote(q.Cluster) + ": its agent may not be registered yet"
	}
	if w.unreadable {
		return clierr.UnknownOutcomeUnreadable(sentText(q), cause, err, r.listCommand(w))
	}
	return clierr.UnknownOutcome(sentText(q), cause, err, r.readCommand(w))
}

// gatewayStatus reports 502 and 504, the statuses a proxy or an API server
// that ran past its time limit responds with after the request may have
// reached the API server, whatever the body.
func gatewayStatus(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusGatewayTimeout
}

// responseStatus is the HTTP status of the response err holds: the status
// line of the last response of the call when ctx records one, else the status
// the error carries. A Kubernetes Status body names its own code, which
// clierr.HTTPStatus prefers, and a proxy can send a Status whose code differs
// from the status line.
func responseStatus(ctx context.Context, err error) (int, bool) {
	status, ok := clierr.HTTPStatus(err)
	if line, answered := kube.LastAnsweredStatus(ctx); ok && answered {
		return line, true
	}
	return status, ok
}

// outcomeUnknown reports whether err, the error of a write whose request was
// written, leaves open whether the server applied it. A 502 or 504 does, with
// any body, including a Kubernetes Status that names another code: the status
// line decides. A 500 or 503 does when its body is not a Kubernetes Status:
// Rancher's cluster proxy and the Kubernetes aggregation layer in front of
// ext.cattle.io respond so to a failed round trip, whether or not the request
// reached the API server. A 500 or 503 that is a Kubernetes Status is the API
// server's own response and says it did not apply the change, as does any
// other HTTP status. A Status that carries client-go's unexpected-server-
// response cause (an API server built on k8s.io/apiserver sends the details
// of such an error unchanged) reads as not a Kubernetes Status: the write is
// reported as unknown, the cautious side. A deadline that cut the wait for a
// 429 retry is that 429 response, not a timeout of the write. An error
// already classified as a *clierr.Error is left to mapRequestErr.
func outcomeUnknown(ctx context.Context, method string, err error, q *clierr.Request) bool {
	if status, ok := responseStatus(ctx, err); ok {
		switch status {
		case http.StatusInternalServerError, http.StatusServiceUnavailable:
			return clierr.NotAKubernetesStatus(err)
		}
		return gatewayStatus(status)
	}
	if _, ok := errors.AsType[*clierr.Error](err); ok {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return answeredThenTimedOut(ctx, method, requestTimeout, q) == nil
	}
	return true
}

// sentText names the request q describes: "the request to <verb>
// <resource>.<group>" and the object's name when it has one; the group is
// left out for the core group.
func sentText(q *clierr.Request) string {
	what := q.Resource
	if q.Group != "" {
		what += "." + q.Group
	}
	text := "the request to " + q.Verb + " " + what
	if q.Name != "" {
		text += " " + clierr.ShellQuote(q.Name)
	}
	return text
}

// readCommand is the get command that reads the object w writes, or lists the
// objects of its kind for a create with no name; the zero Command when the
// resource is not a kind of the table.
func (r *dynamicClient) readCommand(w write) clierr.Command {
	k, ok := kinds.ByGVR(w.gvr)
	if !ok {
		return clierr.Command{}
	}
	return kinds.ReadCommand(k, w.ns, w.name, r.clusterID)
}

// listCommand is the get command that lists the objects of the kind w
// writes, in its namespace; the zero Command when the resource is not a kind
// of the table.
func (r *dynamicClient) listCommand(w write) clierr.Command {
	k, ok := kinds.ByGVR(w.gvr)
	if !ok {
		return clierr.Command{}
	}
	return kinds.ReadCommand(k, w.ns, "", r.clusterID)
}
