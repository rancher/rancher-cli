package runtime

import (
	"context"
	"fmt"
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
		Usage:       "Print the machine-readable capability manifest, optionally filtered to one command",
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
			return Result{Kind: "Schema", Data: m}, nil
		}
	}
	return Result{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("unknown command %q", want)).
		WithHint("run `rancher schema -o json` for the full command list")
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
