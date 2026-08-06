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

// tocView is the bare and filtered selections' Result.Data: the TOC-shaped
// JSON/YAML mirror of renderTOC/renderFilteredTOC (schema disclosure spec, docs/design). bare gates
// the fields only the bare text TOC renders: the kind vocabulary, its
// count, and the exit-code strip's map. -o name keeps the command-name
// rows schemaView produced for these selections.
type tocView struct {
	manifest Manifest
	bare     bool
}

func newTOCView(m Manifest, bare bool) tocView { return tocView{manifest: m, bare: bare} }

type tocEntry struct {
	Name  string `json:"name"`
	Usage string `json:"usage,omitempty"`
}

type tocGroup struct {
	Name     string     `json:"name,omitempty"`
	Commands []tocEntry `json:"commands"`
}

type tocJSON struct {
	CLIVersion            string         `json:"cliVersion"`
	MinRancherVersion     string         `json:"minRancherVersion"`
	OutputAPIVersion      string         `json:"outputApiVersion"`
	ReadOnly              bool           `json:"readOnly"`
	ReadOnlySources       []string       `json:"readOnlySources,omitempty"`
	CommandCount          int            `json:"commandCount"`
	KindCount             int            `json:"kindCount,omitempty"`
	GeneratedCommandCount int            `json:"generatedCommandCount,omitempty"`
	GlobalExitCodes       map[string]int `json:"globalExitCodes,omitempty"`
	Groups                []tocGroup     `json:"groups,omitempty"`
	Kinds                 []string       `json:"kinds,omitempty"`
}

func (v tocView) toc() tocJSON {
	m := v.manifest
	byName := commandIndex(m.Commands)
	out := tocJSON{
		CLIVersion:        m.CLIVersion,
		MinRancherVersion: m.MinRancherVersion,
		OutputAPIVersion:  m.OutputAPIVersion,
		ReadOnly:          m.ReadOnly,
		ReadOnlySources:   m.ReadOnlySources,
		CommandCount:      len(m.Commands),
	}
	// The bare text TOC collapses generated get/patch/delete <kind>
	// commands into one summary line instead of listing them; the bare
	// JSON mirrors both halves — skip them in groups, carry the count
	// (schema disclosure spec). The filtered TOC never collapses: the matches ARE
	// the content.
	generated := map[string]bool{}
	if v.bare {
		generated = generatedCommandNames(m.Kinds)
	}
	for _, g := range m.Groups {
		tg := tocGroup{Name: g.Name}
		for _, name := range g.Commands {
			if generated[name] {
				continue
			}
			c := byName[name]
			tg.Commands = append(tg.Commands, tocEntry{Name: c.Name, Usage: c.Usage})
		}
		if len(tg.Commands) > 0 {
			out.Groups = append(out.Groups, tg)
		}
	}
	if v.bare {
		out.KindCount = len(m.Kinds)
		// Count generated commands the way renderTOC's genCount does —
		// membership over m.Commands, not len(generated) — so the JSON
		// number and the text's collapsed-line N are the same computation,
		// equal by construction.
		for _, c := range m.Commands {
			if generated[c.Name] {
				out.GeneratedCommandCount++
			}
		}
		out.GlobalExitCodes = m.ExitCodes
		for _, k := range m.Kinds {
			out.Kinds = append(out.Kinds, k.Name)
		}
	}
	return out
}

func (v tocView) Columns() []string { return []string{"NAME"} }

func (v tocView) Rows() [][]string {
	rows := make([][]string, len(v.manifest.Commands))
	for i, c := range v.manifest.Commands {
		rows[i] = []string{c.Name}
	}
	return rows
}

func (v tocView) MarshalJSON() ([]byte, error) { return json.Marshal(v.toc()) }

func (v tocView) MarshalYAML() (any, error) { return v.toc(), nil }

// cardView is a command selection's Result.Data: the selected command's
// fields FLATTENED to the top level (schema disclosure spec — no commands array
// wrapper), the global exit map the text card's strip renders, the
// text card's kind-collision pointer as kindViewHint, and the session's
// read-only state (mirroring the TOC views).
type cardView struct {
	exitCodes       map[string]int
	cmd             SchemaCommand
	kindHint        string
	readOnly        bool
	readOnlySources []string
}

func newCardView(m Manifest, c SchemaCommand, kindHint string) cardView {
	return cardView{
		exitCodes:       m.ExitCodes,
		cmd:             c,
		kindHint:        kindHint,
		readOnly:        m.ReadOnly,
		readOnlySources: m.ReadOnlySources,
	}
}

// cardJSON is the typed marshal shape: embedding flattens the command's
// fields to the top level while keeping deterministic field order for
// BOTH encoders (an untyped map would be encoder-defined in YAML —
// plan-review round 3, P3). ReadOnly is unconditional (mirrors tocJSON);
// ReadOnlySources carries omitempty.
type cardJSON struct {
	SchemaCommand
	ReadOnly        bool           `json:"readOnly"`
	ReadOnlySources []string       `json:"readOnlySources,omitempty"`
	GlobalExitCodes map[string]int `json:"globalExitCodes"`
	KindViewHint    string         `json:"kindViewHint,omitempty"`
}

func (v cardView) card() cardJSON {
	return cardJSON{
		SchemaCommand:   v.cmd,
		ReadOnly:        v.readOnly,
		ReadOnlySources: v.readOnlySources,
		GlobalExitCodes: v.exitCodes,
		KindViewHint:    v.kindHint,
	}
}

func (v cardView) Columns() []string { return []string{"NAME"} }

func (v cardView) Rows() [][]string { return [][]string{{v.cmd.Name}} }

func (v cardView) MarshalJSON() ([]byte, error) { return json.Marshal(v.card()) }

func (v cardView) MarshalYAML() (any, error) { return v.card(), nil }

// kindView is a kind selection's Result.Data: the SchemaKind row alone —
// verb state, not per-verb flags — mirroring renderKindCard (schema disclosure spec;
// the per-verb flag detail is one more call away: schema <verb> <kind>).
// The session's read-only state is included, mirroring the TOC and card views.
type kindView struct {
	kind            SchemaKind
	readOnly        bool
	readOnlySources []string
}

func newKindView(m Manifest, k SchemaKind) kindView {
	return kindView{kind: k, readOnly: m.ReadOnly, readOnlySources: m.ReadOnlySources}
}

// kindJSON is the typed marshal shape for a kind selection: the kind row's
// fields at top level plus the session's read-only state, mirroring the
// card and TOC views. ReadOnly is unconditional; ReadOnlySources carries omitempty.
type kindJSON struct {
	SchemaKind
	ReadOnly        bool     `json:"readOnly"`
	ReadOnlySources []string `json:"readOnlySources,omitempty"`
}

func (v kindView) Columns() []string { return []string{"NAME"} }

func (v kindView) Rows() [][]string { return [][]string{{v.kind.Name}} }

func (v kindView) MarshalJSON() ([]byte, error) {
	return json.Marshal(kindJSON{SchemaKind: v.kind, ReadOnly: v.readOnly, ReadOnlySources: v.readOnlySources})
}

func (v kindView) MarshalYAML() (any, error) {
	return kindJSON{SchemaKind: v.kind, ReadOnly: v.readOnly, ReadOnlySources: v.readOnlySources}, nil
}

// commandNames lists m's command names in manifest order — the bare and
// filtered selections' `-o name` sets.
func commandNames(m Manifest) []string {
	out := make([]string, len(m.Commands))
	for i, c := range m.Commands {
		out[i] = c.Name
	}
	return out
}
