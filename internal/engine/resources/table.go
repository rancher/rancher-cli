package resources

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/engine"
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
// Fields listed in the kind's RedactPaths are removed before marshaling.
func (t table) MarshalJSON() ([]byte, error) {
	if t.single {
		if len(t.items) != 1 {
			return nil, fmt.Errorf("table: a single result must have exactly 1 item, got %d", len(t.items))
		}
		return json.Marshal(engine.ScrubPaths(t.items[0].Object, t.k.RedactPaths))
	}
	objs := make([]map[string]any, len(t.items))
	for i := range t.items {
		objs[i] = engine.ScrubPaths(t.items[i].Object, t.k.RedactPaths)
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
			row = append(row, columnString(o, c))
		}
		rows = append(rows, row)
	}
	return rows
}

// conditionCell derives the cell from the named status condition: True→type
// name, False→"Not<Type>", anything else verbatim, absent/degenerate→"".
func conditionCell(o map[string]any, c kinds.Column) string {
	status, _, ok := conditionStatus(o, c.Condition)
	switch {
	case !ok:
		return ""
	case status == "True":
		return c.Condition
	case status == "False":
		return "Not" + c.Condition
	default:
		return status
	}
}

// pathCell projects a column value from Path/AltPaths/Bool/Default/Join:
// Path first, then each AltPaths entry, first non-empty string wins. The
// Bool absent→"false" fallback applies to Path only, preserving the
// single-path semantics documented on Column.Bool (nodedriver's
// ACTIVE/BUILTIN). Join renders a string-array Path comma-joined in server
// order; Default renders when every lookup yields nothing.
func pathCell(o map[string]any, c kinds.Column) string {
	if c.Join {
		if vs, ok, _ := unstructured.NestedStringSlice(o, c.Path...); ok && len(vs) > 0 {
			return strings.Join(vs, ",")
		}
		return c.Default
	}
	v, ok, _ := unstructured.NestedString(o, c.Path...)
	if ok && v != "" {
		return v
	}
	for _, alt := range c.AltPaths {
		if av, aok, _ := unstructured.NestedString(o, alt...); aok && av != "" {
			return av
		}
	}
	if !ok {
		if b, bok, _ := unstructured.NestedBool(o, c.Path...); bok {
			return strconv.FormatBool(b)
		}
		if c.Bool {
			return "false"
		}
		if c.Default != "" {
			return c.Default
		}
	}
	return v
}

// columnString projects one column from one object. validateKinds guarantees
// Condition and Path/AltPaths/Bool are mutually exclusive, so the dispatcher
// selects exactly one projection mode.
func columnString(o map[string]any, c kinds.Column) string {
	if c.Condition != "" {
		return conditionCell(o, c)
	}
	return pathCell(o, c)
}
