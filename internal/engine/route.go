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
// choose from. ClusterRequiredError builds the refusal from a message, so
// every command gives the same code and hint. The messages below it are the
// texts of the commands that word their refusal differently.
const (
	ClusterRequiredMessage = "--cluster is required; Rancher's own namespaces (cattle-system, cattle-tokens, cattle-fleet-system) live on the local cluster, pass --cluster local"

	// ClusterRequiredPlainMessage is the refusal for a command that takes
	// neither --namespace nor --all-namespaces, where the local-cluster
	// advice about Rancher's own namespaces does not apply.
	ClusterRequiredPlainMessage = "--cluster is required; pass a cluster id or name"

	// ClusterOrAllClustersRequiredMessage is the refusal for a command that
	// takes either one cluster or every cluster, given neither.
	ClusterOrAllClustersRequiredMessage = "--cluster or --all-clusters is required; pass --cluster <id|name> for one cluster, or --all-clusters for every cluster you can list"
)

// ClusterRequiredHint lists the clusters of the config directory's current
// server (or of RANCHER_CLI_URL when it is set), which is the server every
// command runs against.
var ClusterRequiredHint = clierr.NewCommand("access clusters")

// DownstreamClusterRequiredMessage is the refusal for a target that runs
// only on a downstream cluster, given no --cluster.
func DownstreamClusterRequiredMessage(target string) string {
	return "--cluster is required; " + target + " runs only on a downstream cluster, pass a downstream cluster id or name"
}

// NamedClusterRequiredMessage is the refusal for the read of one object of a
// cluster-scoped kind by its argName (id or name), given no --cluster.
func NamedClusterRequiredMessage(argName, kind string) string {
	return "--cluster is required when you give the " + argName + " of one " + kind
}

// ClusterRequiredError is the refusal of an absent --cluster: VALIDATION with
// msg and the hint that lists the clusters. It is the one place a command's
// own code builds such an error; a command with its own wording passes it as
// msg. The pipeline builds the refusal of a required --cluster from the
// declaration's MissingMessage and MissingHint.
func ClusterRequiredError(msg string) *clierr.Error {
	return clierr.New(clierr.CodeValidation, msg).WithHintCommand(ClusterRequiredHint)
}

// ClusterFlag declares --cluster with the missing-cluster text that fits the
// command: ClusterRequiredMessage when it takes --namespace or
// --all-namespaces (namespaced), ClusterRequiredPlainMessage otherwise. The
// text and hint are set whether or not the flag is required, because a
// caller may mark it required later. A caller with its own description
// sets Desc on the result.
func ClusterFlag(required, namespaced bool) runtime.ArgSpec {
	return runtime.ArgSpec{Name: FlagCluster, Type: "string", Required: required, Desc: "target cluster `id|name`",
		MissingMessage: clusterRequiredMessage(namespaced), MissingHint: ClusterRequiredHint.Field()}
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
		return "", ClusterRequiredError(clusterRequiredMessage(namespaced))
	}
	return clients.ResolveCluster(ctx, target)
}
