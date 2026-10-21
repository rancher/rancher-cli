package resources

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// tableInput is the Input the pipeline gives a read command: the command's
// own plus whether a text table renders.
type tableInput struct {
	input
	textTable bool
}

func (i tableInput) TextTable() bool { return i.textTable }

func getCommand(t *testing.T, name string, clients engine.ClusterClientFactory) runtime.ReadCommand {
	t.Helper()
	cmd, ok := NewGet(mustKind(t, name), clients).(runtime.ReadCommand)
	require.True(t, ok)
	return cmd
}

// TestGetAsksForATableOnlyForATextTable asserts a get of a kind served by the
// API server's table reads a table when the invocation renders a text table
// and the ordinary objects for every other invocation, including a caller
// that gives no table format at all; and that a kind with columns of its
// own never reads a table.
func TestGetAsksForATableOnlyForATextTable(t *testing.T) {
	t.Parallel()
	newClient := func() *memClient {
		return &memClient{items: []unstructured.Unstructured{podObj("web-0")}, table: engine.TableAnswer{Table: tableFixture(t, "pods")}}
	}
	flags := map[string]string{"cluster": "c-abc", "selector": "app=web"}
	for _, tc := range []struct {
		name      string
		in        runtime.Input
		wantTable bool
	}{
		{"a text table renders", tableInput{s: flags, textTable: true}, true},
		{"no text table renders", tableInput{s: flags}, false},
		{"an Input without the table format", input{s: flags}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newClient()
			res, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(), tc.in, nil)
			require.NoError(t, err)
			assert.Equal(t, "PodList", res.Kind)
			if tc.wantTable {
				assert.Equal(t, []string{""}, client.listTableNS)
				assert.Equal(t, []string{"app=web"}, client.listTableSelector)
				assert.Empty(t, client.listNS, "a table read replaces the list")
				_, ok := res.Data.(serverTable)
				assert.True(t, ok, "got %T", res.Data)
				return
			}
			assert.Empty(t, client.listTableNS)
			assert.Equal(t, []string{"app=web"}, client.listSelector)
			_, ok := res.Data.(table)
			assert.True(t, ok, "got %T", res.Data)
		})
	}

	t.Run("a kind with its own columns", func(t *testing.T) {
		t.Parallel()
		client := newClient()
		_, err := getCommand(t, "cluster", &recFactory{client: client}).Execute(t.Context(), tableInput{textTable: true}, nil)
		require.NoError(t, err)
		assert.Empty(t, client.listTableNS)
		assert.Len(t, client.listNS, 1)
	})
}

// TestGetNamedAsksForATable asserts a named get of a ServerTable kind reads
// the object's table, with the namespace and no NAMESPACE column.
func TestGetNamedAsksForATable(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{podObj("web-0")}, table: engine.TableAnswer{Table: tableFixture(t, "pods")}}
	in := tableInput{args: []string{"web-0"}, s: map[string]string{"cluster": "c-abc", "namespace": "ns-1"}, textTable: true}
	res, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, "Pod", res.Kind)
	assert.Equal(t, []string{"ns-1"}, client.getTableNS)
	assert.Equal(t, []string{"web-0"}, client.getTableName)
	assert.Empty(t, client.getName, "a table read replaces the get")
	st, ok := res.Data.(serverTable)
	require.True(t, ok, "got %T", res.Data)
	assert.Equal(t, "NAME", st.Columns()[0])
}

// TestGetTableErrorsKeepTheirCode asserts a failed table read is returned as
// it came, with no table shown.
func TestGetTableErrorsKeepTheirCode(t *testing.T) {
	t.Parallel()
	denied := clierr.New(clierr.CodeRBAC, "forbidden")
	client := &memClient{tableErr: denied}
	_, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(),
		tableInput{s: map[string]string{"cluster": "c-abc"}, textTable: true}, nil)
	require.ErrorIs(t, err, denied)
}

// TestGetTableNamespaceColumn asserts a namespaced kind listed without
// --namespace shows NAMESPACE first and one listed in a namespace does not;
// a kind with no namespace never does.
func TestGetTableNamespaceColumn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		kind  string
		flags map[string]string
		table string
		first string
	}{
		{"all namespaces", "pod", map[string]string{"cluster": "c-abc"}, "pods", "NAMESPACE"},
		{"one namespace", "pod", map[string]string{"cluster": "c-abc", "namespace": "kube-system"}, "pods", "NAME"},
		{"cluster-scoped kind", "node", map[string]string{"cluster": "c-abc"}, "nodes", "NAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &memClient{table: engine.TableAnswer{Table: tableFixture(t, tc.table)}}
			res, err := getCommand(t, tc.kind, &recFactory{client: client}).Execute(t.Context(),
				tableInput{s: tc.flags, textTable: true}, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.first, res.Data.(output.Tabular).Columns()[0])
		})
	}
}

// projectTable is the pods fixture with the namespace of each row set.
func projectTable(t *testing.T, namespaces ...string) *metav1.Table {
	t.Helper()
	table := tableFixture(t, "pods")
	table.Rows = table.Rows[:len(namespaces)]
	for i, ns := range namespaces {
		table.Rows[i].Object.Object.(*metav1.PartialObjectMetadata).Namespace = ns
	}
	return table
}

// TestGetTableUnderProjectKeepsTheProjectsNamespaces asserts the project's
// member filter reads each row's object metadata: a namespaced kind keeps the
// rows in the project's namespaces, and a row without an object is an error,
// never dropped.
func TestGetTableUnderProjectKeepsTheProjectsNamespaces(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	run := func(t *testing.T, table *metav1.Table) (runtime.Result, error) {
		t.Helper()
		client, fac := newRouteProjectFixture()
		client.tables = map[string]engine.TableAnswer{"pods": {Table: table}}
		ctx, _ := projectContext(t, "ns-a", "ns-b")
		in := tableInput{s: map[string]string{"cluster": "prod", "project": "web"}, textTable: true}
		return NewGet(podKind, fac).(runtime.ReadCommand).Execute(ctx, in, nil)
	}
	t.Run("member and non-member rows", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, projectTable(t, "ns-a", "ns-z", "ns-b", "ns-z"))
		require.NoError(t, err)
		rows := res.Data.(output.Tabular).Rows()
		require.Len(t, rows, 2)
		assert.Equal(t, "ns-a", rows[0][0])
		assert.Equal(t, "ns-b", rows[1][0])
	})
	t.Run("a row without an object", func(t *testing.T) {
		t.Parallel()
		table := projectTable(t, "ns-a", "ns-b")
		table.Rows[1].Object.Object = nil
		_, err := run(t, table)
		require.ErrorContains(t, err, "a row without its object")
	})
}

func annotatedRow(table *metav1.Table, row int, label, annotation string) {
	meta := table.Rows[row].Object.Object.(*metav1.PartialObjectMetadata)
	meta.Labels, meta.Annotations = nil, nil
	if label != "" {
		meta.Labels = map[string]string{"field.cattle.io/projectId": label}
	}
	if annotation != "" {
		meta.Annotations = map[string]string{"field.cattle.io/projectId": annotation}
	}
}

// TestGetNamespaceTableUnderProjectKeepsTheNamespacesTheAnnotationNames
// asserts the list narrowed by the project label keeps the rows whose
// annotation names the project on the cluster: the rows of a table are
// checked as the objects of a list are.
func TestGetNamespaceTableUnderProjectKeepsTheNamespacesTheAnnotationNames(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	table := tableFixture(t, "namespaces")
	table.Rows = append(table.Rows, *table.Rows[0].DeepCopy(), *table.Rows[0].DeepCopy())
	for i, name := range []string{"both", "annotation-only", "label-only", "other-cluster", "other-project"} {
		table.Rows[i].Cells[0] = name
	}
	annotatedRow(table, 0, "p-1", "c-abc:p-1")
	annotatedRow(table, 1, "", "c-abc:p-1")
	annotatedRow(table, 2, "p-1", "")
	annotatedRow(table, 3, "p-1", "c-def:p-1")
	annotatedRow(table, 4, "p-1", "c-abc:p-2")
	client.tables = map[string]engine.TableAnswer{"namespaces": {Table: table}}
	ctx, _ := projectContext(t)
	in := tableInput{s: map[string]string{"cluster": "prod", "project": "web"}, textTable: true}
	res, err := NewGet(mustKind(t, "namespace"), fac).(runtime.ReadCommand).Execute(ctx, in, nil)
	require.NoError(t, err)
	assert.Equal(t, []routeListCall{{resource: "namespaces", selector: "field.cattle.io/projectId=p-1"}}, client.tableCalls)
	var names []string
	for _, row := range res.Data.(output.Tabular).Rows() {
		names = append(names, row[0])
	}
	assert.Equal(t, []string{"both", "annotation-only"}, names)
	assert.Equal(t, "NAME", res.Data.(output.Tabular).Columns()[0], "a cluster-scoped kind has no NAMESPACE column")

	table.Rows[0].Object.Object = nil
	_, err = NewGet(mustKind(t, "namespace"), fac).(runtime.ReadCommand).Execute(ctx, in, nil)
	require.ErrorContains(t, err, "a row without its object")
}

// tableSeqClient serves a wait's polls through seqClient and the table read
// after it.
type tableSeqClient struct {
	*seqClient
	table      engine.TableAnswer
	tableErr   error
	tableCalls int
}

func (c *tableSeqClient) ListTable(context.Context, schema.GroupVersionResource, string, string) (engine.TableAnswer, error) {
	return engine.TableAnswer{}, errors.New("ListTable is not part of a wait")
}

func (c *tableSeqClient) GetTable(context.Context, schema.GroupVersionResource, string, string) (engine.TableAnswer, error) {
	c.tableCalls++
	return c.table, c.tableErr
}

// TestGetWaitThenTable asserts get --wait polls with Get as before and then
// reads the table once; and that a table read that fails after the wait
// shows the polled object's NAME and AGE with a warning that carries the
// error, never fails the command the wait satisfied.
func TestGetWaitThenTable(t *testing.T) {
	withFastPoll(t)
	in := tableInput{
		args:      []string{"web-0"},
		s:         map[string]string{"cluster": "c-abc", "namespace": "ns-1"},
		b:         map[string]bool{"wait": true},
		textTable: true,
	}
	newClient := func() *tableSeqClient {
		return &tableSeqClient{
			seqClient: &seqClient{objs: []*unstructured.Unstructured{conditionObj("Ready", "False"), conditionObj("Ready", "True")}},
			table:     engine.TableAnswer{Table: tableFixture(t, "pods")},
		}
	}
	t.Run("the table is read once after the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := newClient()
			res, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(), in, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, client.tableCalls)
			assert.Equal(t, 2, client.calls, "the wait polled with Get")
			assert.Contains(t, res.Summary, "wait: condition=Ready satisfied after ")
			assert.Empty(t, res.Warnings)
			_, ok := res.Data.(serverTable)
			assert.True(t, ok, "got %T", res.Data)
		})
	})
	t.Run("a failed table read after the wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			client := newClient()
			client.tableErr = clierr.New(clierr.CodeRBAC, "pods is forbidden")
			res, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(), in, nil)
			require.NoError(t, err)
			assert.Contains(t, res.Summary, "wait: condition=Ready satisfied after ")
			assert.Equal(t, []string{"could not read the table after the wait: pods is forbidden; showing NAME and AGE"}, res.Warnings)
			tab, ok := res.Data.(output.Tabular)
			require.True(t, ok)
			assert.Equal(t, []string{"NAME", "AGE"}, tab.Columns())
			assert.Equal(t, "x", tab.Rows()[0][0], "the polled object")
		})
	})
}

// TestGetTableNeedsATableReader asserts a client that cannot read tables is a
// command bug for a ServerTable kind that renders a text table.
func TestGetTableNeedsATableReader(t *testing.T) {
	t.Parallel()
	client := &seqClient{objs: []*unstructured.Unstructured{conditionObj("Ready", "True")}}
	_, err := getCommand(t, "pod", &recFactory{client: client}).Execute(t.Context(),
		tableInput{s: map[string]string{"cluster": "c-abc"}, textTable: true}, nil)
	require.ErrorContains(t, err, "command bug")
}

// TestNewGetRefusesEnrichersForAServerTableKind asserts registering an
// enricher on a ServerTable kind panics, and on any other kind does not.
func TestNewGetRefusesEnrichersForAServerTableKind(t *testing.T) {
	t.Parallel()
	enrich := func(context.Context, *unstructured.Unstructured) (string, bool) { return "", false }
	require.Panics(t, func() { NewGet(mustKind(t, "pod"), nil, enrich) })
	require.NotPanics(t, func() { NewGet(mustKind(t, "pod"), nil) })
	require.NotPanics(t, func() { NewGet(mustKind(t, "feature"), nil, enrich) })
}
