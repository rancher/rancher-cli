// Package observe implements curated Rancher-component log reads (`logs
// <target>`) and the API-reachable support-bundle export, sharing one target
// table and one per-pod log fetcher.
package observe

import (
	"context"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Target describes one curated log source: a fixed (or, for fleet-agent,
// dynamically resolved) namespace/selector/container on either the local or
// a downstream cluster.
type Target struct {
	Name           string
	LocalOnly      bool
	DownstreamOnly bool
	Namespace      string // static default; "" when Resolve computes it dynamically (fleet-agent)
	Selector       string
	Container      string // documented default container; "" lists every container
}

// Targets is the curated table (design spec LS-1), verbatim: namespaces,
// selectors, and containers are source-verified against rancher/rancher —
// do not re-derive them.
var Targets = []Target{
	{Name: "server", LocalOnly: true, Namespace: "cattle-system", Selector: "app=rancher"},
	{Name: "agent", DownstreamOnly: true, Namespace: "cattle-system", Selector: "app=cattle-cluster-agent", Container: "cluster-register"},
	{Name: "fleet-agent", Selector: "app=fleet-agent"},
	{Name: "kube-api-auth", DownstreamOnly: true, Namespace: "cattle-system", Selector: "app=kube-api-auth", Container: "kube-api-auth"},
	{Name: "webhook", LocalOnly: true, Namespace: "cattle-system", Selector: "app=rancher-webhook"},
}

// Lookup resolves a target by name.
func Lookup(name string) (Target, bool) {
	for _, t := range Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// Names returns every target name, in table order (for the command's usage
// text and unknown-target hints).
func Names() []string {
	out := make([]string, len(Targets))
	for i, t := range Targets {
		out[i] = t.Name
	}
	return out
}

// Resolved is the concrete, cluster-bound result of resolving a Target for
// one invocation: the client to fetch logs through, the cluster it targets,
// the namespace pods were found in, and the pods themselves — possibly
// empty; the caller (the logs command) decides whether an empty result is
// fatal, since "zero pods" and "every fetch failed" collapse to the same
// yield-nothing outcome.
type Resolved struct {
	Client    engine.ClusterClient
	ClusterID string
	Namespace string
	Pods      []unstructured.Unstructured
}

const (
	fleetWorkspaceLocal      = "fleet-local"
	fleetWorkspaceDownstream = "fleet-default"
	fleetNamespaceLocal      = "cattle-fleet-local-system"
	fleetNamespaceDownstream = "cattle-fleet-system"
	fleetNamespaceLegacy     = "fleet-system"

	dockerInstallHint = "no rancher pods on the local cluster — single-node Docker install? " +
		"server logs live in `docker logs <container>`"
	nonACEHint = "cluster has no authorized cluster endpoint (kube-api-auth runs only on ACE clusters)"
)

// Resolve maps a Target and the invocation's --cluster flag onto a concrete
// cluster, namespace, and pod list, running every target-specific
// degradation chain from the design spec (LS-1): the server selector
// fallback ending in the Docker-install message, the kube-api-auth ACE
// precheck, and the fleet-agent namespace resolution chain.
func Resolve(ctx context.Context, clients engine.ClusterClientFactory, t Target, in runtime.Input) (Resolved, error) {
	clusterID, err := resolveTargetCluster(ctx, clients, t, in)
	if err != nil {
		return Resolved{}, err
	}
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return Resolved{}, err
	}

	if t.Name == "kube-api-auth" {
		local, err := clients.Cluster(ctx, engine.LocalCluster)
		if err != nil {
			return Resolved{}, err
		}
		ok, err := hasACE(ctx, local, clusterID)
		if err != nil {
			return Resolved{}, err
		}
		if !ok {
			return Resolved{}, clierr.New(clierr.CodeNotFound, nonACEHint)
		}
	}

	switch t.Name {
	case "server":
		ns, pods, err := resolveServerPods(ctx, client)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Client: client, ClusterID: clusterID, Namespace: ns, Pods: pods}, nil
	case "fleet-agent":
		local, err := clients.Cluster(ctx, engine.LocalCluster)
		if err != nil {
			return Resolved{}, err
		}
		ns, pods, err := resolveFleetAgentPods(ctx, local, client, clusterID)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Client: client, ClusterID: clusterID, Namespace: ns, Pods: pods}, nil
	default:
		podKind, _ := kinds.Lookup("pod")
		list, err := client.List(ctx, podKind.GVR, t.Namespace, t.Selector)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Client: client, ClusterID: clusterID, Namespace: t.Namespace, Pods: list.Items}, nil
	}
}

// resolveTargetCluster validates LocalOnly/DownstreamOnly against the
// invocation's --cluster flag and resolves the concrete cluster id.
// fleet-agent (neither flag set) defaults to local when --cluster is
// omitted, matching its "downstream OR local" table entry.
func resolveTargetCluster(ctx context.Context, clients engine.ClusterClientFactory, t Target, in runtime.Input) (string, error) {
	clusterFlag := in.String(engine.FlagCluster)
	switch {
	case t.LocalOnly:
		if clusterFlag != "" {
			resolved, err := clients.ResolveCluster(ctx, clusterFlag)
			if err != nil {
				return "", err
			}
			if resolved != engine.LocalCluster {
				return "", clierr.New(clierr.CodeValidation, fmt.Sprintf("%s is a local-only target", t.Name)).
					WithHint("omit --cluster, or pass --cluster local")
			}
		}
		return engine.LocalCluster, nil
	case t.DownstreamOnly:
		resolved, err := engine.ResolveDownstreamCluster(ctx, clients, in)
		if err != nil {
			return "", err
		}
		if resolved == engine.LocalCluster {
			return "", clierr.New(clierr.CodeValidation, fmt.Sprintf("%s only runs on a downstream cluster", t.Name)).
				WithHint("pass --cluster <id|name>")
		}
		return resolved, nil
	default:
		if clusterFlag == "" {
			return engine.LocalCluster, nil
		}
		return clients.ResolveCluster(ctx, clusterFlag)
	}
}

// hasACE reports whether clusterID's management Cluster object has an
// authorized cluster endpoint enabled (spec.localClusterAuthEndpoint.enabled;
// false or absent is non-ACE).
func hasACE(ctx context.Context, local engine.ClusterClient, clusterID string) (bool, error) {
	clusterKind, _ := kinds.Lookup("cluster")
	obj, err := local.Get(ctx, clusterKind.GVR, "", clusterID)
	if err != nil {
		return false, err
	}
	enabled, _, _ := unstructured.NestedBool(obj.Object, "spec", "localClusterAuthEndpoint", "enabled")
	return enabled, nil
}

// resolveServerPods runs the server target's selector fallback chain: the
// chart-owned app=rancher selector, then the rancher Deployment's own
// spec.selector, then the Docker-install degradation.
func resolveServerPods(ctx context.Context, client engine.ClusterClient) (string, []unstructured.Unstructured, error) {
	const ns = "cattle-system"
	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, ns, "app=rancher")
	if err != nil {
		return "", nil, err
	}
	if len(list.Items) > 0 {
		return ns, list.Items, nil
	}
	deployKind, _ := kinds.Lookup("deployment")
	if dep, err := client.Get(ctx, deployKind.GVR, ns, "rancher"); err == nil {
		if sel, serr := selectorFromDeployment(dep); serr == nil {
			if list2, lerr := client.List(ctx, podKind.GVR, ns, sel); lerr == nil && len(list2.Items) > 0 {
				return ns, list2.Items, nil
			}
		}
	}
	return "", nil, clierr.New(clierr.CodeNotFound, dockerInstallHint)
}

// resolveFleetAgentPods runs the fleet-agent namespace resolution chain
// (LS-1): the management Cluster's spec.fleetWorkspaceName, the fleetcluster
// named after the provisioning identity in that workspace (Rancher keys
// fleet Clusters by the provisioning name, not the mgmt id), its
// spec.agentNamespace, falling through to the local/downstream defaults and
// finally the legacy fleet-system namespace when no pods match. Every read
// in the chain degrades silently to the next default on error/absence —
// per the design spec, "no-permission" is a fall-through, not a failure.
func resolveFleetAgentPods(ctx context.Context, local, client engine.ClusterClient, clusterID string) (string, []unstructured.Unstructured, error) {
	isLocal := clusterID == engine.LocalCluster

	workspace := fleetWorkspaceDownstream
	if isLocal {
		workspace = fleetWorkspaceLocal
	}
	clusterKind, _ := kinds.Lookup("cluster")
	if obj, err := local.Get(ctx, clusterKind.GVR, "", clusterID); err == nil {
		if ws, found, _ := unstructured.NestedString(obj.Object, "spec", "fleetWorkspaceName"); found && ws != "" {
			workspace = ws
		}
	}

	fleetClusterName := "local"
	if !isLocal {
		fleetClusterName = ""
		provKind, _ := kinds.Lookup("provisioningcluster")
		if list, err := local.List(ctx, provKind.GVR, workspace, ""); err == nil {
			for i := range list.Items {
				if cn, _, _ := unstructured.NestedString(list.Items[i].Object, "status", "clusterName"); cn == clusterID {
					fleetClusterName = list.Items[i].GetName()
					break
				}
			}
		}
	}

	ns := fleetNamespaceDownstream
	if isLocal {
		ns = fleetNamespaceLocal
	}
	if fleetClusterName != "" {
		fleetKind, _ := kinds.Lookup("fleetcluster")
		if fc, err := local.Get(ctx, fleetKind.GVR, workspace, fleetClusterName); err == nil {
			if agentNS, found, _ := unstructured.NestedString(fc.Object, "spec", "agentNamespace"); found && agentNS != "" {
				ns = agentNS
			}
		}
	}

	podKind, _ := kinds.Lookup("pod")
	list, err := client.List(ctx, podKind.GVR, ns, "app=fleet-agent")
	if err != nil {
		return "", nil, err
	}
	if len(list.Items) > 0 {
		return ns, list.Items, nil
	}
	if legacy, err := client.List(ctx, podKind.GVR, fleetNamespaceLegacy, "app=fleet-agent"); err == nil && len(legacy.Items) > 0 {
		return fleetNamespaceLegacy, legacy.Items, nil
	}
	return ns, list.Items, nil
}

// selectorFromDeployment decodes a Deployment's spec.selector.matchLabels
// into a comma-joined label selector string ("k1=v1,k2=v2"). matchExpressions
// are not modeled — the one chart-owned Deployment this branch reads
// (rancher) uses plain matchLabels.
func selectorFromDeployment(dep *unstructured.Unstructured) (string, error) {
	matchLabels, found, _ := unstructured.NestedStringMap(dep.Object, "spec", "selector", "matchLabels")
	if !found || len(matchLabels) == 0 {
		return "", fmt.Errorf("deployment %s has no spec.selector.matchLabels", dep.GetName())
	}
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sel := ""
	for i, k := range keys {
		if i > 0 {
			sel += ","
		}
		sel += k + "=" + matchLabels[k]
	}
	return sel, nil
}
