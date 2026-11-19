package clusterops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func snapshotRecord(name, cluster, createdAt string) map[string]any {
	rec := map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{"rke.cattle.io/cluster-name": "prod"}},
		"spec":     map[string]any{"clusterName": cluster},
	}
	if createdAt != "" {
		rec["snapshotFile"] = map[string]any{"createdAt": createdAt}
	}
	return rec
}

// TestSnapshotOfCluster asserts an etcd snapshot object belongs to the cluster named by its
// spec.clusterName, whatever its labels say.
func TestSnapshotOfCluster(t *testing.T) {
	t.Parallel()
	assert.True(t, SnapshotOfCluster(snapshotRecord("a", "prod", ""), "prod"))
	assert.False(t, SnapshotOfCluster(snapshotRecord("a", "other", ""), "prod"), "the label names prod, the field another cluster")
	assert.False(t, SnapshotOfCluster(map[string]any{"metadata": map[string]any{"name": "a"}}, "prod"))
	assert.False(t, SnapshotOfCluster(map[string]any{"spec": map[string]any{}}, ""), "an empty name matches nothing")
}

func names(snapshots []map[string]any) []string {
	var out []string
	for _, s := range snapshots {
		out = append(out, nestedString(s, "metadata", "name"))
	}
	return out
}

// TestSortSnapshotsNewestFirst asserts the order: newest time first, equal
// times (in any zone) by name, a time that does not parse or is absent last.
func TestSortSnapshotsNewestFirst(t *testing.T) {
	t.Parallel()
	snaps := []map[string]any{
		snapshotRecord("old", "prod", "2026-11-01T10:00:00Z"),
		snapshotRecord("none", "prod", ""),
		snapshotRecord("new", "prod", "2026-11-03T10:00:00Z"),
		snapshotRecord("tie-b", "prod", "2026-11-02T10:00:00Z"),
		snapshotRecord("bad", "prod", "yesterday"),
		snapshotRecord("tie-a", "prod", "2026-11-02T12:00:00+02:00"),
		snapshotRecord("zone", "prod", "2026-11-02T08:30:00-02:00"),
	}
	SortSnapshotsNewestFirst(snaps)
	assert.Equal(t, []string{"new", "zone", "tie-a", "tie-b", "old", "bad", "none"}, names(snaps))
}
