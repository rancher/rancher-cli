package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// FlagCluster is the --cluster flag name. It lives here (rather than as a
// literal) so every package that resolves a downstream cluster - resources'
// generated routing and the projects package's bespoke commands - shares one
// constant instead of drifting copies.
const FlagCluster = runtime.FlagCluster

// ClusterRequiredMessage and ClusterRequiredHint are the one refusal for an
// absent --cluster, whether the pipeline rejects it as a missing required
// flag or a resolver reaches the empty value. Routing is never implied, not
// even for Rancher's own namespaces (cattle-system, cattle-tokens,
// cattle-fleet-system), which live on the local cluster: the message says
// so and names the value, the hint lists the clusters to choose from.
const (
	ClusterRequiredMessage = "--cluster is required; Rancher's own namespaces (cattle-system, cattle-tokens, cattle-fleet-system) live on the local cluster, pass --cluster local"
	ClusterRequiredHint    = clierr.Rancher + " access clusters"
)

// ResolveDownstreamCluster resolves --cluster for a command that always
// targets a downstream cluster: an explicit flag is resolved via
// ResolveCluster; an empty flag is a VALIDATION error - routing is never
// filled from stored state. Shared by resources.Route's Downstream branches
// and the projects package's bespoke commands (create namespace, project
// add/remove-namespace) - those live in a package resources already imports
// (Route's project-membership validation), so this helper lives in engine,
// which both can reach without an import cycle.
func ResolveDownstreamCluster(ctx context.Context, clients ClusterClientFactory, in runtime.Input) (string, error) {
	target := in.String(FlagCluster)
	if target == "" {
		if err := runtime.MissingClusterError(ctx); err != nil {
			return "", err
		}
		return "", clierr.New(clierr.CodeValidation, ClusterRequiredMessage).WithHint(ClusterRequiredHint)
	}
	return clients.ResolveCluster(ctx, target)
}
