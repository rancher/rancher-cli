package runtime

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// renderCommandCard builds the operator-facing card for one command (SE-1):
// usage line with args, the command-declared flags, the contract line, the
// gate line (SE-3), Desc as body, examples, and a footer of the global
// exit-code strip plus one pointer line to the global flags. Per-command
// error codes are deliberately absent — no per-command source of truth
// exists yet (SE-16). Rendered via Result.Text under default/table only;
// the structured formats keep serving Result.Data.
func renderCommandCard(m Manifest, c SchemaCommand) string {
	var b strings.Builder
	b.WriteString("rancher " + c.Name + argSummary(c.Args) + "\n")
	if c.Usage != "" {
		b.WriteString("  " + c.Usage + "\n")
	}
	b.WriteString(readOnlyLine(m))
	if len(c.Flags) > 0 {
		b.WriteString("\nflags:\n")
		for _, f := range c.Flags {
			b.WriteString("  " + flagLine(f) + "\n")
		}
	}
	b.WriteString("\n" + contractLine(c) + "\n")
	b.WriteString(gateLine(c.CommandType, c.Destructive, c.Stability) + "\n")
	if c.Desc != "" {
		b.WriteString("\n" + strings.TrimRight(c.Desc, "\n") + "\n")
	}
	if len(c.Examples) > 0 {
		b.WriteString("\nexamples:\n")
		for _, e := range c.Examples {
			b.WriteString("  " + e + "\n")
		}
	}
	b.WriteString("\n" + exitCodeStrip(m.ExitCodes) + "\n")
	b.WriteString("global flags: see `rancher schema --all -o json` (globalFlags)\n")
	return b.String()
}

// argSummary renders the positional args for the usage line: <name> when
// required, [name] otherwise; an enum lists its values in place of the name.
func argSummary(args []ArgSpec) string {
	var b strings.Builder
	for _, a := range args {
		token := a.Name
		if len(a.Enum) > 0 {
			token = strings.Join(a.Enum, "|")
		}
		if a.Required {
			b.WriteString(" <" + token + ">")
		} else {
			b.WriteString(" [" + token + "]")
		}
	}
	return b.String()
}

// flagLine renders one command-declared flag: name with short aliases, the
// type (an enum lists its values), required marker, description, and any
// backing env vars.
func flagLine(f ArgSpec) string {
	name := "--" + f.Name
	for _, a := range f.Aliases {
		name += "/-" + a
	}
	typ := f.Type
	if len(f.Enum) > 0 {
		typ = strings.Join(f.Enum, "|")
	}
	line := name + " " + typ
	if f.Required {
		line += " (required)"
	}
	if f.Desc != "" {
		line += "  " + f.Desc
	}
	if len(f.Env) > 0 {
		line += "  [env: " + strings.Join(f.Env, ", ") + "]"
	}
	return line
}

// contractLine renders the command's contract facts in SE-1's order —
// commandType · contract · stability · destructive · network · auth — omitting
// segments with no value (contract/stability unresolved, destructive unset,
// network/auth false).
func contractLine(c SchemaCommand) string {
	parts := []string{string(c.CommandType)}
	if c.Contract != "" {
		parts = append(parts, c.Contract)
	}
	if c.Stability != "" {
		parts = append(parts, c.Stability)
	}
	if c.Destructive != "" {
		parts = append(parts, "destructive="+c.Destructive)
	}
	if c.Network {
		parts = append(parts, "network")
	}
	if c.Auth {
		parts = append(parts, "auth")
	}
	return "contract: " + strings.Join(parts, " · ")
}

// gateLine is SE-3's total function of (commandType, destructive, stability);
// the wordings are contract, pinned verbatim by TestSchemaGateLineMatrix.
// A mutating destructive value other than "true"/"dependsOnInput" (including
// unset) carries no destructive gate; a non-mutating commandType other than
// Local reads as read-only, matching the pipeline (only Mutating is gated).
// Gates is gateLine's structured twin: the flags a command unconditionally
// requires before executing a mutation, as a list the manifest can carry
// per command. A destructive "dependsOnInput" command is not represented
// here beyond --apply — the destructive field signals the conditional
// --yes. Nil for read-only and local commands.
func Gates(ct CommandType, destructive, stability string) []string {
	if ct != Mutating {
		return nil
	}
	gates := []string{"--apply"}
	if destructive == "true" {
		gates = append(gates, "--yes")
	}
	if stability == string(safety.Deprecated) {
		gates = append(gates, "--allow-deprecated")
	}
	return gates
}

func gateLine(ct CommandType, destructive, stability string) string {
	switch ct {
	case Local:
		return "gates: none (local command)"
	case Mutating:
		// The flag set comes from Gates — the single source shared with the
		// manifest's gates field — and only the dependsOnInput prose (a
		// conditional --yes the structured list deliberately omits) is
		// layered on here, right after --apply.
		gates := Gates(ct, destructive, stability)
		g := "gates: " + gates[0]
		if destructive == "dependsOnInput" {
			g += "; --yes when the input is destructive (see description)"
		}
		for _, f := range gates[1:] {
			g += " " + f
		}
		return g
	default:
		return "gates: none (read-only)"
	}
}

// renderKindCard builds the operator-facing card for one kind (SE-5):
// identity (canonical name, key, aliases), the GVR, scope, the namespace
// policy with the routing flags a named op needs, wait default, traits
// (sensitive, completable), and the effective verb table from the manifest's
// SE-17 data. Rendered via Result.Text under default/table only; the
// structured formats keep serving Result.Data.
func renderKindCard(m Manifest, k SchemaKind) string {
	var b strings.Builder
	b.WriteString("kind: " + k.Name + "\n")
	b.WriteString(readOnlyLine(m))
	b.WriteString("key: " + k.Key + "\n")
	if len(k.Aliases) > 0 {
		b.WriteString("aliases: " + strings.Join(k.Aliases, ", ") + "\n")
	}
	if k.Group != "" {
		b.WriteString("group: " + k.Group + "\n")
	}
	if k.Version != "" {
		b.WriteString("version: " + k.Version + "\n")
	}
	b.WriteString("resource: " + k.Resource + "\n")
	b.WriteString("scope: " + k.Scope + "\n")
	b.WriteString(kindNamespaceLine(k) + "\n")
	if k.WaitDefault != "" {
		b.WriteString("wait: bare --wait waits for " + k.WaitDefault + "\n")
	}
	var traits []string
	if k.Sensitive {
		traits = append(traits, "sensitive")
	}
	if k.Completable {
		traits = append(traits, "completable")
	}
	if len(traits) > 0 {
		b.WriteString("traits: " + strings.Join(traits, " · ") + "\n")
	}
	b.WriteString("\nverbs:\n")
	for _, verb := range []string{"get", "patch", "delete"} {
		if vs, ok := k.Verbs[verb]; ok {
			b.WriteString("  " + verbLine(verb, vs) + "\n")
		}
	}
	return b.String()
}

// kindNamespaceLine renders the kind's namespace policy plus the routing
// flags a named op needs — the card counterpart of the kinds package's
// NamespacePolicy semantics, computed from manifest strings only.
func kindNamespaceLine(k SchemaKind) string {
	switch k.NamespacePolicy {
	case "none":
		if k.Scope == "downstream" {
			return "namespace: none (cluster-scoped); named ops need --cluster"
		}
		return "namespace: none (cluster-scoped)"
	case "cluster":
		return "namespace: cluster (the namespace is a cluster id); named ops need --cluster"
	case "workspace":
		return "namespace: workspace; named ops take --namespace (default " + k.DefaultNamespace + ")"
	case "plain":
		if k.Scope == "downstream" {
			return "namespace: plain; named ops need --namespace and --cluster"
		}
		return "namespace: plain; named ops need --namespace"
	default:
		return "namespace: " + k.NamespacePolicy
	}
}

// verbLine renders one verb of the SE-5 table, worded exactly per spec:
// `available (contract · stability)` | `refused — <note>` |
// `not available — <reason>` | `not available`. The degenerate shapes
// (available without a binding, refused without a note) drop the empty
// segment rather than rendering stray punctuation.
func verbLine(verb string, vs VerbState) string {
	switch vs.State {
	case VerbAvailable:
		var meta []string
		if vs.Contract != "" {
			meta = append(meta, vs.Contract)
		}
		if vs.Stability != "" {
			meta = append(meta, vs.Stability)
		}
		if len(meta) > 0 {
			return verb + ": available (" + strings.Join(meta, " · ") + ")"
		}
		return verb + ": available"
	case VerbRefused:
		if vs.Note != "" {
			return verb + ": refused — " + vs.Note
		}
		return verb + ": refused"
	default:
		if vs.Note != "" {
			return verb + ": not available — " + vs.Note
		}
		return verb + ": not available"
	}
}

// readOnlyLine renders the session's read-only banner for the TOC text
// views — both of them, so tocView's unconditional readOnly fields mirror
// text on the bare and filtered paths alike (schema disclosure spec).
func readOnlyLine(m Manifest) string {
	if !m.ReadOnly {
		return ""
	}
	return "read-only (" + strings.Join(m.ReadOnlySources, ", ") + ")\n"
}

// renderTOCText builds the bare-schema TOC text. withManifestLine controls
// whether the "full manifest: rancher schema --all -o json" footer line is
// appended; the bare path includes it, the --all path omits it (the footer
// would be self-referential when --all is already active).
func renderTOCText(m Manifest, withManifestLine bool) string {
	generated := generatedCommandNames(m.Kinds)
	byName := commandIndex(m.Commands)
	genCount := 0
	for _, c := range m.Commands {
		if generated[c.Name] {
			genCount++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rancher %s — %d commands · %d kinds\n", m.CLIVersion, len(m.Commands), len(m.Kinds))
	b.WriteString(readOnlyLine(m))
	blockDone := false
	for _, g := range m.Groups {
		indent := ""
		if g.Name != "" {
			indent = "  "
		}
		var lines []string
		hasGenerated := false
		for _, name := range g.Commands {
			if generated[name] {
				hasGenerated = true
				continue
			}
			c, ok := byName[name]
			if !ok {
				c = SchemaCommand{Name: name}
			}
			lines = append(lines, indent+tocLine(c))
		}
		if hasGenerated && !blockDone {
			blockDone = true
			fmt.Fprintf(&b, "\nget/patch/delete <kind> — %d generated commands across %d kinds\n",
				genCount, len(m.Kinds))
			b.WriteString("  kind view: rancher schema <kind>\n")
		}
		if len(lines) > 0 {
			b.WriteString("\n")
			if g.Name != "" {
				b.WriteString(g.Name + ":\n")
			}
			for _, l := range lines {
				b.WriteString(l + "\n")
			}
		}
	}
	b.WriteString("\n" + exitCodeStrip(m.ExitCodes) + "\n")
	if withManifestLine {
		b.WriteString("full manifest: rancher schema --all -o json\n")
	}
	return b.String()
}

// renderTOC builds the bare-`schema` table of contents (SE-9): a header of
// CLI version plus derived counts, the manifest's ordered sections with one
// line per non-generated command, the generated get/patch/delete commands
// collapsed into ONE kind-vocabulary block at the position of the first
// generated command's group, and a footer of the exit-code strip plus the
// `-o json` pointer. Rendered via Result.Text under default/table only.
func renderTOC(m Manifest) string { return renderTOCText(m, true) }

// renderFilteredTOC builds the filter/search result view (SE-10/SE-11):
// a match-count header naming the active filters, every retained command as
// its own line in pruned-section order (no vocabulary collapse — the matches
// ARE the content), a search's matching fragment where the match is not
// already visible in the line, and the `-o json` pointer. Zero hits render
// the no-matches message alone; the caller returns success either way.
func renderFilteredTOC(m Manifest, f schemaFilters) string {
	filters := strings.Join(f.describe(), " ")
	if len(m.Commands) == 0 {
		return "no commands match " + filters + "\n" + readOnlyLine(m)
	}
	byName := commandIndex(m.Commands)
	var b strings.Builder
	fmt.Fprintf(&b, "%d commands match %s\n", len(m.Commands), filters)
	b.WriteString(readOnlyLine(m))
	for _, g := range m.Groups {
		b.WriteString("\n")
		indent := ""
		if g.Name != "" {
			b.WriteString(g.Name + ":\n")
			indent = "  "
		}
		for _, name := range g.Commands {
			line := tocLine(byName[name])
			if f.search != "" {
				if frag := searchFragment(byName[name], f.search); frag != "" {
					line += "  [" + frag + "]"
				}
			}
			b.WriteString(indent + line + "\n")
		}
	}
	b.WriteString("\nfull manifest: rancher schema --all -o json\n")
	return b.String()
}

// tocLine renders one TOC entry: `name — usage`, or the bare name when the
// command carries no usage line.
func tocLine(c SchemaCommand) string {
	if c.Usage == "" {
		return c.Name
	}
	return c.Name + " — " + c.Usage
}

// generatedCommandNames derives the generated command set from the kind
// vocabulary (SE-17's rule: available and refused verbs are registered
// commands, absent verbs are not) — never from name-shape guessing.
func generatedCommandNames(ks []SchemaKind) map[string]bool {
	out := map[string]bool{}
	for _, k := range ks {
		for verb, vs := range k.Verbs {
			if vs.State == VerbAvailable || vs.State == VerbRefused {
				out[verb+" "+k.Name] = true
			}
		}
	}
	return out
}

// commandIndex maps command names to their manifest entries for the TOC's
// section walk (Groups carry names; lines need usage text).
func commandIndex(cmds []SchemaCommand) map[string]SchemaCommand {
	out := make(map[string]SchemaCommand, len(cmds))
	for _, c := range cmds {
		out[c.Name] = c
	}
	return out
}

// searchFragment shows WHERE a search term matched when the TOC line itself
// does not (SE-11): "" for a name or usage match (already visible), the full
// alias form for an alias match, or an ellipsed fragment of the desc around
// the match. The term is matched case-insensitively, mirroring searchMatches.
func searchFragment(c SchemaCommand, term string) string {
	q := strings.ToLower(term)
	if strings.Contains(strings.ToLower(c.Name), q) ||
		strings.Contains(strings.ToLower(c.Usage), q) {
		return ""
	}
	verb, _, verbed := strings.Cut(c.Name, " ")
	for _, a := range c.Aliases {
		full := a
		if verbed {
			full = verb + " " + a
		}
		if strings.Contains(strings.ToLower(full), q) {
			return "alias: " + full
		}
	}
	if i := strings.Index(strings.ToLower(c.Desc), q); i >= 0 && i+len(term) <= len(c.Desc) {
		return "desc: " + fragmentAround(c.Desc, i, len(term))
	}
	return ""
}

// fragmentContext is how many bytes of surrounding desc a search fragment
// keeps on each side of the match before clipping with ellipses.
const fragmentContext = 20

// fragmentAround clips s to a window around the match at byte offset idx of
// length n, expanding the cut points to rune boundaries and marking clipped
// ends with ellipses.
func fragmentAround(s string, idx, n int) string {
	start := max(idx-fragmentContext, 0)
	end := min(idx+n+fragmentContext, len(s))
	for start > 0 && !utf8.RuneStart(s[start]) {
		start--
	}
	for end < len(s) && !utf8.RuneStart(s[end]) {
		end++
	}
	frag := s[start:end]
	if start > 0 {
		frag = "…" + frag
	}
	if end < len(s) {
		frag += "…"
	}
	return frag
}

// exitCodeStrip renders the global code→exit map as one footer line, ordered
// by exit status then code name so the strip is deterministic.
func exitCodeStrip(exits map[string]int) string {
	codes := make([]string, 0, len(exits))
	for code := range exits {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool {
		if exits[codes[i]] != exits[codes[j]] {
			return exits[codes[i]] < exits[codes[j]]
		}
		return codes[i] < codes[j]
	})
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = fmt.Sprintf("%s=%d", code, exits[code])
	}
	return "exit codes: " + strings.Join(parts, " · ")
}
