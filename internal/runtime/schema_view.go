package runtime

import "encoding/json"

// schemaView is the schema command's Result.Data for every selection (SE-19):
// it wraps the (possibly filtered) manifest and adds the `-o name` contract —
// a single NAME column whose rows are the selection's name set per the SE-2
// matrix (all/filtered command names for bare and filtered selections, the
// command's name for a command selection, the kind's canonical name for a
// kind selection) — while delegating json and yaml marshaling to the wrapped
// manifest, so structured output is byte-identical to marshaling the manifest
// directly. Default/table rendering is unaffected: schema always sets
// Result.Text, which wins over the envelope path.
type schemaView struct {
	manifest Manifest
	names    []string
}

func newSchemaView(m Manifest, names []string) schemaView {
	return schemaView{manifest: m, names: names}
}

func (v schemaView) Columns() []string { return []string{"NAME"} }

func (v schemaView) Rows() [][]string {
	rows := make([][]string, len(v.names))
	for i, n := range v.names {
		rows[i] = []string{n}
	}
	return rows
}

func (v schemaView) MarshalJSON() ([]byte, error) { return json.Marshal(v.manifest) }

func (v schemaView) MarshalYAML() (any, error) { return v.manifest, nil }

// commandNames lists m's command names in manifest order — the bare and
// filtered selections' `-o name` sets.
func commandNames(m Manifest) []string {
	out := make([]string, len(m.Commands))
	for i, c := range m.Commands {
		out[i] = c.Name
	}
	return out
}
