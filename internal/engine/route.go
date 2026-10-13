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

// FlagNamespace is the --namespace flag's name.
const FlagNamespace = "namespace"

// ClusterRequiredMessage, ClusterRequiredPlainMessage and
// ClusterRequiredHint are the refusal for an absent --cluster, whether the
// pipeline rejects it as a missing required flag or a resolver reaches the
// empty value. Routing is never implied, not even for Rancher's own
// namespaces (cattle-system, cattle-tokens, cattle-fleet-system), which live
// on the local cluster: ClusterRequiredMessage, for commands that take a
// namespace, says so and names the value; the hint lists the clusters to
// choose from.
const (
	ClusterRequiredMessage = "--cluster is required; Rancher's own namespaces (cattle-system, cattle-tokens, cattle-fleet-system) live on the local cluster, pass --cluster local"
	ClusterRequiredHint    = clierr.Rancher + " access clusters"

	// ClusterRequiredPlainMessage is the refusal for a command that takes
	// neither --namespace nor --all-namespaces, where the local-cluster
	// advice about Rancher's own namespaces does not apply.
	ClusterRequiredPlainMessage = "--cluster is required; pass a cluster id or name"
)

// ClusterFlag declares --cluster with the missing-cluster text that fits the
// command: ClusterRequiredMessage when it takes --namespace or
// --all-namespaces (namespaced), ClusterRequiredPlainMessage otherwise. The
// text and hint are set whether or not the flag is required, because a
// caller may mark it required later. A caller with its own description
// sets Desc on the result.
func ClusterFlag(required, namespaced bool) runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagCluster, Type: "string", Required: required, Desc: "target cluster `id|name`",
		MissingMessage: clusterRequiredMessage(namespaced), MissingHint: ClusterRequiredHint}
}

// NamespaceFlag declares --namespace with kubectl's -n. A caller with its
// own description sets Desc on the result.
func NamespaceFlag(required bool) runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagNamespace, Type: "string", Required: required,
		Aliases: []string{"n"}, Desc: "`namespace` to operate in"}
}

// clusterRequiredMessage is the missing-cluster refusal for a command that
// does (namespaced) or does not take --namespace or --all-namespaces.
func clusterRequiredMessage(namespaced bool) string {
	if namespaced {
		return ClusterRequiredMessage
	}
	return ClusterRequiredPlainMessage
}

// ResolveDownstreamCluster resolves --cluster for a command that always
// targets a downstream cluster: an explicit flag is resolved via
// ResolveCluster; an empty flag is a VALIDATION error - routing is never
// filled from stored state. Shared by resources.Route's Downstream branches
// and the projects package's bespoke commands (create namespace, project
// add/remove-namespace) - those live in a package resources already imports
// (Route's project-membership validation), so this helper lives in engine,
// which both can reach without an import cycle.
//
// namespaced says whether the calling command takes --namespace or
// --all-namespaces; it selects the text of the refusal.
func ResolveDownstreamCluster(ctx context.Context, clients ClusterClientFactory, in runtime.Input, namespaced bool) (string, error) {
	target := in.String(FlagCluster)
	if target == "" {
		if err := runtime.MissingClusterError(ctx); err != nil {
			return "", err
		}
		return "", clierr.New(clierr.CodeValidation, clusterRequiredMessage(namespaced)).WithHint(ClusterRequiredHint)
	}
	return clients.ResolveCluster(ctx, target)
}
