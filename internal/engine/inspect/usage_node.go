package inspect

import (
	"context"
	"sort"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// collectByNode groups pods by spec.nodeName for REQUEST/LIMIT columns and
// fetches metrics.k8s.io node metrics for USAGE columns. Node USAGE is
// intentionally not the sum of pod metrics — it is true machine consumption
// including system overhead (parity with old `top node`). A missing metrics
// server degrades the USAGE columns to empty plus one warning. Pods with no
// spec.nodeName (pending/unscheduled) are excluded. Nodes with zero pods still
// get rows (from the node List) with zero request/limit. A node present in the
// cluster but absent from the metrics response reports empty usage (no
// measurement), unlike the pod/namespace groupings where an absent pod-metrics
// entry contributes zero to a sum.
func (c *usageCmd) collectByNode(ctx context.Context, client engine.ClusterClient, sec *sections, in runtime.Input) (runtime.Result, error) {
	nodeKind, _ := kinds.Lookup("node")
	podKind, _ := kinds.Lookup("pod")
	nodeName := in.String("node")

	rows := usageRows{}

	if nodeName != "" {
		// Single-node path: Get node + cluster-wide pod List filtered client-side.
		_, err := client.Get(ctx, nodeKind.GVR, "", nodeName)
		if err != nil {
			return runtime.Result{}, err
		}

		pods, err := client.List(ctx, podKind.GVR, "", "")
		if err != nil {
			return runtime.Result{}, err
		}
		var nodePods []unstructured.Unstructured
		for _, pod := range pods.Items {
			nn, _, _ := unstructured.NestedString(pod.Object, "spec", "nodeName")
			if nn == nodeName {
				nodePods = append(nodePods, pod)
			}
		}
		cpuReq, cpuLim, memReq, memLim := sumPodResources(nodePods)
		totals := resourceTotals{
			CPURequest: cpuReq.String(), CPULimit: cpuLim.String(),
			MemoryRequest: memReq.String(), MemoryLimit: memLim.String(),
		}
		if m, merr := client.Get(ctx, nodeMetricsGVR, "", nodeName); merr != nil {
			if cerr := sec.Unavailable(sectionMetrics, merr); cerr != nil {
				return runtime.Result{}, cerr
			}
		} else {
			usage, _, _ := unstructured.NestedMap(m.Object, "usage")
			totals.CPUUsage = canonicalUsage(usage, "cpu")
			totals.MemoryUsage = canonicalUsage(usage, "memory")
		}
		rows[nodeName] = totals
		return usageResult(byNode, rows, nil, sec), nil
	}

	// List path: all nodes, pods grouped by spec.nodeName.
	nodes, err := client.List(ctx, nodeKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	for i := range nodes.Items {
		rows[nodes.Items[i].GetName()] = resourceTotals{}
	}

	pods, err := client.List(ctx, podKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	grouped := map[string][]unstructured.Unstructured{}
	for _, pod := range pods.Items {
		nn, _, _ := unstructured.NestedString(pod.Object, "spec", "nodeName")
		if nn == "" {
			continue // pending/unscheduled — excluded per spec UU-19
		}
		grouped[nn] = append(grouped[nn], pod)
	}

	// Pods on nodes absent from the List (deleted mid-run, partial RBAC) keep
	// their row — USAGE stays empty, REQUEST/LIMIT reflect the pod's spec.
	for nn := range grouped {
		if _, ok := rows[nn]; !ok {
			rows[nn] = resourceTotals{}
		}
	}

	nodeNames := make([]string, 0, len(rows))
	for k := range rows {
		nodeNames = append(nodeNames, k)
	}
	sort.Strings(nodeNames)

	metricsOK := true
	usageByName := map[string]map[string]any{}
	if metrics, merr := client.List(ctx, nodeMetricsGVR, "", ""); merr != nil {
		if cerr := sec.Unavailable(sectionMetrics, merr); cerr != nil {
			return runtime.Result{}, cerr
		}
		metricsOK = false
	} else {
		for i := range metrics.Items {
			usage, _, _ := unstructured.NestedMap(metrics.Items[i].Object, "usage")
			usageByName[metrics.Items[i].GetName()] = usage
		}
	}

	for _, nn := range nodeNames {
		cpuReq, cpuLim, memReq, memLim := sumPodResources(grouped[nn])
		totals := resourceTotals{
			CPURequest: cpuReq.String(), CPULimit: cpuLim.String(),
			MemoryRequest: memReq.String(), MemoryLimit: memLim.String(),
		}
		if metricsOK {
			if u, ok := usageByName[nn]; ok {
				totals.CPUUsage = canonicalUsage(u, "cpu")
				totals.MemoryUsage = canonicalUsage(u, "memory")
			}
		}
		rows[nn] = totals
	}
	return usageResult(byNode, rows, nil, sec), nil
}

// canonicalUsage parses a metrics.k8s.io usage quantity and re-serializes it
// so the single-node and list paths render identically: formats are
// canonicalized (80000000n → 80m) and a missing or malformed value degrades
// to "0" instead of passing garbage through to output.
func canonicalUsage(usage map[string]any, key string) string {
	var q resource.Quantity
	if v, _, _ := unstructured.NestedString(usage, key); v != "" {
		if parsed, err := resource.ParseQuantity(v); err == nil {
			q = parsed
		}
	}
	return q.String()
}
