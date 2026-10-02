package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// completionGetter is a steve.Getter over a completion Connection for
// collections at the server root, page by page: /v1/<type> and
// /v1/<type>/<namespace>. Every request counts against c's allowance, and
// every segment passes completion.ValidSegment. A request for one object
// or through a cluster's proxy has no completion path yet.
func completionGetter(c *completion.Connection) steve.Getter {
	return func(ctx context.Context, r steve.Request) ([]byte, error) {
		if r.Cluster != "" || r.Name != "" {
			return nil, fmt.Errorf("completion: no path for steve request %+v", r)
		}
		var p completion.Path
		var err error
		if r.Namespace == "" {
			p, err = completion.SteveCollectionPath(r.Type)
		} else {
			p, err = completion.SteveNamespacedCollectionPath(r.Type, r.Namespace)
		}
		if err != nil {
			return nil, err
		}
		if r.Continue != "" {
			p = p.WithQuery("continue", r.Continue)
		}
		body, err := c.GetBody(ctx, p)
		if se, ok := errors.AsType[*completion.StatusError](err); ok {
			return nil, &steve.StatusError{Status: se.Status}
		}
		return body, err
	}
}

// listProjects is the project lister tracedConnection sets: the steve
// project collection of one cluster, read as the command reads it.
func listProjects(ctx context.Context, c *completion.Connection, clusterID string) ([]completion.ProjectItem, error) {
	list, err := steve.Projects(ctx, completionGetter(c), clusterID)
	if err != nil {
		return nil, err
	}
	out := make([]completion.ProjectItem, len(list))
	for i, p := range list {
		out[i] = completion.ProjectItem{ID: p.ID, DisplayName: p.DisplayName}
	}
	return out, nil
}
