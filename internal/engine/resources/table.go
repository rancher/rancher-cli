package resources

import (
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// table projects objects to NAME plus the kind's extra columns for -o table
// and -o name. It implements output.Tabular.
type table struct {
	k     kinds.Kind
	items []unstructured.Unstructured
}

func newTable(k kinds.Kind, items []unstructured.Unstructured) table {
	return table{k: k, items: items}
}

func (t table) Columns() []string {
	cols := make([]string, 0, len(t.k.Columns)+1)
	cols = append(cols, "NAME")
	for _, c := range t.k.Columns {
		cols = append(cols, c.Header)
	}
	return cols
}

func (t table) Rows() [][]string {
	rows := make([][]string, 0, len(t.items))
	for i := range t.items {
		o := t.items[i].Object
		row := make([]string, 0, len(t.k.Columns)+1)
		row = append(row, t.items[i].GetName())
		for _, c := range t.k.Columns {
			v, _, _ := unstructured.NestedString(o, c.Path...)
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	return rows
}
