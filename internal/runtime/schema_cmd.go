package runtime

import (
	"context"
	"fmt"
	"sort"
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
		Name:        "schema",
		Usage:       "Print the capability manifest, optionally filtered to one command",
		Args:        []ArgSpec{{Name: "command", Type: "string"}},
		CommandType: ReadOnly,
		Examples:    []string{"rancher schema -o json", "rancher schema get pod -o json"},
		Toolset:     "core",
	}
}

func (s *schemaCmd) Execute(ctx context.Context, in Input, _ Prompter) (Result, error) {
	m := s.reg.Schema(s.cliVersion, s.minRancherVersion)
	if s.readOnly != nil {
		m.ReadOnly, m.ReadOnlySources = s.readOnly()
	}
	if len(in.Args()) == 0 {
		return Result{Kind: "Schema", Data: m}, nil
	}
	want := strings.Join(in.Args(), " ")
	for _, c := range m.Commands {
		if c.Name == want || matchesAlias(c, want) {
			m.Commands = []SchemaCommand{c}
			m.Groups = filterGroups(m.Groups, map[string]bool{c.Name: true})
			return Result{Kind: "Schema", Data: m, Text: renderCommandCard(m, c)}, nil
		}
	}
	// Kind selection (SE-5) resolves here in a later slice: a positional that
	// names no command is looked up against Manifest.Kinds before falling
	// through to the unknown-selection error.
	return Result{}, unknownSelection(m.Commands, want)
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
// matches nothing, listing nearest matches when any exist.
func unknownSelection(cmds []SchemaCommand, want string) error {
	ce := clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown command %q", want)).
		WithHint("run `rancher schema -o json` for the full command list")
	matches := nearestCommands(cmds, want)
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

// nearestCommands finds SE-4's nearest matches: a case-insensitive substring
// search of want over command names and full alias forms (the "<verb> <alias>"
// shape matchesAlias resolves). Deduplicated to command names, sorted, capped.
func nearestCommands(cmds []SchemaCommand, want string) []string {
	q := strings.ToLower(want)
	var out []string
	for _, c := range cmds {
		if matchesSubstring(c, q) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	if len(out) > maxNearestMatches {
		out = out[:maxNearestMatches]
	}
	return out
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
