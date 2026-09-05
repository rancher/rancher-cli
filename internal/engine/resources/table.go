package resources

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/duration"
)

// now is AGE's clock, overridden in tests for a deterministic duration.
var now = time.Now

// table projects objects to NAME plus the kind's extra columns for -o table,
// -o wide, and -o name. It implements output.WideTabular (Columns/Rows for
// -o table filter out Wide-marked columns; WideColumns/WideRows for -o wide
// return the full set) AND json.Marshaler: -o json/yaml must see the real
// object(s), not the table projection, so MarshalJSON bypasses
// Columns/Rows entirely and emits the underlying object(s).
type table struct {
	k      kinds.Kind
	items  []unstructured.Unstructured
	single bool // true for a named get (one object), false for a list
}

var _ output.WideTabular = table{}

func newTable(k kinds.Kind, items []unstructured.Unstructured, single bool) table {
	return table{k: k, items: items, single: single}
}

// MarshalJSON emits the bare object for a named get, or a bare array of
// objects for a list - never {"items":...} - so gjson paths like
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

// Columns is the -o table column set: NAME plus every non-Wide column, plus
// AGE last, in declaration order.
func (t table) Columns() []string {
	cols := make([]string, 0, len(t.k.Columns)+2)
	cols = append(cols, "NAME")
	for _, c := range t.k.Columns {
		if c.Wide {
			continue
		}
		cols = append(cols, c.Header)
	}
	cols = append(cols, "AGE")
	return cols
}

// Rows is Columns' matching row data.
func (t table) Rows() [][]string {
	rows := make([][]string, 0, len(t.items))
	for i := range t.items {
		o := t.items[i].Object
		row := make([]string, 0, len(t.k.Columns)+2)
		row = append(row, t.items[i].GetName())
		for _, c := range t.k.Columns {
			if c.Wide {
				continue
			}
			row = append(row, columnString(o, c))
		}
		row = append(row, ageCell(o))
		rows = append(rows, row)
	}
	return rows
}

// WideColumns is the -o wide column set: NAME plus every column, Wide or
// not, plus AGE last - the output.WideTabular superset.
func (t table) WideColumns() []string {
	cols := make([]string, 0, len(t.k.Columns)+2)
	cols = append(cols, "NAME")
	for _, c := range t.k.Columns {
		cols = append(cols, c.Header)
	}
	cols = append(cols, "AGE")
	return cols
}

// WideRows is WideColumns' matching row data.
func (t table) WideRows() [][]string {
	rows := make([][]string, 0, len(t.items))
	for i := range t.items {
		o := t.items[i].Object
		row := make([]string, 0, len(t.k.Columns)+2)
		row = append(row, t.items[i].GetName())
		for _, c := range t.k.Columns {
			row = append(row, columnString(o, c))
		}
		row = append(row, ageCell(o))
		rows = append(rows, row)
	}
	return rows
}

// ageCell renders metadata.creationTimestamp as kubectl's own AGE column
// does - duration.HumanDuration since now() - built in on every KIND table,
// like NAME. A missing or unparseable timestamp renders blank: nothing is
// fabricated, no placeholder.
func ageCell(o map[string]any) string {
	ts, ok, _ := unstructured.NestedString(o, "metadata", "creationTimestamp")
	if !ok || ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return duration.HumanDuration(now().Sub(t))
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

// pathCell projects a column value from Path/AltPaths/Bool/Default/Join.
// Join renders a string-array Path comma-joined in server order. Otherwise
// every path - Path first, then each AltPaths entry, in order - is tried
// through the typed leaf ladder (see ladderCell); the first path with a
// present leaf wins. When no path yields anything: the Bool marker's
// absent→"false" fallback applies, then Default, then blank.
func pathCell(o map[string]any, c kinds.Column) string {
	if c.Join {
		if vs, ok, _ := unstructured.NestedStringSlice(o, c.Path...); ok && len(vs) > 0 {
			return strings.Join(vs, ",")
		}
		return c.Default
	}
	if v, ok := ladderCell(o, c.Path); ok {
		return v
	}
	for _, alt := range c.AltPaths {
		if v, ok := ladderCell(o, alt); ok {
			return v
		}
	}
	if c.Bool {
		return "false"
	}
	return c.Default
}

// ladderCell tries one path through the typed leaf ladder: NestedString →
// NestedBool → NestedInt64 → NestedFloat64 - the first present leaf renders
// (string: as-is; bool: strconv.FormatBool; int64: strconv.FormatInt;
// float64: strconv.FormatFloat with 'g' and the shortest round-trip
// precision, which may emit exponent form for extreme magnitudes). A leaf
// that is present as a string but empty is NOT considered present - the
// caller falls through to the next path, preserving the existing
// first-non-empty-string-wins contract for AltPaths. A present zero (int64
// or float64) renders as a real value, "0", never blank.
func ladderCell(o map[string]any, path []string) (string, bool) {
	if v, ok, _ := unstructured.NestedString(o, path...); ok {
		if v == "" {
			return "", false
		}
		return v, true
	}
	if b, ok, _ := unstructured.NestedBool(o, path...); ok {
		return strconv.FormatBool(b), true
	}
	if i, ok, _ := unstructured.NestedInt64(o, path...); ok {
		return strconv.FormatInt(i, 10), true
	}
	if f, ok, _ := unstructured.NestedFloat64(o, path...); ok {
		return strconv.FormatFloat(f, 'g', -1, 64), true
	}
	return "", false
}

// countCell renders len() of the map or slice at Count's path: absent →
// blank, present-but-empty → "0" (a real count, never fabricated).
func countCell(o map[string]any, c kinds.Column) string {
	v, ok, _ := unstructured.NestedFieldNoCopy(o, c.Count...)
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case map[string]any:
		return strconv.Itoa(len(val))
	case []any:
		return strconv.Itoa(len(val))
	default:
		return ""
	}
}

// columnString projects one column from one object. validateKinds guarantees
// Condition, Count, and Path/AltPaths/Bool/Default/Join are mutually
// exclusive, so the dispatcher selects exactly one projection mode.
func columnString(o map[string]any, c kinds.Column) string {
	switch {
	case c.Condition != "":
		return conditionCell(o, c)
	case len(c.Count) > 0:
		return countCell(o, c)
	default:
		return pathCell(o, c)
	}
}
