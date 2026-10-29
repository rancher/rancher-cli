package provider

import (
	"context"
	"errors"
	"net/http"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

// mapErr translates a Kubernetes API error into a structured *clierr.Error so the
// pipeline renders it with the right exit code, for a call through the
// local cluster's proxy that names no object. Returns nil for nil.
func mapErr(err error) error {
	return mapRequestErr(err, &clierr.Request{Cluster: engine.LocalCluster})
}

// mapRequestErr is mapErr for the call q describes; q is nil when there is
// no request to name. A server status goes through clierr's one table; a
// hung request and any other error are this client's.
func mapRequestErr(err error, q *clierr.Request) error {
	if err == nil {
		return nil
	}
	if ce := clierr.FromStatus(err, q); ce != nil {
		return ce
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A hung request expires the client timeout without any HTTP status -
		// client-go hands back a url.Error wrapping context.DeadlineExceeded.
		// This arm serves management-plane reads and downstream agent-tunnel
		// reads alike, and the two causes are indistinguishable here, so the
		// message names both; which cluster to inspect is unknown here, so
		// no hint names one.
		return clierr.Retryable(clierr.CodeGeneric,
			err.Error()+" - the server may be slow, or, if the target is a downstream cluster, its agent may be disconnected (check with: "+InspectClusterRemedy+")")
	}
	return clierr.From(err)
}

// InspectClusterRemedy is the command a timed-out read names: the
// cluster's own state.
var (
	InspectClusterRemedy = clierr.Rancher + " inspect cluster <cluster-id>"
)

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
