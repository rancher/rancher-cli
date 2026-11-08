package runtime

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
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
	// or the loaded config - internal/cli hands over the SAME resolution
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
		Usage: "Print the schema manifest, optionally filtered to one command",
		Desc:  "A command name of two words (\"get pod\") can be given unquoted or as one quoted argument. Use --search <text> to search all commands by name, alias, usage, or description.",
		Args: []ArgSpec{
			{Name: "command", Type: "string"},
			{Name: "subcommand", Type: "string", Desc: "the second word of a two-word command name"},
		},
		Flags: []ArgSpec{
			{Name: "all", Type: "bool",
				Desc: "print the complete schema manifest in structured output (table output still shows the table of contents)"},
			{Name: "kind", Type: "bool",
				Desc: "treat the argument as a kind name, not a command name"},
			// The verb enum is a static literal - the descriptor cannot see
			// the registry's full command set at Describe time. The surface
			// test TestSchemaVerbEnumMatchesCommandVerbs asserts it to the real
			// registry's distinct first name tokens; a new verb fails there
			// until this list is updated.
			{Name: "verb", Type: "string",
				Enum: []string{"access", "auth", "charts", "cluster", "completion", "config", "create", "delete", "get",
					"grant", "images", "inspect", "kubeconfig", "logs", "oidc-client", "patch",
					"project", "revoke", "schema", "skill", "support-bundle", "telemetry", "token", "usage"},
				Desc: "show only commands with this verb (the first word of the command name)"},
			{Name: "destructive", Type: "bool",
				Desc: "show only destructive commands, including commands that are destructive only for some inputs (destructive true or dependsOnInput)"},
			// Contracts PRESENT in the manifest, not every declared safety.Contract -
			// asserted by TestSchemaContractEnumMatchesCommandContracts.
			{Name: "contract", Type: "string",
				Enum: []string{"public", "internal"},
				Desc: "show only commands with this contract"},
			{Name: "mutating", Type: "bool",
				Desc: "show only mutating commands"},
			{Name: "search", Type: "string",
				Desc: "case-insensitive substring search over command names, aliases, usage, and descriptions"},
		},
		CommandType:  ReadOnly,
		ServerAccess: ServerAccessNone,
		Examples:     []string{"rancher schema -o json", "rancher schema get pod -o json"},
		Shape:        ShapeText,
	}
}

// CompleteArgs implements ShellCompleter: `schema <TAB>` offers every valid
// schema selector, offline from the registry alone - full command
// names; command aliases in their full typed form ("get pods", "get po" -
// never the bare leaf alias, which Execute's selection would reject; the verb
// comes from the command name's first token); and kind names, keys, and
// aliases as bare tokens. The set is deduplicated (a command/kind collision
// like kubeconfig yields one candidate) and deterministically sorted. Every
// candidate resolves through Execute's own selection logic - a command match
// wins a collision, a kind identifier falls through to the kind view.
// `schema <word> <TAB>` offers the second words of the two-word selectors
// that begin with word, so the two arguments, joined, are one of them.
func (s *schemaCmd) CompleteArgs(_ context.Context, _ ConfigView, args []string) []string {
	switch len(args) {
	case 0:
		return s.selectors()
	case 1:
		var out []string
		for _, sel := range s.selectors() {
			if rest, ok := strings.CutPrefix(sel, args[0]+" "); ok {
				out = append(out, rest)
			}
		}
		return out
	default:
		return nil
	}
}

// selectors returns every schema selector, deduplicated and sorted.
func (s *schemaCmd) selectors() []string {
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
	// Named group selectors - every group name resolves via Execute's third
	// tier; unnamed ("") sections are not selectable.
	for _, g := range m.Groups {
		if g.Name != "" {
			add(g.Name)
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
	// Mutually exclusive selection modes: --all | filter/search | positional
	// selector | --kind. --all is tested first; within its conflict switch
	// asKind precedes the positional check so `schema pod --kind --all` names
	// --kind in the error rather than the selector.
	if in.Bool("all") {
		switch {
		case f.active():
			return Result{}, allConflict(strings.Join(f.describe(), " "))
		case asKind:
			return Result{}, allConflict("--kind")
		case len(in.Args()) > 0:
			return Result{}, allConflict("the argument")
		}
		text := renderTOCText(m, false)
		return Result{Kind: "Schema", Data: newSchemaView(m), Text: text,
			StyledText: func(s style.Style) string { return styleSchemaTOC(text, m, allPrimeGroups(m), s) }}, nil
	}
	if f.active() {
		// Filters and --search span the whole surface; a positional
		// selector (or --kind, itself a selection modifier) conflicts.
		if len(in.Args()) > 0 || asKind {
			return Result{}, filterSelectorConflict(f, len(in.Args()) > 0)
		}
		fm := filterManifest(m, f)
		primeGroups := allPrimeGroups(m)
		text := renderFilteredTOC(fm, f, primeGroups)
		return Result{Kind: "Schema", Data: newTOCView(fm, false), Text: text,
			StyledText: func(s style.Style) string { return styleSchemaTOC(text, fm, primeGroups, s) }}, nil
	}
	if len(in.Args()) == 0 {
		if asKind {
			return Result{}, clierr.New(clierr.CodeValidation, "--kind requires a <kind> argument, e.g. kubeconfig").
				WithHint("rancher schema kubeconfig --kind")
		}
		text := renderTOC(m)
		return Result{Kind: "Schema", Data: newTOCView(m, true), Text: text,
			StyledText: func(s style.Style) string { return styleSchemaTOC(text, m, allPrimeGroups(m), s) }}, nil
	}
	want := strings.Join(in.Args(), " ")
	// Resolution order: command match wins; a positional that
	// names no command is looked up against Manifest.Kinds; --kind skips the
	// command pass and forces the kind interpretation.
	if !asKind {
		for _, c := range m.Commands {
			if c.Name == want || matchesAlias(c, want) {
				text := renderCommandCard(m, c)
				kindHint := ""
				if k, collides := lookupKind(m.Kinds, want); collides {
					line, hint := collisionFooter("to see this name as a kind, run: rancher schema "+k.Name+" --kind", k.Name)
					text += line
					kindHint = hint
				}
				return Result{Kind: "Schema", Data: newCardView(m, c, kindHint), Text: text,
					StyledText: func(s style.Style) string {
						return styleSchemaCommandCard(text, len(c.Flags) > 0, len(c.Examples) > 0, s)
					}}, nil
			}
		}
	}
	// Resolution order: groups sit in the
	// COMMAND tier - command, then group, then kind - because --kind exists
	// to reach a shadowed kind, while nothing could reach a group shadowed
	// by a kind (the "project" collision). Named groups only: the unnamed
	// ("") sections are positional bookmarks, not selectable. The group
	// tier is gated under !asKind: with --kind set the selector must name
	// a kind; falling through to a group would contradict "resolve the
	// selector as a kind, not a command".
	if !asKind {
		if g, ok := lookupGroup(m.Groups, want); ok {
			gv := newGroupView(m, g)
			text := renderGroupCard(m, gv)
			if k, collides := lookupKind(m.Kinds, want); collides {
				line, hint := collisionFooter("to see this name as a kind, run: rancher schema "+k.Name+" --kind", k.Name)
				text += line
				gv.kindHint = hint
			}
			return Result{Kind: "Schema", Data: gv, Text: text,
				StyledText: func(s style.Style) string {
					return styleSchemaGroupCard(text, len(gv.bespoke) > 0 || gv.generatedCount > 0, gv.prime, gv.bespoke, s)
				}}, nil
		}
	}
	if k, ok := lookupKind(m.Kinds, want); ok {
		kv := newKindView(m, k)
		text := renderKindCard(m, k)
		// The reverse hint: a kind reached while a same-name group exists
		// names the other view, mirroring the command card's kind-view
		// hint. The hint carries the GROUP's own name (g.Name), never the
		// kind's canonical Name - a kind reached via an alias that equals
		// a group name must not hint a selector that resolves elsewhere.
		if g, collides := lookupGroup(m.Groups, want); collides {
			line, hint := collisionFooter("to see this name as a command group, run: rancher schema "+g.Name, g.Name)
			text += line
			kv.groupHint = hint
		}
		return Result{Kind: "Schema", Data: kv, Text: text}, nil
	}
	return Result{}, unknownSelection(m, want, asKind)
}

// schemaFilters holds the five filter/search flags. All active
// filters must match for a command to be retained (they compose as AND).
type schemaFilters struct {
	verb        string
	contract    string
	search      string
	destructive bool
	mutating    bool
}

func filtersFrom(in Input) schemaFilters {
	return schemaFilters{
		verb:        in.String("verb"),
		contract:    in.String("contract"),
		search:      in.String("search"),
		destructive: in.Bool("destructive"),
		mutating:    in.Bool("mutating"),
	}
}

func (f schemaFilters) active() bool {
	return f.verb != "" || f.contract != "" || f.search != "" || f.destructive || f.mutating
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
	if f.contract != "" {
		out = append(out, "--contract "+f.contract)
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
// first name token, --destructive keeps true and dependsOnInput, --contract the
// resolver contract, --mutating the commandType, --search the substring set.
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
	if f.contract != "" && c.Contract != f.contract {
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

// searchMatches is the --search match set: a case-insensitive substring of the
// command's name or full alias forms (matchesSubstring), usage, or desc.
func searchMatches(c SchemaCommand, q string) bool {
	if matchesSubstring(c, q) {
		return true
	}
	return strings.Contains(strings.ToLower(c.Usage), q) ||
		strings.Contains(strings.ToLower(c.Desc), q)
}

// filterManifest applies f to m's commands and prunes Groups to the retained
// set. Kinds are untouched: command filters describe commands, not
// the kind vocabulary. Commands stays a non-nil slice so a zero-hit manifest
// still serializes the valid empty `commands` array.
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

// filterSelectorConflict is the VALIDATION error: filters/--search span
// the whole surface and cannot combine with a positional selector or with
// --kind (which requires one).
func filterSelectorConflict(f schemaFilters, hasSelector bool) error {
	with, remove := "--kind", "--kind"
	if hasSelector {
		with, remove = "an argument", "the argument"
	}
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("%s cannot combine with %s; filters and --search search all commands; remove %s or the filter",
			strings.Join(f.describe(), ", "), with, remove))
}

// allConflict constructs the --all selection-mode conflict error. mode names
// what the caller also passed: a filter description (from schemaFilters.describe()),
// "--kind", or "the argument". The four mutually exclusive modes are enumerated
// at the --all switch site in Execute.
func allConflict(mode string) error {
	return clierr.New(clierr.CodeValidation,
		"--all prints the unfiltered manifest; remove "+mode+" or remove --all")
}

// lookupKind resolves a positional selector against the manifest's kind rows
// by exact match on the canonical name, the key, or any alias - manifest data
// only, mirroring kinds.Lookup without importing it.
func lookupKind(ks []SchemaKind, want string) (SchemaKind, bool) {
	for _, k := range ks {
		if want == k.Name || want == k.Key {
			return k, true
		}
		if slices.Contains(k.Aliases, want) {
			return k, true
		}
	}
	return SchemaKind{}, false
}

// filterGroups prunes each ordered section to the retained command names and
// drops empty sections, preserving relative order (the consistency rule:
// the manifest never carries two contradictory command sets). Desc is
// intentionally omitted from filtered sections - a filtered section covers
// only the retained commands, not the full set the group desc was written for.
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

// collisionFooter builds a collision card's footer line and its JSON hint
// value in one place, so a card's rendered pointer and machine-readable hint
// can never drift apart (they once did: a kind card hinted the kind's
// canonical name where only the group's own name resolves).
func collisionFooter(line, hint string) (string, string) {
	return line + "\n", hint
}

// lookupGroup resolves a positional selector against the manifest's named
// group sections. Unnamed ("") sections are not selectable - they are
// positional bookmarks, not identifiers.
func lookupGroup(gs []SchemaGroup, want string) (SchemaGroup, bool) {
	if want == "" {
		return SchemaGroup{}, false
	}
	for _, g := range gs {
		if g.Name == want {
			return g, true
		}
	}
	return SchemaGroup{}, false
}

// maxNearestMatches is how many of the nearest matches the error lists: a
// short query can substring-match a large slice of the generated commands,
// and past a screenful the list stops helping. A longer list is cut to its
// first entries and the error records the full size.
const maxNearestMatches = 10

// commandListCmd and kindsArrayCmd are the two paste-valid pointers to the
// full surface: -o name for the command list, -o json for the kind names.
// unknownSelection names both in its message when the miss is genuinely
// ambiguous (no single command covers both), and carries the one that
// applies as the Hint when the tier settles on a single interpretation.
const commandListCmd = "rancher schema -o name"
const kindsArrayCmd = "rancher schema -o json"

// unknownSelection builds the VALIDATION error for a positional that
// matches nothing, listing nearest matches when any exist. Under --kind
// (kindOnly) the selector could only ever be a kind, so the message and the
// candidate set are restricted to kinds.
func unknownSelection(m Manifest, want string, kindOnly bool) error {
	// Three ambiguity tiers: --kind fixes the kind interpretation; a
	// multi-token selector is command-shaped (kinds are single tokens,
	// always) so the kind half of the dual guidance is noise; only a bare
	// single token is genuinely either. The dual tier names both commands in
	// the message - an "X or Y" pair is never a hint - while the other two
	// tiers name one command, which the Hint carries.
	commandShaped := len(strings.Fields(want)) > 1
	var noun, guidance, hint string
	switch {
	case kindOnly:
		noun = "kind"
		guidance = kindsArrayCmd + " lists every kind name"
		hint = kindsArrayCmd
	case commandShaped:
		noun = "command"
		guidance = "see the full command list: " + commandListCmd
		hint = commandListCmd
	default:
		noun = "command or kind"
		guidance = "see the full command list (" + commandListCmd + ") or the kind names (" + kindsArrayCmd + ")"
	}
	ce := clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown %s %q; %s", noun, want, guidance))
	if hint != "" {
		ce = ce.WithHint(hint)
	}
	matches := nearestSelectors(m, want, kindOnly, commandShaped)
	if len(matches) == 0 {
		return ce
	}
	cands := make([]clierr.Candidate, len(matches))
	for i, name := range matches {
		cands[i] = clierr.Candidate{ID: name, Name: name}
	}
	return ce.WithCandidatesCut(cands, maxNearestMatches)
}

// nearestSelectors finds every nearest match: a case-insensitive substring
// search of want over command names and full alias forms (the "<verb> <alias>"
// shape matchesAlias resolves) plus kind names, keys, and aliases - every
// candidate deduplicated to its selectable name (a command/kind collision
// yields one candidate), sorted. kindOnly restricts the search to kinds,
// matching --kind's forced interpretation. commandOnly skips the kind scan
// for multi-token (command-shaped) queries - kinds are always single
// tokens, so the kind half of the search is noise for a query like "get pdo".
func nearestSelectors(m Manifest, want string, kindOnly, commandOnly bool) []string {
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
	if !commandOnly {
		for _, k := range m.Kinds {
			if kindMatchesSubstring(k, q) {
				add(k.Name)
			}
		}
	}
	// Named group selectors are single-token (like kinds) and now directly
	// resolvable. Include them when neither --kind (groups are not kinds) nor
	// commandOnly (multi-token query cannot substring-match a group name in a
	// useful way) is active - consistent with the group-tier guard in Execute.
	if !kindOnly && !commandOnly {
		for _, g := range m.Groups {
			if g.Name != "" && strings.Contains(strings.ToLower(g.Name), q) {
				add(g.Name)
			}
		}
	}
	sort.Strings(out)
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
// or in any alias - the full "<verb> <alias>" form when the name is verbed,
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

// CommandAliasNames returns every full alias name c answers to: aliases on
// generated commands are BARE kind aliases (get.go:48 copies k.Aliases, e.g.
// "pods", "po" - the existing schema.txtar canary asserts `aliases.0 ==
// pods`), so a full alias name is "<verb> <alias>" with the verb taken from
// c.Name. nil for an unverbed c.Name (a bare fixed command carries no
// resolvable alias form) or a command with no aliases. Exported so a second
// consumer of this exact resolution rule (internal/cli's e2e coverage
// conformance test) never re-derives it inline, where the two could drift
// apart silently.
func CommandAliasNames(c SchemaCommand) []string {
	verb, _, verbed := strings.Cut(c.Name, " ")
	if !verbed || len(c.Aliases) == 0 {
		return nil
	}
	names := make([]string, len(c.Aliases))
	for i, a := range c.Aliases {
		names[i] = verb + " " + a
	}
	return names
}

// matchesAlias reports whether want names c via a kind alias.
func matchesAlias(c SchemaCommand, want string) bool {
	return slices.Contains(CommandAliasNames(c), want)
}
