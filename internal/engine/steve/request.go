// Package steve reads Rancher's steve API (/v1 and /k8s/clusters/<id>/v1)
// as the caller: one request type, a getter the caller's credential sends,
// the collection page protocol, and the project lookup the command
// pipeline uses for --project.
package steve

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"github.com/rancher/rancher-cli/internal/engine"
)

// Request is one steve GET: Cluster "" is /v1/..., a cluster id is
// /k8s/clusters/<id>/v1/...; Namespace and Name select a namespaced
// collection or one object; Continue is the previous page's token.
type Request struct{ Cluster, Type, Namespace, Name, Continue string }

// Path is the request's server-relative path and query.
func (r Request) Path() string {
	p := "/v1/" + url.PathEscape(r.Type)
	if r.Cluster != "" {
		p = "/k8s/clusters/" + url.PathEscape(r.Cluster) + p
	}
	if r.Namespace != "" {
		p += "/" + url.PathEscape(r.Namespace)
	}
	if r.Name != "" {
		p += "/" + url.PathEscape(r.Name)
	}
	if r.Continue != "" {
		p += "?continue=" + url.QueryEscape(r.Continue)
	}
	return p
}

// Getter sends one request with the caller's credential and returns the
// body; a non-2xx answer is an error whose chain holds *StatusError.
type Getter func(ctx context.Context, r Request) ([]byte, error)

// StatusError is the HTTP status of a non-2xx steve answer.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return "http status " + strconv.Itoa(e.Status) }

// RootGetter sends requests through the CLI's root client. A non-2xx
// answer keeps the root client's error and adds *StatusError to its chain.
func RootGetter(c engine.RootClient) Getter {
	return func(ctx context.Context, r Request) ([]byte, error) {
		body, err := c.GetJSON(ctx, r.Path())
		if err == nil {
			return body, nil
		}
		if rs, ok := errors.AsType[*engine.RootStatusError](err); ok {
			return nil, statusError{err: err, status: &StatusError{Status: rs.Status}}
		}
		return nil, err
	}
}

// statusError is a getter's error: the transport's own error, with the
// status beside it in the chain.
type statusError struct {
	err    error
	status *StatusError
}

func (e statusError) Error() string   { return e.err.Error() }
func (e statusError) Unwrap() []error { return []error{e.err, e.status} }
