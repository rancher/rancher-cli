package runtime

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// schemaCmd emits the capability manifest. It is readOnly (produces no server
// mutation) and needs no auth.
type schemaCmd struct {
	reg               *Registry
	cliVersion        string
	minRancherVersion string
	// readOnly resolves this invocation's read-only state (config + env +
	// flag OR'd) at Execute time. schema is wired via Adapt, which never
	// calls resolveDeps, so it has no other way to see --config/--read-only
	// or the loaded config — surface/cli hands over the SAME resolution
	// buildIO uses for every other command (one resolver, two consumers).
	// nil is treated as "inactive" (registry_test's stub registrations).
	readOnly func() (bool, []string)
}

// NewSchemaCommand builds the `schema` command bound to a registry.
func NewSchemaCommand(reg *Registry, cliVersion, minRancherVersion string, readOnly func() (bool, []string)) Command {
	return &schemaCmd{reg: reg, cliVersion: cliVersion, minRancherVersion: minRancherVersion, readOnly: readOnly}
}

func (s *schemaCmd) Describe() Descriptor {
	return Descriptor{
		Name:  "schema",
		Usage: "Print the capability manifest, optionally filtered to one command",
		Args:  []ArgSpec{{Name: "command", Type: "string"}},
		Flags: []ArgSpec{
			{Name: "kind", Type: "bool",
				Desc: "resolve the selector as a kind, not a command"},
			// The verb enum is a static literal — the descriptor cannot see
			// the registry's full command set at Describe time. The surface
			// test TestSchemaVerbEnumMatchesCommandVerbs pins it to the real
			// registry's distinct first name tokens; a new verb fails there
			// until this list is updated.
			{Name: "verb", Type: "string",
				Enum: []string{"auth", "completion", "config", "create", "delete", "get",
					"grant", "images", "inspect", "kubeconfig", "logs", "patch",
					"project", "revoke", "schema", "skill", "support-bundle", "usage"},
				Desc: "show only commands with this verb (the first word of the command name)"},
			{Name: "destructive", Type: "bool",
				Desc: "show only destructive commands (destructive true or dependsOnInput)"},
			// Tiers PRESENT in the manifest, not every declared safety.Tier —
			// pinned by TestSchemaTierEnumMatchesCommandTiers.
			{Name: "tier", Type: "string",
				Enum: []string{"public", "crd"},
				Desc: "show only commands served at this tier"},
			{Name: "mutating", Type: "bool",
				Desc: "show only mutating commands"},
			{Name: "search", Type: "string",
				Desc: "case-insensitive substring search over command names, aliases, usage, and descriptions"},
		},
		CommandType: ReadOnly,
		Examples:    []string{"rancher schema -o json", "rancher schema get pod -o json"},
		Toolset:     "core",
	}
}

// CompleteArgs implements ShellCompleter: `schema <TAB>` offers every valid
// schema selector, offline from the registry alone (SE-12) — full command
// names; command aliases in their full typed form ("get pods", "get po" —
// never the bare leaf alias, which Execute's selection would reject; the verb
// comes from the command name's first token); and kind names, keys, and
// aliases as bare tokens. The set is deduplicated (a command/kind collision
// like kubeconfig yields one candidate) and deterministically sorted. Every
// candidate resolves through Execute's own selection logic — a command match
// wins a collision, a kind identifier falls through to the kind view.
func (s *schemaCmd) CompleteArgs(_ context.Context, _ ConfigView) []string {
	m := s.reg.Schema(s.cliVersion, s.minRancherVersion)
	seen := map[string]bool{}
	var out []string
	add := func(sel string) {
		if !seen[sel] {
			seen[sel] = true
			out = append(out, sel)
		}
	}
	for _, c := range m.Commands {
		add(c.Name)
		verb, _, verbed := strings.Cut(c.Name, " ")
		if !verbed {
			// A single-token command's bare alias is not a selector: Execute
			// resolves aliases only in the "<verb> <alias>" form (matchesAlias).
			continue
		}
		for _, a := range c.Aliases {
			add(verb + " " + a)
		}
	}
	for _, k := range m.Kinds {
		add(k.Name)
		add(k.Key)
		for _, a := range k.Aliases {
			add(a)
		}
	}
	sort.Strings(out)
	return out
}

func (s *schemaCmd) Execute(ctx context.Context, in Input, _ Prompter) (Result, error) {
	m := s.reg.Schema(s.cliVersion, s.minRancherVersion)
	if s.readOnly != nil {
		m.ReadOnly, m.ReadOnlySources = s.readOnly()
	}
	asKind := in.Bool("kind")
	f := filtersFrom(in)
	if f.active() {
		// SE-20: filters and --search span the whole surface; a positional
		// selector (or --kind, itself a selection modifier) conflicts.
		if len(in.Args()) > 0 || asKind {
			return Result{}, filterSelectorConflict(f, len(in.Args()) > 0)
		}
		fm := filterManifest(m, f)
		return Result{Kind: "Schema", Data: newSchemaView(fm, commandNames(fm)),
			Text: renderFilteredTOC(fm, f)}, nil
	}
	if len(in.Args()) == 0 {
		if asKind {
			return Result{}, clierr.New(clierr.CodeValidation, "--kind requires a <kind> selector").
				WithHint("pass a kind name, e.g. `rancher schema kubeconfig --kind`")
		}
		return Result{Kind: "Schema", Data: newSchemaView(m, commandNames(m)), Text: renderTOC(m)}, nil
	}
	want := strings.Join(in.Args(), " ")
	// Resolution order (SE-5/SE-6): command match wins; a positional that
	// names no command is looked up against Manifest.Kinds; --kind skips the
	// command pass and forces the kind interpretation.
	if !asKind {
		for _, c := range m.Commands {
			if c.Name == want || matchesAlias(c, want) {
				sel := m
				sel.Commands = []SchemaCommand{c}
				sel.Groups = filterGroups(m.Groups, map[string]bool{c.Name: true})
				text := renderCommandCard(sel, c)
				if k, collides := lookupKind(m.Kinds, want); collides {
					text += "kind view: rancher schema " + k.Name + " --kind\n"
				}
				return Result{Kind: "Schema", Data: newSchemaView(sel, []string{c.Name}), Text: text}, nil
			}
		}
	}
	if k, ok := lookupKind(m.Kinds, want); ok {
		return Result{Kind: "Schema", Data: newSchemaView(filterToKind(m, k), []string{k.Name}),
			Text: renderKindCard(k)}, nil
	}
	return Result{}, unknownSelection(m, want, asKind)
}

// schemaFilters holds the five SE-10/SE-11 filter/search flags. All active
// filters must match for a command to be retained (they compose as AND).
type schemaFilters struct {
	verb        string
	tier        string
	search      string
	destructive bool
	mutating    bool
}

func filtersFrom(in Input) schemaFilters {
	return schemaFilters{
		verb:        in.String("verb"),
		tier:        in.String("tier"),
		search:      in.String("search"),
		destructive: in.Bool("destructive"),
		mutating:    in.Bool("mutating"),
	}
}

func (f schemaFilters) active() bool {
	return f.verb != "" || f.tier != "" || f.search != "" || f.destructive || f.mutating
}

// describe names the active flags as typed, for conflict errors and the
// filtered TOC's header/no-matches message.
func (f schemaFilters) describe() []string {
	var out []string
	if f.verb != "" {
		out = append(out, "--verb "+f.verb)
	}
	if f.destructive {
		out = append(out, "--destructive")
	}
	if f.tier != "" {
		out = append(out, "--tier "+f.tier)
	}
	if f.mutating {
		out = append(out, "--mutating")
	}
	if f.search != "" {
		out = append(out, "--search "+strconv.Quote(f.search))
	}
	return out
}

// keep reports whether c survives every active filter: --verb matches the
// first name token, --destructive keeps true and dependsOnInput, --tier the
// resolver tier, --mutating the commandType, --search SE-11's substring set.
func (f schemaFilters) keep(c SchemaCommand) bool {
	if f.verb != "" {
		verb, _, _ := strings.Cut(c.Name, " ")
		if verb != f.verb {
			return false
		}
	}
	if f.destructive && c.Destructive != "true" && c.Destructive != "dependsOnInput" {
		return false
	}
	if f.tier != "" && c.Tier != f.tier {
		return false
	}
	if f.mutating && c.CommandType != Mutating {
		return false
	}
	if f.search != "" && !searchMatches(c, strings.ToLower(f.search)) {
		return false
	}
	return true
}

// searchMatches is SE-11's match set: a case-insensitive substring of the
// command's name or full alias forms (matchesSubstring), usage, or desc.
func searchMatches(c SchemaCommand, q string) bool {
	if matchesSubstring(c, q) {
		return true
	}
	return strings.Contains(strings.ToLower(c.Usage), q) ||
		strings.Contains(strings.ToLower(c.Desc), q)
}

// filterManifest applies f to m's commands and prunes Groups to the retained
// set per SE-18. Kinds are untouched: command filters describe commands, not
// the kind vocabulary. Commands stays a non-nil slice so a zero-hit manifest
// still serializes the valid empty `commands` array (SE-2).
func filterManifest(m Manifest, f schemaFilters) Manifest {
	keep := map[string]bool{}
	cmds := make([]SchemaCommand, 0, len(m.Commands))
	for _, c := range m.Commands {
		if f.keep(c) {
			cmds = append(cmds, c)
			keep[c.Name] = true
		}
	}
	m.Commands = cmds
	m.Groups = filterGroups(m.Groups, keep)
	return m
}

// filterSelectorConflict is SE-20's VALIDATION error: filters/--search span
// the whole surface and cannot combine with a positional selector or with
// --kind (which requires one).
func filterSelectorConflict(f schemaFilters, hasSelector bool) error {
	with := "--kind"
	if hasSelector {
		with = "a schema selector"
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s cannot combine with %s", strings.Join(f.describe(), ", "), with)).
		WithHint("filters and --search search the whole surface; drop the selector or the filter")
}

// lookupKind resolves a positional selector against the manifest's kind rows
// by exact match on the canonical name, the key, or any alias — manifest data
// only, mirroring kinds.Lookup without importing it (SE-5).
func lookupKind(ks []SchemaKind, want string) (SchemaKind, bool) {
	for _, k := range ks {
		if want == k.Name || want == k.Key {
			return k, true
		}
		for _, a := range k.Aliases {
			if want == a {
				return k, true
			}
		}
	}
	return SchemaKind{}, false
}

// filterToKind shapes the manifest for a kind selection (SE-2's kind row):
// kinds holds exactly the selected kind, commands exactly its registered
// generated verbs — available OR refused, both registered per SE-17; absent
// verbs have no command — and Groups pruned to the same set (SE-18).
func filterToKind(m Manifest, k SchemaKind) Manifest {
	keep := map[string]bool{}
	for verb, vs := range k.Verbs {
		if vs.State == VerbAvailable || vs.State == VerbRefused {
			keep[verb+" "+k.Name] = true
		}
	}
	cmds := make([]SchemaCommand, 0, len(keep))
	for _, c := range m.Commands {
		if keep[c.Name] {
			cmds = append(cmds, c)
		}
	}
	m.Kinds = []SchemaKind{k}
	m.Commands = cmds
	m.Groups = filterGroups(m.Groups, keep)
	return m
}

// filterGroups prunes each ordered section to the retained command names and
// drops empty sections, preserving relative order (SE-18's consistency rule:
// the manifest never carries two contradictory command sets).
func filterGroups(gs []SchemaGroup, keep map[string]bool) []SchemaGroup {
	var out []SchemaGroup
	for _, g := range gs {
		var names []string
		for _, n := range g.Commands {
			if keep[n] {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			out = append(out, SchemaGroup{Name: g.Name, Commands: names})
		}
	}
	return out
}

// maxNearestMatches caps SE-4's candidate list: a short query can substring-
// match a large slice of the generated commands, and past a screenful the
// list stops helping.
const maxNearestMatches = 10

// unknownSelection builds SE-4's VALIDATION error for a positional that
// matches nothing, listing nearest matches when any exist. Under --kind
// (kindOnly) the selector could only ever be a kind, so the message and the
// candidate set are restricted to kinds.
func unknownSelection(m Manifest, want string, kindOnly bool) error {
	noun := "command or kind"
	if kindOnly {
		noun = "kind"
	}
	ce := clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown %s %q", noun, want)).
		WithHint("run `rancher schema -o json` for the full command list")
	matches := nearestSelectors(m, want, kindOnly)
	if len(matches) == 0 {
		return ce
	}
	cands := make([]clierr.Candidate, len(matches))
	for i, name := range matches {
		cands[i] = clierr.Candidate{ID: name, Name: name}
	}
	return ce.WithCandidates(cands).
		WithHint(fmt.Sprintf("nearest matches: %s — run `rancher schema -o json` for the full command list",
			strings.Join(matches, ", ")))
}

// nearestSelectors finds SE-4's nearest matches: a case-insensitive substring
// search of want over command names and full alias forms (the "<verb> <alias>"
// shape matchesAlias resolves) plus kind names, keys, and aliases — every
// candidate deduplicated to its selectable name (a command/kind collision
// yields one candidate), sorted, capped. kindOnly restricts the search to
// kinds, matching --kind's forced interpretation.
func nearestSelectors(m Manifest, want string, kindOnly bool) []string {
	q := strings.ToLower(want)
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if !kindOnly {
		for _, c := range m.Commands {
			if matchesSubstring(c, q) {
				add(c.Name)
			}
		}
	}
	for _, k := range m.Kinds {
		if kindMatchesSubstring(k, q) {
			add(k.Name)
		}
	}
	sort.Strings(out)
	if len(out) > maxNearestMatches {
		out = out[:maxNearestMatches]
	}
	return out
}

// kindMatchesSubstring reports whether the lowercased query q occurs in k's
// canonical name, key, or any alias.
func kindMatchesSubstring(k SchemaKind, q string) bool {
	ids := append([]string{k.Name, k.Key}, k.Aliases...)
	for _, id := range ids {
		if strings.Contains(strings.ToLower(id), q) {
			return true
		}
	}
	return false
}

// matchesSubstring reports whether the lowercased query q occurs in c's name
// or in any alias — the full "<verb> <alias>" form when the name is verbed,
// the bare alias otherwise.
func matchesSubstring(c SchemaCommand, q string) bool {
	if strings.Contains(strings.ToLower(c.Name), q) {
		return true
	}
	verb, _, verbed := strings.Cut(c.Name, " ")
	for _, a := range c.Aliases {
		full := a
		if verbed {
			full = verb + " " + a
		}
		if strings.Contains(strings.ToLower(full), q) {
			return true
		}
	}
	return false
}

// matchesAlias reports whether want names c via a kind alias. Aliases on
// generated commands are BARE kind aliases (get.go:48 copies k.Aliases, e.g.
// "pods", "po" — the existing schema.txtar canary pins `aliases.0 == pods`),
// so the alias full name is "<verb> <alias>" with the verb taken from c.Name.
func matchesAlias(c SchemaCommand, want string) bool {
	verb, _, ok := strings.Cut(c.Name, " ")
	if !ok {
		return false
	}
	for _, a := range c.Aliases {
		if verb+" "+a == want {
			return true
		}
	}
	return false
}
