package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// projectResolver is the pipeline's --project lookup over the invocation's
// client factory: steve as the caller, the GET by id through the local
// cluster.
type projectResolver struct{ f *clientFactory }

func (r projectResolver) ResolveProject(ctx context.Context, in runtime.Input, value string) (runtime.Project, string, error) {
	root, err := r.f.Root()
	if err != nil {
		return runtime.Project{}, "", err
	}
	get := steve.RootGetter(root)
	sp, warning, err := steve.ResolveProjectFlag(ctx, get, r.f, in.String(engine.FlagCluster), value)
	if err != nil {
		return runtime.Project{}, "", err
	}
	clusterID, projectID := sp.Cluster, sp.ID
	return runtime.Project{Cluster: clusterID, ID: projectID, DisplayName: sp.DisplayName,
		Namespaces: func(ctx context.Context) (map[string]bool, error) {
			downstream, err := r.f.Cluster(ctx, clusterID)
			if err != nil {
				return nil, err
			}
			return steve.ProjectNamespaces(ctx, downstream, get, clusterID, projectID)
		}}, warning, nil
}
