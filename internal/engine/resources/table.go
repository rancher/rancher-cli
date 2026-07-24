package resources

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// table projects objects to NAME plus the kind's extra columns for -o table
// and -o name. It implements output.Tabular AND json.Marshaler: -o json/yaml
// must see the real object(s), not the table projection, so MarshalJSON
// bypasses Columns/Rows entirely and emits the underlying object(s).
type table struct {
	k      kinds.Kind
	items  []unstructured.Unstructured
	single bool // true for a named get (one object), false for a list
}

func newTable(k kinds.Kind, items []unstructured.Unstructured, single bool) table {
	return table{k: k, items: items, single: single}
}

// MarshalJSON emits the bare object for a named get, or a bare array of
// objects for a list — never {"items":...} — so gjson paths like
// `data.metadata.name` (single) and `data.0.metadata.name`/`data.#` (list)
// address the real object graph. sigs.k8s.io/yaml marshals through JSON, so
// this fixes -o yaml too; no separate MarshalYAML is needed.
func (t table) MarshalJSON() ([]byte, error) {
	if t.single {
		if len(t.items) != 1 {
			return nil, fmt.Errorf("table: a single result must have exactly 1 item, got %d", len(t.items))
		}
		return json.Marshal(t.items[0].Object)
	}
	objs := make([]map[string]any, len(t.items))
	for i := range t.items {
		objs[i] = t.items[i].Object
	}
	return json.Marshal(objs)
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
			v, ok, _ := unstructured.NestedString(o, c.Path...)
			if !ok {
				// Bool leaves render "true"/"false" (nodedriver's ACTIVE/
				// BUILTIN are bools in the served v3 NodeDriver). An ABSENT
				// field in a Bool-marked column renders "false": an omitempty
				// bool (NodeDriverSpec's builtin) drops false from the wire,
				// so blank would misread as unknown. Other non-string leaves
				// (int64, array) still render blank — rows keep omitting such
				// columns deliberately (token, kubeconfig).
				if b, bok, _ := unstructured.NestedBool(o, c.Path...); bok {
					v = strconv.FormatBool(b)
				} else if c.Bool {
					v = "false"
				}
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	return rows
}
