package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// FlagCluster is the --cluster flag name. It lives here (rather than as a
// literal) so every package that resolves a downstream cluster — resources'
// generated routing and the projects package's bespoke commands — shares one
// constant instead of drifting copies.
const FlagCluster = "cluster"

// ResolveDownstreamCluster resolves --cluster for a command that always
// targets a downstream cluster: an explicit flag is resolved via
// ResolveCluster; an empty flag is a VALIDATION error — routing is never
// filled from stored state. Shared by resources.Route's Downstream branches
// and the projects package's bespoke commands (create namespace, project
// add/remove-namespace) — those live in a package resources already imports
// (Route's project-membership validation), so this helper lives in engine,
// which both can reach without an import cycle.
func ResolveDownstreamCluster(ctx context.Context, clients ClusterClientFactory, in runtime.Input) (string, error) {
	target := in.String(FlagCluster)
	if target == "" {
		return "", clierr.New(clierr.CodeValidation, "--cluster is required")
	}
	return clients.ResolveCluster(ctx, target)
}
