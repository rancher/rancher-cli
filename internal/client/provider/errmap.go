package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// mapErr translates a Kubernetes API error into a structured *clierr.Error so the
// pipeline renders it with the right exit code, for a call through the
// local cluster's proxy that names no object. ctx is the context of the
// call, which holds what the server answered; method is the call's HTTP
// method; eff names the server. Returns nil for nil.
func mapErr(ctx context.Context, method string, eff config.Effective, err error) error {
	return mapRequestErr(ctx, method, err, &clierr.Request{Cluster: engine.LocalCluster, Server: config.DisplayServerURL(eff.URL)})
}

// mapRequestErr is mapErr for the call q describes; q is nil when there is
// no request to name. method is the HTTP method of the call, empty when the
// caller has none to name. A server status goes through clierr's one table;
// a request that ran out of time and any other error are this client's.
func mapRequestErr(ctx context.Context, method string, err error, q *clierr.Request) error {
	return mapRequestErrWithin(ctx, method, requestTimeout, err, q)
}

// mapRequestErrWithin is mapRequestErr for a call bounded by bound, the time
// a deadline error names; zero for a call with no bound of its own, which
// names none.
func mapRequestErrWithin(ctx context.Context, method string, bound time.Duration, err error, q *clierr.Request) error {
	if err == nil {
		return nil
	}
	if ce := clierr.FromStatus(err, q); ce != nil {
		return ce
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if ce := answeredThenTimedOut(ctx, method, bound, q); ce != nil {
			return ce
		}
		// With no response received the request hung, and client-go hands
		// back a url.Error wrapping context.DeadlineExceeded. Through the
		// local cluster, or with no cluster named, that is a slow server. A
		// downstream cluster's agent may be disconnected as well, and that
		// cluster is the one to inspect.
		if q != nil && q.Cluster != "" && q.Cluster != engine.LocalCluster {
			return clierr.Retryable(clierr.CodeGeneric,
				clierr.Text(err.Error()+" - the server may be slow, or its agent may be disconnected",
					clierr.Lead("; check the cluster with: "), inspectClusterCommand(q.Cluster)))
		}
		return clierr.Retryable(clierr.CodeGeneric, err.Error()+" - the server may be slow; retry later")
	}
	return clierr.From(err)
}

// answeredThenTimedOut is the error for a call whose deadline ended it while
// the request in flight held a 400 or above answer: a 429 whose wait the
// deadline cut. The status is what the server said, so it comes first. Nil
// when that request had no answer: one that ran out of time says nothing of
// an earlier request's answer. bound is the time the call was bounded by,
// zero to name none. The cause carries the status for clierr.HTTPStatus.
func answeredThenTimedOut(ctx context.Context, method string, bound time.Duration, q *clierr.Request) *clierr.Error {
	status, ok := kube.LastAnsweredStatus(ctx)
	if !ok || status < http.StatusBadRequest {
		return nil
	}
	limit := ""
	if bound > 0 {
		limit = " (" + bound.String() + ")"
	}
	return clierr.Retryable(clierr.CodeGeneric,
		clierr.AnsweredLead(q, status)+" and the request ran out of time"+limit+" - retry later").
		WithCause(clierr.ResponseError(method, status, nil))
}

// inspectClusterCommand is the command a timed-out read through a downstream
// cluster names: the cluster's own state.
func inspectClusterCommand(cluster string) clierr.Command {
	return clierr.NewCommand("inspect cluster").Arg(cluster)
}

// restoreStatus rebuilds a raw request's error from the answer's body
// with the one decoder hand-read answers use (clierr.ResponseError):
// client-go builds a raw request's error from the body only when it is
// text, so the JSON Status an API server answers a refused pods/log read
// with, or Rancher's JSON error object, would read "unknown". An empty
// body is rebuilt too, as an answer with no reason, so it does not keep
// the request suffix client-go adds to its own text. An error without a
// status is left as it was.
func restoreStatus(err error, body []byte) error {
	status, ok := clierr.HTTPStatus(err)
	if !ok {
		return err
	}
	return clierr.ResponseError(http.MethodGet, status, body)
}
