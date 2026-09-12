package complete

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// PodSource is the source logs hands the surface: --pod completes the
// pods of the target typed as the first positional, in the target's
// namespace under its selector. A local-only target lists on the
// management cluster with no cluster need and any credential; a
// downstream-only target needs the typed cluster resolved. A target
// whose namespace is resolved at run time (fleet-agent), an unknown
// target, and every other site are declined.
func PodSource() completion.Source {
	return completion.Source{Claim: claimPods, Complete: completePods}
}

func claimPods(req completion.Request) (completion.Requirements, bool) {
	target, ok := typedTarget(req)
	if !ok {
		return completion.Requirements{}, false
	}
	cluster := completion.ClusterNone
	if target.DownstreamOnly {
		cluster = completion.ClusterResolved
	}
	return completion.Requirements{Cluster: cluster, Credential: completion.CredentialAny, Allowance: completion.AllowanceOne}, true
}

// typedTarget resolves a --pod site's first positional to a target with
// a static namespace.
func typedTarget(req completion.Request) (Target, bool) {
	if req.Flag != "pod" || len(req.Positionals) == 0 {
		return Target{}, false
	}
	target, ok := Lookup(req.Positionals[0])
	if !ok || target.Namespace == "" {
		return Target{}, false
	}
	return target, true
}

func completePods(ctx context.Context, conn *completion.Connection, req completion.Request) completion.Result {
	target, ok := typedTarget(req)
	if !ok {
		return completion.Result{}
	}
	k, ok := kinds.ByKey("pod")
	if !ok {
		return completion.Result{}
	}
	g, v, r := k.GVR.Group, k.GVR.Version, k.GVR.Resource
	var p completion.Path
	var err error
	if target.DownstreamOnly {
		p, err = completion.NamespacedPath(req.ClusterID, g, v, target.Namespace, r)
	} else {
		p, err = completion.ManagementNamespacedPath(g, v, target.Namespace, r)
	}
	if err != nil {
		return completion.Result{}
	}
	if target.Selector != "" {
		p = p.WithQuery("labelSelector", target.Selector)
	}
	var list completion.ItemList
	if err := conn.GetJSON(ctx, p, "", &list); err != nil {
		return completion.Result{}
	}
	return completion.Result{Names: list.Names()}
}
