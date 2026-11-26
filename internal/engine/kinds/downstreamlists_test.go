package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	listableAcrossTheCluster = "every project role and cluster member may list it across the cluster"
	plainNamespaceReason     = "lists every namespace through the Kubernetes API under --project; a caller with roles in the project's namespaces only is refused"
)

// downstreamKindsWithNoNamespace maps each downstream row with no namespace
// that does not read the Steve API after a 403 (SteveReadOnForbidden) to why a
// caller who may not list the kind across the cluster needs no Steve read of
// it.
var downstreamKindsWithNoNamespace = map[string]string{
	"persistentvolume":   listableAcrossTheCluster,
	"storageclass":       listableAcrossTheCluster,
	"clusterrepo":        listableAcrossTheCluster,
	"node":               "project owners and cluster members may list it across the cluster; Rancher gives no other role permission to get a node by name",
	"clusterrole":        "no project role or cluster member has a rule on it, and the Steve API would list none",
	"clusterrolebinding": "no project role or cluster member has a rule on it, and the Steve API would list none",
}

// downstreamKindsWithAPlainNamespace maps each downstream row with a plain
// namespace to how `get <kind> --project <p>` without --namespace reads it
// for a caller whom Rancher grants the project's namespaces only: each row
// lists every namespace through the Kubernetes API, and the caller is refused.
var downstreamKindsWithAPlainNamespace = map[string]string{
	"pod": plainNamespaceReason, "deployment": plainNamespaceReason, "service": plainNamespaceReason,
	"configmap": plainNamespaceReason, "secret": plainNamespaceReason, "statefulset": plainNamespaceReason,
	"daemonset": plainNamespaceReason, "replicaset": plainNamespaceReason, "job": plainNamespaceReason,
	"cronjob": plainNamespaceReason, "ingress": plainNamespaceReason, "persistentvolumeclaim": plainNamespaceReason,
	"serviceaccount": plainNamespaceReason, "event": plainNamespaceReason, "horizontalpodautoscaler": plainNamespaceReason,
	"limitrange": plainNamespaceReason, "networkpolicy": plainNamespaceReason, "poddisruptionbudget": plainNamespaceReason,
	"resourcequota": plainNamespaceReason, "role": plainNamespaceReason, "rolebinding": plainNamespaceReason,
	"app": plainNamespaceReason, "operation": plainNamespaceReason, "k3kcluster": plainNamespaceReason,
}

// TestEveryDownstreamKindSaysHowACallerWithoutAClusterListReadsIt asserts that
// every downstream row is classified: a row with no namespace either sets
// SteveReadOnForbidden (the namespace row) or is a key of
// downstreamKindsWithNoNamespace with a reason; a row with a plain namespace is
// a key of downstreamKindsWithAPlainNamespace with a reason; each key names a
// downstream row of its kind of namespace. A new downstream row fails until it
// is classified.
func TestEveryDownstreamKindSaysHowACallerWithoutAClusterListReadsIt(t *testing.T) {
	t.Parallel()
	noNamespace, plain := map[string]bool{}, map[string]bool{}
	for _, k := range All() {
		if k.Scope != Downstream {
			continue
		}
		switch k.Policy {
		case NSNone:
			noNamespace[k.Key] = true
			reason, listed := downstreamKindsWithNoNamespace[k.Key]
			if k.SteveReadOnForbidden {
				assert.False(t, listed, "row %s reads the Steve API after a 403 and is also listed as needing none", k.Key)
				continue
			}
			assert.True(t, listed, "downstream row %s has no namespace: set SteveReadOnForbidden, or list it in downstreamKindsWithNoNamespace with a reason", k.Key)
			assert.NotEmpty(t, reason, k.Key)
		case NSPlain:
			plain[k.Key] = true
			reason, listed := downstreamKindsWithAPlainNamespace[k.Key]
			assert.True(t, listed, "downstream row %s has a plain namespace: list it in downstreamKindsWithAPlainNamespace with a reason", k.Key)
			assert.NotEmpty(t, reason, k.Key)
		default:
			assert.Fail(t, "a downstream row has a namespace policy no table classifies", "row %s has policy %q", k.Key, k.Policy)
		}
	}
	for key := range downstreamKindsWithNoNamespace {
		assert.True(t, noNamespace[key], "downstreamKindsWithNoNamespace lists %q, which is no downstream row with no namespace", key)
	}
	for key := range downstreamKindsWithAPlainNamespace {
		assert.True(t, plain[key], "downstreamKindsWithAPlainNamespace lists %q, which is no downstream row with a plain namespace", key)
	}
}
