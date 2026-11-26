package kinds

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// serverNamespaceGolden is the recorded Table the API server sends for a list
// of namespaces: its columns only, as the fidelity probe records them.
const serverNamespaceGolden = "../../testrancher/fidelity/testdata/oracle/server-table/local-namespaces.json"

// namespaceObject is a namespace as the Kubernetes API returns it; created is
// its metadata.creationTimestamp, left out when empty.
func namespaceObject(name, phase string, created string) unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if created != "" {
		meta["creationTimestamp"] = created
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": meta,
		"status": map[string]any{"phase": phase},
	}}
}

// TestEveryServerTableKindThatReadsSteveHasATableBuilder asserts that every
// kind that sets ServerTable and SteveReadOnForbidden has a table builder, that
// every builder belongs to such a kind, and that the kinds with a Steve read
// downstream are the namespace kind alone.
func TestEveryServerTableKindThatReadsSteveHasATableBuilder(t *testing.T) {
	t.Parallel()
	for _, k := range All() {
		_, has := SteveTable(k.Key)
		assert.Equal(t, k.ServerTable && k.SteveReadOnForbidden, has, "kind %s: a table builder exists only for a ServerTable kind that sets SteveReadOnForbidden, and each such kind has one", k.Key)
		if k.Scope == Downstream && k.SteveReadOnForbidden {
			assert.Contains(t, downstreamSteveReadKeys, k.Key)
		}
	}
	for key := range steveTableBuilders {
		k, ok := ByKey(key)
		require.True(t, ok, "the table builder of %q belongs to no kind", key)
		assert.True(t, k.ServerTable && k.SteveReadOnForbidden, key)
	}
}

// TestNamespaceTableHasTheColumnsOfTheServersTable asserts the column
// definitions of the namespace builder (name, type, format and priority) equal
// the columnDefinitions the server recorded for a list of namespaces.
func TestNamespaceTableHasTheColumnsOfTheServersTable(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(serverNamespaceGolden)
	require.NoError(t, err)
	var golden struct {
		Observations []struct {
			Name string `json:"name"`
			Body struct {
				ColumnDefinitions []metav1.TableColumnDefinition `json:"columnDefinitions"`
			} `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	require.Len(t, golden.Observations, 1)
	require.Equal(t, "namespaces", golden.Observations[0].Name)
	want := golden.Observations[0].Body.ColumnDefinitions
	require.Len(t, want, 3)

	build, ok := SteveTable("namespace")
	require.True(t, ok)
	got := build(nil, time.Now()).ColumnDefinitions
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, want[i].Name, got[i].Name, "column %d name", i)
		assert.Equal(t, want[i].Type, got[i].Type, "column %d type", i)
		assert.Equal(t, want[i].Format, got[i].Format, "column %d format", i)
		assert.Equal(t, want[i].Priority, got[i].Priority, "column %d priority", i)
	}
}

// TestNamespaceTableCellsFollowThePrinterOfTheServer asserts the cells of two
// namespaces equal a Table written by hand from the server's printer rule: the
// name, status.phase as it is and the age of metadata.creationTimestamp in the
// form of duration.HumanDuration; a namespace with no creation time has a blank
// age, as `get` shows it for the other tables. The expected cells are written from
// that rule, not recorded from a server.
func TestNamespaceTableCellsFollowThePrinterOfTheServer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	objs := []unstructured.Unstructured{
		namespaceObject("team-a", "Active", "2026-11-24T09:00:00Z"),
		namespaceObject("old", "Terminating", "2026-09-01T12:00:00Z"),
		namespaceObject("few-fields", "Active", ""),
	}
	build, ok := SteveTable("namespace")
	require.True(t, ok)
	table := build(objs, now)
	assert.Equal(t, "Table", table.Kind)
	assert.Equal(t, "meta.k8s.io/v1", table.APIVersion)
	want := [][]any{
		{"team-a", "Active", "3h"},
		{"old", "Terminating", "84d"},
		{"few-fields", "Active", ""},
	}
	require.Len(t, table.Rows, len(want))
	for i, row := range table.Rows {
		assert.Equal(t, want[i], row.Cells, "row %d", i)
		meta, ok := row.Object.Object.(metav1.Object)
		require.True(t, ok, "row %d carries its object, which the project filter reads", i)
		assert.Equal(t, objs[i].GetName(), meta.GetName())
	}
}

// TestNamespaceTableDoesNotChangeItsInput asserts the builder copies each
// object: a change to a row's object leaves the caller's list as it was.
func TestNamespaceTableDoesNotChangeItsInput(t *testing.T) {
	t.Parallel()
	objs := []unstructured.Unstructured{namespaceObject("team-a", "Active", "2026-11-24T09:00:00Z")}
	build, ok := SteveTable("namespace")
	require.True(t, ok)
	table := build(objs, time.Now())
	row, ok := table.Rows[0].Object.Object.(*unstructured.Unstructured)
	require.True(t, ok)
	row.SetName("changed")
	assert.Equal(t, "team-a", objs[0].GetName())
}
