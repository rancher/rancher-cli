package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestETCDSnapshotColumnsRenderTheirPaths asserts the columns of the
// etcdsnapshot row: each column shows the field at its path; STORAGE shows the
// annotation etcdsnapshot.rke.io/storage; MESSAGE shows snapshotFile.message;
// NODE and SIZE-BYTES are blank when the snapshot has no node name or no size;
// a size shows as an integer; and the wide columns appear only with wide
// output.
func TestETCDSnapshotColumnsRenderTheirPaths(t *testing.T) {
	t.Parallel()

	local := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "local-1", "namespace": "fleet-default",
			"annotations": map[string]any{"etcdsnapshot.rke.io/storage": "local"},
		},
		"spec": map[string]any{"clusterName": "c1"},
		"snapshotFile": map[string]any{
			"name": "etcd-snapshot-n1-1700000000", "nodeName": "n1",
			"location":  "file:///var/lib/rancher/rke2/server/db/snapshots/etcd-snapshot-n1-1700000000",
			"createdAt": "2026-01-02T03:04:05Z", "size": int64(1048576), "status": "successful",
		},
	}}
	s3 := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{
			"name": "s3-1", "namespace": "fleet-default",
			"annotations": map[string]any{"etcdsnapshot.rke.io/storage": "s3"},
		},
		"spec": map[string]any{"clusterName": "c2"},
		"snapshotFile": map[string]any{
			"name": "etcd-snapshot-n2-1700000001", "location": "s3://bucket/etcd-snapshot-n2-1700000001",
			"createdAt": "2026-01-03T03:04:05Z", "status": "failed", "message": "the upload failed",
		},
	}}
	row := catalogRow(t, "etcdsnapshot")

	tbl := newTable(row, []unstructured.Unstructured{local, s3}, false)
	assert.Equal(t, []string{"NAME", "STATUS", "CLUSTER", "NODE", "SIZE-BYTES", "CREATED-AT", "AGE"}, tbl.Columns())
	rows := tbl.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"local-1", "successful", "c1", "n1", "1048576", "2026-01-02T03:04:05Z"}, rows[0][:6])
	assert.Equal(t, []string{"s3-1", "failed", "c2", "", "", "2026-01-03T03:04:05Z"}, rows[1][:6],
		"an absent node name and an absent size render blank")

	assert.Equal(t, []string{"NAME", "STATUS", "CLUSTER", "NODE", "SIZE-BYTES", "CREATED-AT", "STORAGE", "LOCATION", "SNAPSHOT-NAME", "MESSAGE", "AGE"}, tbl.WideColumns())
	wideRows := tbl.WideRows()
	require.Len(t, wideRows, 2)
	assert.Equal(t, []string{"local", "file:///var/lib/rancher/rke2/server/db/snapshots/etcd-snapshot-n1-1700000000", "etcd-snapshot-n1-1700000000", ""}, wideRows[0][6:10])
	assert.Equal(t, []string{"s3", "s3://bucket/etcd-snapshot-n2-1700000001", "etcd-snapshot-n2-1700000001", "the upload failed"}, wideRows[1][6:10])
}
