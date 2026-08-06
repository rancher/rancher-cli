package runtime

import "encoding/json"

// schemaView is --all's Result.Data: it wraps the full manifest and provides
// the -o name contract — a single NAME column whose rows are every command
// name in manifest order. Structured output (-o json/-o yaml) is
// byte-identical to marshaling the manifest directly; -o name lists all
// names including generated commands. Per-selection roles (bare TOC, command
// card, kind card) moved to tocView, cardView, and kindView respectively.
type schemaView struct {
	manifest Manifest
}

func newSchemaView(m Manifest) schemaView {
	return schemaView{manifest: m}
}

func (v schemaView) Columns() []string { return []string{"NAME"} }

func (v schemaView) Rows() [][]string { return namesToRows(commandNames(v.manifest)) }

func (v schemaView) MarshalJSON() ([]byte, error) { return json.Marshal(v.manifest) }

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

// readOnlyJSON is the session read-only pair shared by all three JSON shapes
// (tocJSON, cardJSON, kindJSON). Embedding it keeps the pair in sync and
// the wire format is byte-identical to the three hand-copied pairs it replaces
// — ReadOnly is unconditional, ReadOnlySources carries omitempty.
type readOnlyJSON struct {
	ReadOnly        bool     `json:"readOnly"`
	ReadOnlySources []string `json:"readOnlySources,omitempty"`
}

// tocJSON is the bare and filtered TOC's JSON/YAML shape. KindCount,
// GeneratedCommandCount, GlobalExitCodes, and Kinds carry omitempty: they
// are zero/nil on the filtered path (bare-only gating), so they are absent
// there. A genuinely-zero GeneratedCommandCount or KindCount on the bare
// path is unreachable — the kind table is static and validated non-empty.
type tocJSON struct {
	CLIVersion        string `json:"cliVersion"`
	MinRancherVersion string `json:"minRancherVersion"`
	OutputAPIVersion  string `json:"outputApiVersion"`
	readOnlyJSON
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
		readOnlyJSON:      newReadOnlyJSON(m.ReadOnly, m.ReadOnlySources),
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
			c, ok := byName[name]
			if !ok {
				c = SchemaCommand{Name: name}
			}
			tg.Commands = append(tg.Commands, tocEntry{Name: c.Name, Usage: c.Usage})
		}
		if len(tg.Commands) > 0 {
			out.Groups = append(out.Groups, tg)
		}
	}
	if v.bare {
		out.KindCount = len(m.Kinds)
		// Shared computation via countGenerated — same function renderTOCText
		// uses for its collapsed-line N, so JSON and text are always in sync.
		out.GeneratedCommandCount = countGenerated(m.Commands, generated)
		out.GlobalExitCodes = m.ExitCodes
		for _, k := range m.Kinds {
			out.Kinds = append(out.Kinds, k.Name)
		}
	}
	return out
}

func (v tocView) Columns() []string { return []string{"NAME"} }

func (v tocView) Rows() [][]string { return namesToRows(commandNames(v.manifest)) }

func (v tocView) MarshalJSON() ([]byte, error) { return json.Marshal(v.toc()) }

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
// plan-review round 3, P3). readOnlyJSON is embedded (mirrors tocJSON and
// kindJSON); GlobalExitCodes is unconditional on the card.
type cardJSON struct {
	SchemaCommand
	readOnlyJSON
	GlobalExitCodes map[string]int `json:"globalExitCodes"`
	KindViewHint    string         `json:"kindViewHint,omitempty"`
}

func (v cardView) card() cardJSON {
	return cardJSON{
		SchemaCommand:   v.cmd,
		readOnlyJSON:    newReadOnlyJSON(v.readOnly, v.readOnlySources),
		GlobalExitCodes: v.exitCodes,
		KindViewHint:    v.kindHint,
	}
}

func (v cardView) Columns() []string { return []string{"NAME"} }

func (v cardView) Rows() [][]string { return [][]string{{v.cmd.Name}} }

func (v cardView) MarshalJSON() ([]byte, error) { return json.Marshal(v.card()) }

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
// card and TOC views. readOnlyJSON is embedded (mirrors tocJSON and cardJSON).
// GlobalExitCodes is absent here — renderKindCard renders no exit-code strip,
// so the kind JSON carries none either. cardJSON differs because the command
// card's text DOES render the strip. An agent doing the two-step kind lookup
// (schema <kind> --kind, then schema <verb> <kind>) gets globalExitCodes from
// the per-verb command card call.
type kindJSON struct {
	SchemaKind
	readOnlyJSON
}

func (v kindView) Columns() []string { return []string{"NAME"} }

func (v kindView) Rows() [][]string { return [][]string{{v.kind.Name}} }

func (v kindView) MarshalJSON() ([]byte, error) {
	return json.Marshal(kindJSON{SchemaKind: v.kind, readOnlyJSON: newReadOnlyJSON(v.readOnly, v.readOnlySources)})
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

// namesToRows converts a name slice into the single-column row matrix Rows()
// returns — shared by schemaView and tocView whose Rows() bodies are otherwise
// byte-identical.
func namesToRows(names []string) [][]string {
	rows := make([][]string, len(names))
	for i, n := range names {
		rows[i] = []string{n}
	}
	return rows
}

// newReadOnlyJSON constructs the readOnlyJSON pair from its two fields —
// shared by the three call sites in toc(), card(), and MarshalJSON().
func newReadOnlyJSON(readOnly bool, sources []string) readOnlyJSON {
	return readOnlyJSON{ReadOnly: readOnly, ReadOnlySources: sources}
}
