package resources

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// noTableWarning is shown when the server answered a table request with
// something other than a usable Table.
const noTableWarning = "the server answered without a table; showing NAME and AGE"

// waitTableWarning is shown when the table could not be read after a wait.
func waitTableWarning(err error) string {
	return "could not read the table after the wait: " + err.Error() + "; showing NAME and AGE"
}

// serverTable is the text table of a kind whose columns the cluster's API
// server decides, as kubectl shows them: the Table's column names in the
// server's order, upper-cased; the columns with a non-zero priority only
// under -o wide; and each cell as kubectl prints it. A namespaced kind
// listed across namespaces gets a NAMESPACE column first, read from each
// row's object metadata. It implements output.WideTabular. It never reads a
// kind's Columns. Structured formats never reach it: the command asks for a
// Table only when a text table renders, so marshaling it is a command bug.
type serverTable struct {
	defs      []metav1.TableColumnDefinition
	rows      []metav1.TableRow
	namespace []string // one per row, set when the NAMESPACE column shows
	showNS    bool
}

var _ output.WideTabular = serverTable{}

// newServerTable builds the payload for t. showNamespace adds the NAMESPACE
// column; a row without an object then has no namespace to show, which is an
// error rather than a blank cell.
func newServerTable(t *metav1.Table, showNamespace bool) (serverTable, error) {
	st := serverTable{defs: t.ColumnDefinitions, rows: t.Rows, showNS: showNamespace}
	if showNamespace {
		st.namespace = make([]string, len(t.Rows))
		for i := range t.Rows {
			meta, ok := t.Rows[i].Object.Object.(metav1.Object)
			if !ok {
				return serverTable{}, rowWithoutObjectError()
			}
			st.namespace[i] = meta.GetNamespace()
		}
	}
	return st, nil
}

func rowWithoutObjectError() error {
	return errors.New("the server's table has a row without its object; the namespace and project of the row cannot be read")
}

// MarshalJSON refuses: a structured format is never rendered from the
// table.
func (serverTable) MarshalJSON() ([]byte, error) {
	return nil, errors.New("command bug: the server's table renders only as a text table")
}

// Columns is the -o table column set.
func (t serverTable) Columns() []string { return t.header(false) }

// Rows is Columns' matching row data.
func (t serverTable) Rows() [][]string { return t.body(false) }

// WideColumns is the -o wide column set: every column, the priority ones
// included, in the server's order.
func (t serverTable) WideColumns() []string { return t.header(true) }

// WideRows is WideColumns' matching row data.
func (t serverTable) WideRows() [][]string { return t.body(true) }

// shown reports whether column i shows: every column under -o wide, else the
// priority 0 ones.
func (t serverTable) shown(i int, wide bool) bool {
	return wide || t.defs[i].Priority == 0
}

func (t serverTable) header(wide bool) []string {
	cols := make([]string, 0, len(t.defs)+1)
	if t.showNS {
		cols = append(cols, "NAMESPACE")
	}
	for i, d := range t.defs {
		if t.shown(i, wide) {
			cols = append(cols, strings.ToUpper(d.Name))
		}
	}
	return cols
}

func (t serverTable) body(wide bool) [][]string {
	out := make([][]string, 0, len(t.rows))
	for r, row := range t.rows {
		cells := make([]string, 0, len(t.defs)+1)
		if t.showNS {
			cells = append(cells, t.namespace[r])
		}
		for i := range t.defs {
			if !t.shown(i, wide) {
				continue
			}
			var v any
			if i < len(row.Cells) {
				v = row.Cells[i]
			}
			cells = append(cells, cellString(v))
		}
		out = append(out, cells)
	}
	return out
}

// cellString renders one Table cell as kubectl does: nil is empty, a string
// is shown as it is, any other value as fmt.Sprint prints it, and a value
// that runs past its first line (a newline, carriage return or form feed) is
// cut there and ends in "...". The table writer then removes control and
// format characters.
func cellString(v any) string {
	var s string
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		s = c
	default:
		s = fmt.Sprint(c)
	}
	if i := strings.IndexAny(s, "\n\r\f"); i >= 0 {
		return s[:i] + "..."
	}
	return s
}

// tableData builds the Data of a get answered with a table request: the
// server's table, or, when the server sent no usable Table, the NAME and AGE
// table of the objects it sent with a warning. scope filters the rows to the
// project (nil is no filter); showNamespace adds the NAMESPACE column.
func tableData(k kinds.Kind, ans engine.TableAnswer, single bool, scope *ProjectScope, showNamespace bool) (any, []string, error) {
	if ans.Table == nil || len(ans.Table.ColumnDefinitions) == 0 {
		items := ans.Items
		if ans.Table != nil {
			var err error
			if items, err = objectsOfRows(ans.Table, scope != nil); err != nil {
				return nil, nil, err
			}
		}
		return newTable(k, inScopeItems(items, scope), single), []string{noTableWarning}, nil
	}
	t := *ans.Table
	if scope != nil {
		rows := make([]metav1.TableRow, 0, len(t.Rows))
		for _, row := range t.Rows {
			meta, ok := row.Object.Object.(metav1.Object)
			if !ok {
				return nil, nil, rowWithoutObjectError()
			}
			if inScope(scope, meta) {
				rows = append(rows, row)
			}
		}
		t.Rows = rows
	}
	st, err := newServerTable(&t, showNamespace)
	if err != nil {
		return nil, nil, err
	}
	return st, nil, nil
}

// inScopeItems keeps the objects of scope.
func inScopeItems(items []unstructured.Unstructured, scope *ProjectScope) []unstructured.Unstructured {
	if scope == nil {
		return items
	}
	kept := make([]unstructured.Unstructured, 0, len(items))
	for i := range items {
		if inScope(scope, &items[i]) {
			kept = append(kept, items[i])
		}
	}
	return kept
}

// objectsOfRows turns the rows of a Table that has no column definitions into
// objects with the name and creation time the rows carry, so NAME and AGE
// can show: the row's object metadata, else its first cell as the name. When
// the namespace of each row is needed (needObjects), a row without its object
// is an error.
func objectsOfRows(t *metav1.Table, needObjects bool) ([]unstructured.Unstructured, error) {
	items := make([]unstructured.Unstructured, 0, len(t.Rows))
	for _, row := range t.Rows {
		meta := map[string]any{}
		m, ok := row.Object.Object.(*metav1.PartialObjectMetadata)
		switch {
		case ok:
			if u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&m.ObjectMeta); err == nil {
				meta = u
			}
		case needObjects:
			return nil, rowWithoutObjectError()
		case len(row.Cells) > 0:
			if name, ok := row.Cells[0].(string); ok {
				meta["name"] = name
			}
		}
		items = append(items, unstructured.Unstructured{Object: map[string]any{"metadata": meta}})
	}
	return items, nil
}
