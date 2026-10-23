// quorum.go implements a pure etcd-quorum-impact computation over a
// provisioningcluster patch's original and patched objects. It is deliberately
// side-effect-free and imports nothing beyond unstructured - the caller
// (resources.NewPatch's EffectEnricher hook, wired in internal/cli/root.go)
// decides what to do with the result.
package provisioning

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// QuorumDelta is the etcd-member-count change a machinePools patch implies.
type QuorumDelta struct {
	Before, After int
	// Crossing reports whether After drops enough to risk losing the OLD
	// quorum (see ComputeQuorumDelta's comment for the exact rule).
	Crossing bool
	// Note is the readable line meant for Effect.Warnings.
	Note string
}

// ComputeQuorumDelta compares the etcd member count (the sum of
// spec.rkeConfig.machinePools[].quantity over pools with etcdRole: true)
// between orig and patched. The second return is false when the count did
// not change - e.g. a patch that only touches a non-etcd pool, or the
// cluster's kubernetesVersion - meaning the caller has nothing to warn
// about. Note always states the before/after counts; it additionally notes
// that an even number of etcd members does not improve fault tolerance over
// the next odd number down, and separately that the drop risks losing the
// old quorum, when each condition applies - the two are independent and may
// both fire (e.g. 3 -> 2).
func ComputeQuorumDelta(orig, patched *unstructured.Unstructured) (QuorumDelta, bool) {
	before := etcdMemberCount(orig)
	after := etcdMemberCount(patched)
	if before == after {
		return QuorumDelta{}, false
	}

	d := QuorumDelta{Before: before, After: after}
	note := fmt.Sprintf("etcd member count changes from %d to %d", before, after)
	if after%2 == 0 {
		note += "; an even number of etcd members does not improve fault tolerance"
	}
	// Simplified crossing rule: a drop below the old
	// majority threshold (floor(before/2)+1, using real division) risks
	// losing quorum. Written as 2*after < before+2 to stay in integer
	// arithmetic instead of computing a float.
	if 2*after < before+2 {
		d.Crossing = true
		note += "; removing this many etcd members risks losing quorum"
	}
	d.Note = note
	return d, true
}

// etcdMemberCount sums spec.rkeConfig.machinePools[].quantity over pools
// with etcdRole: true. A pool with no quantity field defaults to 1 - the
// rancher default for an unset machine pool size (createcluster.go
// always sets it explicitly, but a hand-authored manifest or an RFC
// 6902 patch removing the field must still be counted correctly).
func etcdMemberCount(u *unstructured.Unstructured) int {
	if u == nil {
		return 0
	}
	pools, found, err := unstructured.NestedSlice(u.Object, "spec", "rkeConfig", "machinePools")
	if err != nil || !found {
		return 0
	}
	total := 0
	for _, p := range pools {
		pool, ok := p.(map[string]any)
		if !ok {
			continue
		}
		etcd, _, _ := unstructured.NestedBool(pool, "etcdRole")
		if !etcd {
			continue
		}
		total += poolQuantity(pool)
	}
	return total
}

// poolQuantity reads a single machine pool's quantity, defaulting to 1
// when absent (see etcdMemberCount's comment).
func poolQuantity(pool map[string]any) int {
	v, found, err := unstructured.NestedInt64(pool, "quantity")
	if err != nil || !found {
		return 1
	}
	return int(v)
}
