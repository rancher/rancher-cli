// Package observe implements curated Rancher-component log reads (`logs
// <target>`) and the API-reachable support-bundle export, sharing one target
// table and one per-pod log fetcher.
package observe

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/observe/complete"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Package-level kind vars, resolved once and fail-fast: a typo or removed
// kind name here would otherwise silently produce a zero-value Kind{}/GVR{},
// only discovered downstream via a confusing client.Get/List error far from
// the actual mistake. mustKind panics at init on a miss, so any test run (or
// `rancher` invocation) catches it immediately.
var (
	podKind     = mustKind("pod")
	clusterKind = mustKind("cluster")
	deployKind  = mustKind("deployment")
	provKind    = mustKind("provisioningcluster")
	fleetKind   = mustKind("fleetcluster")
	nodeKind    = mustKind("node")
	eventKind   = mustKind("event")
	settingKind = mustKind("setting")
	featureKind = mustKind("feature")
)

func mustKind(name string) kinds.Kind {
	k, ok := kinds.Lookup(name)
	if !ok {
		panic("observe: kind " + name + " not registered in the kinds table")
	}
	return k
}

// Target is complete.Target: the curated target table lives in the
// completion source's fenced package, and the command and the support
// bundle read it through these names.
type Target = complete.Target

// Targets is complete.Targets, the curated table.
var Targets = complete.Targets

// Lookup is complete.Lookup, a target by name.
func Lookup(name string) (Target, bool) { return complete.Lookup(name) }

// Names is complete.Names, every target name in table order.
func Names() []string { return complete.Names() }

// Resolved is the concrete, cluster-bound result of resolving a Target for
// one invocation: the client to fetch logs through, the cluster it targets,
// the namespace pods were found in, and the pods themselves - possibly
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

	dockerInstallHint = "no Rancher pods found on the local cluster; if Rancher runs as a single Docker container, " +
		"read the server logs with `docker logs <container>`"
	nonACEHint = "cluster has no Authorized Cluster Endpoint (kube-api-auth runs only on clusters that have one)"
)

// Resolve maps a Target and the invocation's --cluster flag onto a concrete
// cluster, namespace, and pod list, running every target-specific
// degradation chain: the server selector
// fallback ending in the Docker-install message, the kube-api-auth ACE
// precheck, and the fleet-agent namespace resolution chain.
func Resolve(ctx context.Context, clients engine.ClusterClientFactory, get steve.Getter, t Target, in runtime.Input) (Resolved, error) {
	clusterID, err := resolveTargetCluster(ctx, clients, t, in)
	if err != nil {
		return Resolved{}, err
	}
	return ResolveOnCluster(ctx, clients, get, t, clusterID)
}

// Applicable reports whether t can run against clusterID at all - a
// LocalOnly target against a downstream cluster, or a DownstreamOnly target
// against the local cluster, is never applicable. Callers that iterate a
// target × cluster matrix themselves (support-bundle) check this BEFORE
// calling ResolveOnCluster, since an inapplicable combination is a
// "skipped: not applicable" manifest entry, never an error.
func Applicable(t Target, clusterID string) bool {
	isLocal := clusterID == engine.LocalCluster
	if t.LocalOnly && !isLocal {
		return false
	}
	if t.DownstreamOnly && isLocal {
		return false
	}
	return true
}

// ResolveOnCluster runs every target-specific degradation chain (the server
// selector fallback, the kube-api-auth ACE precheck, the fleet-agent
// namespace chain) against an already-decided clusterID. Resolve (the logs
// command, which reads --cluster from one Input) and support-bundle (which
// iterates its own target × cluster matrix, checking Applicable first) both
// funnel through this.
// get reads the management Cluster object through steve, as the caller.
func ResolveOnCluster(ctx context.Context, clients engine.ClusterClientFactory, get steve.Getter, t Target, clusterID string) (Resolved, error) {
	client, err := clients.Cluster(ctx, clusterID)
	if err != nil {
		return Resolved{}, err
	}

	if t.Name == "kube-api-auth" {
		ok, err := hasACE(ctx, get, clusterID)
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
		ns, pods, err := resolveFleetAgentPods(ctx, local, client, get, clusterID)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Client: client, ClusterID: clusterID, Namespace: ns, Pods: pods}, nil
	default:
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
				return "", clierr.New(clierr.CodeValidation,
					fmt.Sprintf("%s is a local-only target; omit --cluster, or pass --cluster local", t.Name))
			}
		}
		return engine.LocalCluster, nil
	case t.DownstreamOnly:
		if clusterFlag == "" {
			return "", engine.ClusterRequiredError(engine.DownstreamClusterRequiredMessage(t.Name))
		}
		resolved, err := engine.ResolveDownstreamCluster(ctx, clients, in, false)
		if err != nil {
			return "", err
		}
		if resolved == engine.LocalCluster {
			return "", clierr.New(clierr.CodeValidation, fmt.Sprintf("%s only runs on a downstream cluster", t.Name))
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
func hasACE(ctx context.Context, get steve.Getter, clusterID string) (bool, error) {
	obj, err := steve.GetCluster(ctx, get, clusterID)
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
	list, err := client.List(ctx, podKind.GVR, ns, "app=rancher")
	if err != nil {
		return "", nil, err
	}
	if len(list.Items) > 0 {
		return ns, list.Items, nil
	}
	if dep, err := client.Get(ctx, deployKind.GVR, ns, "rancher"); err == nil {
		if sel, serr := engine.SelectorFrom(dep); serr == nil {
			if list2, lerr := client.List(ctx, podKind.GVR, ns, sel); lerr == nil && len(list2.Items) > 0 {
				return ns, list2.Items, nil
			}
		}
	}
	return "", nil, clierr.New(clierr.CodeNotFound, dockerInstallHint)
}

// resolveFleetAgentPods runs the fleet-agent namespace resolution chain:
// the management Cluster's spec.fleetWorkspaceName, the fleetcluster
// named after the provisioning identity in that workspace (Rancher keys
// fleet Clusters by the provisioning name, not the mgmt id), its
// spec.agentNamespace, falling through to the local/downstream defaults and
// finally the legacy fleet-system namespace when no pods match. Every read
// in the chain degrades silently to the next default on error/absence -
// "no-permission" is a fall-through, not a failure.
func resolveFleetAgentPods(ctx context.Context, local, client engine.ClusterClient, get steve.Getter, clusterID string) (string, []unstructured.Unstructured, error) {
	isLocal := clusterID == engine.LocalCluster

	workspace := fleetWorkspaceDownstream
	if isLocal {
		workspace = fleetWorkspaceLocal
	}
	if obj, err := steve.GetCluster(ctx, get, clusterID); err == nil {
		if ws, found, _ := unstructured.NestedString(obj.Object, "spec", "fleetWorkspaceName"); found && ws != "" {
			workspace = ws
		}
	}

	fleetClusterName := "local"
	if !isLocal {
		fleetClusterName = ""
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
		if fc, err := local.Get(ctx, fleetKind.GVR, workspace, fleetClusterName); err == nil {
			if agentNS, found, _ := unstructured.NestedString(fc.Object, "spec", "agentNamespace"); found && agentNS != "" {
				ns = agentNS
			}
		}
	}

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
