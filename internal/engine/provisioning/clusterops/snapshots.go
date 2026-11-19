package clusterops

import (
	"slices"
	"strings"
	"time"
)

// SnapshotOfCluster reports whether an etcd snapshot object belongs to the
// cluster: its spec.clusterName equals cluster. The objects of one namespace
// can belong to several clusters, and the label rke.cattle.io/cluster-name
// says nothing the field does not.
func SnapshotOfCluster(snapshot map[string]any, cluster string) bool {
	name := nestedString(snapshot, "spec", "clusterName")
	return name != "" && name == cluster
}

// SortSnapshotsNewestFirst orders etcd snapshot objects in place by
// snapshotFile.createdAt, read as RFC 3339, newest first; objects with the
// same time are ordered by metadata.name, and an object without a parseable
// time sorts last.
func SortSnapshotsNewestFirst(snapshots []map[string]any) {
	slices.SortStableFunc(snapshots, func(a, b map[string]any) int {
		ta, okA := snapshotTime(a)
		tb, okB := snapshotTime(b)
		switch {
		case okA && !okB:
			return -1
		case !okA && okB:
			return 1
		case okA && okB:
			if c := tb.Compare(ta); c != 0 {
				return c
			}
		}
		return strings.Compare(nestedString(a, "metadata", "name"), nestedString(b, "metadata", "name"))
	})
}

// snapshotTime returns snapshotFile.createdAt and whether it parses.
func snapshotTime(snapshot map[string]any) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, nestedString(snapshot, "snapshotFile", "createdAt"))
	return t, err == nil
}
