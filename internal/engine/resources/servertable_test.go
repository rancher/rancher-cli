package resources

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// tableFixture decodes the Table body testdata/tables/<name>.json the way
// the provider does: the meta.k8s.io decoder, then the rows' objects.
func tableFixture(tb testing.TB, name string) *metav1.Table {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "tables", name+".json"))
	require.NoError(tb, err)
	obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(raw, nil, nil)
	require.NoError(tb, err)
	table, ok := obj.(*metav1.Table)
	require.True(tb, ok, "got %T", obj)
	require.NoError(tb, engine.DecodeRowObjects(table))
	return table
}

// tablePayload is the Data a get returns for a table answer, without project
// scope.
func tablePayload(t *testing.T, table *metav1.Table, showNamespace bool) serverTable {
	t.Helper()
	st, err := newServerTable(table, showNamespace)
	require.NoError(t, err)
	return st
}

// renderPayload renders data as format through the table writer.
func renderPayload(tb testing.TB, data any, format string) string {
	tb.Helper()
	var buf bytes.Buffer
	require.NoError(tb, output.Render(&buf, format, output.NewEnvelope("X", data)))
	return buf.String()
}

// TestServerTableShowsTheServersColumnsAndCells asserts, for tables the API
// server answered, kubectl's headers (the server's names in its order,
// upper-cased), its default columns without -o wide and the priority
// columns with it, and each cell as the server sent it, whole numbers
// included.
func TestServerTableShowsTheServersColumnsAndCells(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		cols      []string
		wideCols  []string
		row       []string
		wideRow   []string
		rowsCount int
	}{
		{"nodes",
			[]string{"NAME", "STATUS", "ROLES", "AGE", "VERSION"},
			[]string{"NAME", "STATUS", "ROLES", "AGE", "VERSION", "INTERNAL-IP", "EXTERNAL-IP", "OS-IMAGE", "KERNEL-VERSION", "CONTAINER-RUNTIME"},
			[]string{"k3s-node-1", "Ready", "control-plane", "120m", "v1.34.4+k3s1"},
			[]string{"k3s-node-1", "Ready", "control-plane", "120m", "v1.34.4+k3s1", "192.0.2.10", "<none>", "K3s v1.34.4+k3s1", "6.18.37-0-virt", "containerd://2.1.5-k3s1"},
			1},
		{"pods",
			[]string{"NAME", "READY", "STATUS", "RESTARTS", "AGE"},
			[]string{"NAME", "READY", "STATUS", "RESTARTS", "AGE", "IP", "NODE", "NOMINATED NODE", "READINESS GATES"},
			[]string{"fleet-agent-55dd77c5f5-kw44x", "1/1", "Running", "0", "119m"},
			[]string{"fleet-agent-55dd77c5f5-kw44x", "1/1", "Running", "0", "119m", "192.0.2.10", "k3s-node-1", "<none>", "<none>"},
			6},
		{"deployments",
			[]string{"NAME", "READY", "UP-TO-DATE", "AVAILABLE", "AGE"},
			[]string{"NAME", "READY", "UP-TO-DATE", "AVAILABLE", "AGE", "CONTAINERS", "IMAGES", "SELECTOR"},
			[]string{"fleet-agent", "1/1", "1", "1", "119m"},
			[]string{"fleet-agent", "1/1", "1", "1", "119m", "fleet-agent", "rancher/fleet-agent:v0.16.2", "app=fleet-agent"},
			6},
		{"clusterroles",
			[]string{"NAME", "CREATED AT"},
			[]string{"NAME", "CREATED AT"},
			[]string{"admin", "2026-10-20T18:32:41Z"},
			[]string{"admin", "2026-10-20T18:32:41Z"},
			1},
		{"namespaces",
			[]string{"NAME", "STATUS", "AGE"},
			[]string{"NAME", "STATUS", "AGE"},
			[]string{"c-m-fixture", "Active", "120m"},
			[]string{"c-m-fixture", "Active", "120m"},
			3},
		{"node-named",
			[]string{"NAME", "STATUS", "ROLES", "AGE", "VERSION"},
			[]string{"NAME", "STATUS", "ROLES", "AGE", "VERSION", "INTERNAL-IP", "EXTERNAL-IP", "OS-IMAGE", "KERNEL-VERSION", "CONTAINER-RUNTIME"},
			[]string{"k3s-node-1", "Ready", "control-plane", "5h47m", "v1.34.4+k3s1"},
			[]string{"k3s-node-1", "Ready", "control-plane", "5h47m", "v1.34.4+k3s1", "192.0.2.10", "<none>", "K3s v1.34.4+k3s1", "6.18.37-0-virt", "containerd://2.1.5-k3s1"},
			1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tablePayload(t, tableFixture(t, tc.name), false)
			assert.Equal(t, tc.cols, st.Columns())
			assert.Equal(t, tc.wideCols, st.WideColumns())
			require.Len(t, st.Rows(), tc.rowsCount)
			require.Len(t, st.WideRows(), tc.rowsCount)
			assert.Equal(t, tc.row, st.Rows()[0])
			assert.Equal(t, tc.wideRow, st.WideRows()[0])
		})
	}
}

// TestServerTableWideColumnsOnlyUnderWide asserts a column with priority
// above 0 shows under -o wide alone, in the server's position.
func TestServerTableWideColumnsOnlyUnderWide(t *testing.T) {
	t.Parallel()
	st := tablePayload(t, tableFixture(t, "events-empty"), false)
	assert.Equal(t, []string{"LAST SEEN", "TYPE", "REASON", "OBJECT", "MESSAGE"}, st.Columns())
	assert.Equal(t, []string{"LAST SEEN", "TYPE", "REASON", "OBJECT", "SUBOBJECT", "SOURCE", "MESSAGE", "FIRST SEEN", "COUNT", "NAME"}, st.WideColumns(),
		"a wide column keeps the server's position, between the default ones")
}

// TestServerTableRowStates asserts the cells of rows the live pair does not
// have - a node that is not ready or cordoned, a pod that crashes - show as
// the server sent them: the rows are copies of recorded rows with the
// status cells edited.
func TestServerTableRowStates(t *testing.T) {
	t.Parallel()
	nodes := tableFixture(t, "nodes")
	ready := nodes.Rows[0]
	notReady, cordoned := *ready.DeepCopy(), *ready.DeepCopy()
	notReady.Cells[1] = "NotReady"
	cordoned.Cells[1] = "Ready,SchedulingDisabled"
	nodes.Rows = []metav1.TableRow{ready, notReady, cordoned}
	st := tablePayload(t, nodes, false)
	var statuses []string
	for _, row := range st.Rows() {
		statuses = append(statuses, row[1])
	}
	assert.Equal(t, []string{"Ready", "NotReady", "Ready,SchedulingDisabled"}, statuses)

	pods := tableFixture(t, "pods")
	crash := *pods.Rows[0].DeepCopy()
	crash.Cells[2] = "CrashLoopBackOff"
	crash.Cells[3] = "7 (3m ago)"
	pods.Rows = []metav1.TableRow{crash}
	got := tablePayload(t, pods, false).Rows()
	assert.Equal(t, []string{"fleet-agent-55dd77c5f5-kw44x", "1/1", "CrashLoopBackOff", "7 (3m ago)", "119m"}, got[0])
}

func cellsTable(cells ...[]any) *metav1.Table {
	t := &metav1.Table{ColumnDefinitions: []metav1.TableColumnDefinition{
		{Name: "Name"}, {Name: "Value"}, {Name: "Extra", Priority: 1},
	}}
	for _, c := range cells {
		t.Rows = append(t.Rows, metav1.TableRow{Cells: c})
	}
	return t
}

// TestServerTableCellRules asserts the cell rules: a nil cell is empty, a
// string is kept, any other value prints as fmt.Sprint does, a value cut at
// its first newline, carriage return or form feed ends in "...", cells past
// the columns are ignored, and a missing cell is empty.
func TestServerTableCellRules(t *testing.T) {
	t.Parallel()
	st := tablePayload(t, cellsTable(
		[]any{"a", nil, "x"},
		[]any{"b", "one\ntwo", "x"},
		[]any{"c", "one\rtwo", "x"},
		[]any{"d", "one\ftwo", "x"},
		[]any{"e", int64(42), "x"},
		[]any{"f", 0.5, "x"},
		[]any{"g", true, "x"},
		[]any{"h", "kept", "x", "extra cell", "and another"},
		[]any{"i"},
		[]any{"j", "\nstarts with a newline", "x"},
		[]any{},
	), false)
	assert.Equal(t, [][]string{
		{"a", ""},
		{"b", "one..."},
		{"c", "one..."},
		{"d", "one..."},
		{"e", "42"},
		{"f", "0.5"},
		{"g", "true"},
		{"h", "kept"},
		{"i", ""},
		{"j", "..."},
		{"", ""},
	}, st.Rows())
	wide := st.WideRows()
	assert.Equal(t, []string{"h", "kept", "x"}, wide[7], "cells past the columns are ignored")
	assert.Equal(t, []string{"i", "", ""}, wide[8], "a missing cell is empty")
}

// TestServerTableMarshalIsRefused asserts a structured format cannot render
// the server's table: the command asks for a table only for a text format.
func TestServerTableMarshalIsRefused(t *testing.T) {
	t.Parallel()
	st := tablePayload(t, tableFixture(t, "nodes"), false)
	_, err := json.Marshal(st)
	require.ErrorContains(t, err, "command bug")
}

// TestServerTableNamespaceColumn asserts the NAMESPACE column comes first,
// from each row's object metadata, in both the default and the wide
// columns; and that a row without an object cannot supply one.
func TestServerTableNamespaceColumn(t *testing.T) {
	t.Parallel()
	pods := tableFixture(t, "pods")
	st := tablePayload(t, pods, true)
	assert.Equal(t, []string{"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "AGE"}, st.Columns())
	assert.Equal(t, []string{"NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "AGE", "IP", "NODE", "NOMINATED NODE", "READINESS GATES"}, st.WideColumns())
	assert.Equal(t, []string{"cattle-fleet-system", "fleet-agent-55dd77c5f5-kw44x", "1/1", "Running", "0", "119m"}, st.Rows()[0])
	assert.Equal(t, "cattle-system", st.Rows()[1][0])
	assert.Equal(t, "kube-system", st.WideRows()[5][0])

	pods.Rows[0].Object.Object = nil
	_, err := newServerTable(pods, true)
	require.ErrorContains(t, err, "a row without its object")
	_, err = newServerTable(pods, false)
	require.NoError(t, err, "without the NAMESPACE column no object is read")
}

// TestServerTableNeverReadsKindColumns asserts the table's headers and cells
// come from the server's answer alone: a kind that carries Columns changes
// nothing.
func TestServerTableNeverReadsKindColumns(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{Name: "node", ServerTable: true, Columns: []kinds.Column{{Header: "MINE", Path: []string{"spec", "mine"}}}}
	data, warnings, err := tableData(k, engine.TableAnswer{Table: tableFixture(t, "nodes")}, false, nil, false)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	st, ok := data.(serverTable)
	require.True(t, ok, "got %T", data)
	assert.Equal(t, []string{"NAME", "STATUS", "ROLES", "AGE", "VERSION"}, st.Columns())
}

// TestServerTableFallbackShowsNameAndAge asserts the answer that is no
// usable table - the ordinary objects, a Table with no column definitions -
// renders NAME and AGE with one warning that says so.
func TestServerTableFallbackShowsNameAndAge(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "node")
	created := "2026-10-20T18:32:41Z"
	item := func(name string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "creationTimestamp": created}}}
	}
	t.Run("ordinary objects", func(t *testing.T) {
		t.Parallel()
		data, warnings, err := tableData(k, engine.TableAnswer{Items: []unstructured.Unstructured{item("n-1"), item("n-2")}}, false, nil, false)
		require.NoError(t, err)
		assert.Equal(t, []string{noTableWarning}, warnings)
		tab := data.(output.Tabular)
		assert.Equal(t, []string{"NAME", "AGE"}, tab.Columns())
		require.Len(t, tab.Rows(), 2)
		assert.Equal(t, "n-1", tab.Rows()[0][0])
		assert.NotEmpty(t, tab.Rows()[0][1], "AGE comes from creationTimestamp")
	})
	t.Run("a table without column definitions", func(t *testing.T) {
		t.Parallel()
		table := tableFixture(t, "nodes")
		table.ColumnDefinitions = nil
		data, warnings, err := tableData(k, engine.TableAnswer{Table: table}, false, nil, false)
		require.NoError(t, err)
		assert.Equal(t, []string{noTableWarning}, warnings)
		tab := data.(output.Tabular)
		assert.Equal(t, []string{"NAME", "AGE"}, tab.Columns())
		assert.Equal(t, "k3s-node-1", tab.Rows()[0][0])
		assert.NotEmpty(t, tab.Rows()[0][1])
	})
	t.Run("an empty plain list", func(t *testing.T) {
		t.Parallel()
		data, warnings, err := tableData(k, engine.TableAnswer{}, false, nil, false)
		require.NoError(t, err)
		assert.Equal(t, []string{noTableWarning}, warnings)
		assert.Equal(t, "NAME   AGE\n", renderPayload(t, data, "table"))
	})
	t.Run("a project scope keeps the member namespaces of a plain list", func(t *testing.T) {
		t.Parallel()
		inNS := func(ns, name string) unstructured.Unstructured {
			return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"namespace": ns, "name": name}}}
		}
		items := []unstructured.Unstructured{inNS("ns-a", "one"), inNS("ns-z", "two"), inNS("ns-a", "three")}
		data, _, err := tableData(mustKind(t, "pod"), engine.TableAnswer{Items: items}, false, &ProjectScope{Members: map[string]bool{"ns-a": true}}, false)
		require.NoError(t, err)
		rows := data.(output.Tabular).Rows()
		require.Len(t, rows, 2)
		assert.Equal(t, "one", rows[0][0])
		assert.Equal(t, "three", rows[1][0])
	})
	t.Run("a project scope over a table without columns", func(t *testing.T) {
		t.Parallel()
		scope := &ProjectScope{Members: map[string]bool{"ns-a": true}}
		table := projectTable(t, "ns-a", "ns-z")
		table.ColumnDefinitions = nil
		data, _, err := tableData(mustKind(t, "pod"), engine.TableAnswer{Table: table}, false, scope, false)
		require.NoError(t, err)
		require.Len(t, data.(output.Tabular).Rows(), 1)

		table = projectTable(t, "ns-a", "ns-z")
		table.ColumnDefinitions = nil
		table.Rows[1].Object.Object = nil
		_, _, err = tableData(mustKind(t, "pod"), engine.TableAnswer{Table: table}, false, scope, false)
		require.ErrorContains(t, err, "a row without its object")
	})
	t.Run("the fallback is still the objects for a structured format", func(t *testing.T) {
		t.Parallel()
		data, _, err := tableData(k, engine.TableAnswer{Items: []unstructured.Unstructured{item("n-1")}}, true, nil, false)
		require.NoError(t, err)
		b, err := json.Marshal(data)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, "n-1", got["metadata"].(map[string]any)["name"])
	})
}

// TestServerTableEmptyListShowsTheHeader asserts an empty list prints its
// header line and nothing else, as every other table of the CLI does.
func TestServerTableEmptyListShowsTheHeader(t *testing.T) {
	t.Parallel()
	data, warnings, err := tableData(mustKind(t, "event"), engine.TableAnswer{Table: tableFixture(t, "events-empty")}, false, nil, true)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, "NAMESPACE   LAST SEEN   TYPE   REASON   OBJECT   MESSAGE\n", renderPayload(t, data, "table"))
	assert.Equal(t, 1, strings.Count(renderPayload(t, data, "wide"), "\n"))
}

// TestServerTableRendersThroughTheTableWriter asserts a rendered table is
// aligned by the CLI's table writer, one line per row after the header.
func TestServerTableRendersThroughTheTableWriter(t *testing.T) {
	t.Parallel()
	st := tablePayload(t, tableFixture(t, "namespaces"), false)
	assert.Equal(t, "NAME                  STATUS   AGE\n"+
		"c-m-fixture           Active   120m\n"+
		"c-m-fixture-p-47jmq   Active   120m\n"+
		"c-m-fixture-p-d5nh2   Active   120m\n", renderPayload(t, st, "table"))
}
