// Package steve reads Rancher's steve API (/v1 and /k8s/clusters/<id>/v1)
// as the caller: one request type, a getter the caller's credential sends,
// the collection page protocol, and the project lookup the command
// pipeline uses for --project.
package steve

import (
	"context"
	"net/url"

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
// body; a non-2xx answer is an error whose status clierr.HTTPStatus reads.
type Getter func(ctx context.Context, r Request) ([]byte, error)

// RootGetter sends requests through the CLI's root client, whose error
// for a non-2xx answer holds the status (clierr.HTTPStatus).
func RootGetter(c engine.RootClient) Getter {
	return func(ctx context.Context, r Request) ([]byte, error) {
		return c.GetJSON(ctx, r.Path())
	}
}

// RootFactoryGetter is RootGetter over the root client roots hands out,
// asked for at each request (the factory memoizes it), so a command that
// reads no steve object never needs one.
func RootFactoryGetter(roots engine.RootClientFactory) Getter {
	return func(ctx context.Context, r Request) ([]byte, error) {
		root, err := roots.Root()
		if err != nil {
			return nil, err
		}
		return RootGetter(root)(ctx, r)
	}
}
