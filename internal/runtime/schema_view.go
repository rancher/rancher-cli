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
	Name      string                 `json:"name,omitempty"`
	Desc      string                 `json:"desc,omitempty"`
	Commands  []tocEntry             `json:"commands,omitempty"`
	Generated *groupGeneratedSummary `json:"generated,omitempty"`
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
// GeneratedCommandCount, GlobalExitCodes, EffectSchema, and Kinds carry
// omitempty: they are zero/nil on the filtered path (bare-only gating), so
// they are absent there. A genuinely-zero GeneratedCommandCount or KindCount
// on the bare path is unreachable — the kind table is static and validated
// non-empty. EffectSchema is the second global contract surfaced on the bare
// TOC beside the exit map: an agent that just previewed a mutation can parse
// the Effect without fetching the full --all manifest. JSON/YAML only — the
// text TOC does not render it.
type tocJSON struct {
	CLIVersion        string `json:"cliVersion"`
	MinRancherVersion string `json:"minRancherVersion"`
	OutputAPIVersion  string `json:"outputApiVersion"`
	readOnlyJSON
	CommandCount          int            `json:"commandCount"`
	KindCount             int            `json:"kindCount,omitempty"`
	GeneratedCommandCount int            `json:"generatedCommandCount,omitempty"`
	GlobalExitCodes       map[string]int `json:"globalExitCodes,omitempty"`
	EffectSchema          *EffectSchema  `json:"effectSchema,omitempty"`
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
	// The bare text TOC collapses generated get/patch/delete <kind> commands
	// into one summary line instead of listing them individually; the bare JSON
	// mirrors both halves — skip generated in the per-command list, add a
	// Generated summary to all-generated sections so they remain visible
	// (schema disclosure spec). The filtered TOC never collapses: the matches
	// ARE the content.
	generated := map[string]bool{}
	if v.bare {
		generated = generatedCommandNames(m.Kinds)
	}
	for _, g := range m.Groups {
		bespoke, genCount := partitionGroup(g, byName, generated)
		tg := tocGroup{Name: g.Name, Desc: g.Desc}
		for _, c := range bespoke {
			tg.Commands = append(tg.Commands, tocEntry{Name: c.Name, Usage: c.Usage})
		}
		if v.bare && genCount > 0 {
			tg.Generated = &groupGeneratedSummary{Count: genCount, Pattern: g.Name + " <kind>"}
		}
		if len(tg.Commands) > 0 || tg.Generated != nil {
			out.Groups = append(out.Groups, tg)
		}
	}
	if v.bare {
		out.KindCount = len(m.Kinds)
		// Shared computation via countGenerated — same function renderTOCText
		// uses for its collapsed-line N, so JSON and text are always in sync.
		out.GeneratedCommandCount = countGenerated(m.Commands, generated)
		out.GlobalExitCodes = m.ExitCodes
		es := m.EffectSchema
		out.EffectSchema = &es
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

// groupView is a group selection's Result.Data: the group name, desc, bespoke
// member commands, and — when the group contains generated per-kind commands
// — a collapsed count+pattern rather than N per-kind lines. Generated commands
// are identified via the same generatedCommandNames map the TOC uses, so the
// two collapse paths are always consistent. The session's read-only state is
// included, mirroring the other card shapes.
type groupView struct {
	group            SchemaGroup
	bespoke          []SchemaCommand // non-generated members, listed individually
	generatedCount   int             // 0 for groups with no generated members
	generatedPattern string          // e.g. "get <kind>"; empty when generatedCount == 0
	readOnly         bool
	readOnlySources  []string
}

// partitionGroup splits g's Commands into bespoke (non-generated) commands
// and a count of generated per-kind commands. Commands not found in idx fall
// back to SchemaCommand{Name: name} — consistent with tocView.toc() and
// renderTOCText, so no member is silently dropped. The caller builds both
// maps once per manifest and passes them in to avoid per-group recomputation.
func partitionGroup(g SchemaGroup, idx map[string]SchemaCommand, generated map[string]bool) (bespoke []SchemaCommand, genCount int) {
	for _, name := range g.Commands {
		if generated[name] {
			genCount++
			continue
		}
		c, ok := idx[name]
		if !ok {
			c = SchemaCommand{Name: name}
		}
		bespoke = append(bespoke, c)
	}
	return
}

// newGroupView splits g's members into bespoke and generated using the shared
// partitionGroup helper, ensuring text and JSON cards are consistent with the
// TOC. partitionGroup falls back to SchemaCommand{Name: name} for unknown
// members — matching tocView.toc()/renderTOCText — so no member is silently
// dropped.
func newGroupView(m Manifest, g SchemaGroup) groupView {
	generated := generatedCommandNames(m.Kinds)
	idx := commandIndex(m.Commands)
	bespoke, genCount := partitionGroup(g, idx, generated)
	pattern := ""
	if genCount > 0 {
		pattern = g.Name + " <kind>"
	}
	return groupView{
		group:            g,
		bespoke:          bespoke,
		generatedCount:   genCount,
		generatedPattern: pattern,
		readOnly:         m.ReadOnly,
		readOnlySources:  m.ReadOnlySources,
	}
}

// groupGeneratedSummary is the collapsed representation of per-kind generated
// commands in the group card's JSON shape — mirrors the TOC's
// GeneratedCommandCount/Kinds treatment at the per-group level.
type groupGeneratedSummary struct {
	Count   int    `json:"count"`
	Pattern string `json:"pattern"`
}

// groupJSON is the typed marshal shape for a group selection: name, desc,
// bespoke commands as {name,usage} pairs reusing tocEntry (byte-identical to
// the TOC's per-entry shape), an optional generated summary, and the session
// read-only state — consistent with cardJSON and kindJSON.
type groupJSON struct {
	Name      string                 `json:"name"`
	Desc      string                 `json:"desc,omitempty"`
	Commands  []tocEntry             `json:"commands"`
	Generated *groupGeneratedSummary `json:"generated,omitempty"`
	readOnlyJSON
}

func (v groupView) Columns() []string { return []string{"NAME"} }

func (v groupView) Rows() [][]string { return [][]string{{v.group.Name}} }

func (v groupView) MarshalJSON() ([]byte, error) {
	entries := make([]tocEntry, len(v.bespoke))
	for i, c := range v.bespoke {
		entries[i] = tocEntry{Name: c.Name, Usage: c.Usage}
	}
	var gen *groupGeneratedSummary
	if v.generatedCount > 0 {
		gen = &groupGeneratedSummary{Count: v.generatedCount, Pattern: v.generatedPattern}
	}
	return json.Marshal(groupJSON{
		Name:         v.group.Name,
		Desc:         v.group.Desc,
		Commands:     entries,
		Generated:    gen,
		readOnlyJSON: newReadOnlyJSON(v.readOnly, v.readOnlySources),
	})
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
