package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// completionGetter is a steve.Getter over a completion Connection at the
// server root: the collections /v1/<type> and /v1/<type>/<namespace>, page
// by page, and one object /v1/<type>/<name>. Every request counts against
// c's allowance, and every segment passes completion.ValidSegment. A
// request through a cluster's proxy, or for one namespaced object, has no
// completion path.
func completionGetter(c *completion.Connection) steve.Getter {
	return func(ctx context.Context, r steve.Request) ([]byte, error) {
		if r.Cluster != "" || (r.Name != "" && r.Namespace != "") {
			return nil, fmt.Errorf("completion: no path for steve request %+v", r)
		}
		var p completion.Path
		var err error
		switch {
		case r.Name != "":
			p, err = completion.SteveObjectPath(r.Type, r.Name)
		case r.Namespace == "":
			p, err = completion.SteveCollectionPath(r.Type)
		default:
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

// listClusters is the cluster lister tracedConnection sets: the steve
// clusters collection in one walk, read as the command reads it. A
// collection that moves under the read is an error at once, so a TAB costs
// one walk at most.
func listClusters(ctx context.Context, c *completion.Connection) ([]completion.ClusterItem, error) {
	list, err := steve.ClustersOnce(ctx, completionGetter(c))
	if err != nil {
		return nil, err
	}
	out := make([]completion.ClusterItem, len(list))
	for i, cl := range list {
		out[i] = completion.ClusterItem{ID: cl.ID, DisplayName: cl.DisplayName}
	}
	return out, nil
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
