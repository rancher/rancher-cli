package runtime

import (
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// GlobalFlag documents a global flag in the schema manifest.
type GlobalFlag struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Enum      []string `json:"enum,omitempty"`
	AppliesTo string   `json:"appliesTo"`
	Desc      string   `json:"desc,omitempty"`
}

// GlobalFlags is the confirmation/output model, discoverable via schema.
func GlobalFlags() []GlobalFlag {
	return []GlobalFlag{
		{Name: "--dry-run", Type: "bool", AppliesTo: "mutating", Desc: "preview Effect, no change"},
		{Name: "--apply", Type: "bool", AppliesTo: "mutating", Desc: "execute the mutation"},
		{Name: "--yes", Type: "bool", AppliesTo: "mutating", Desc: "confirm a destructive Effect"},
		{Name: "--allow-deprecated", Type: "bool", AppliesTo: "mutating", Desc: "permit a deprecated-tier write"},
		{Name: "--no-input", Type: "bool", AppliesTo: "all", Desc: "never prompt; ambiguity -> error"},
		{Name: "-o/--output", Type: "enum", Enum: []string{"table", "json", "yaml", "name", "template"}, AppliesTo: "all", Desc: "'name' = ids only"},
		{Name: "-c/--config", Type: "string", AppliesTo: "all", Desc: "config dir override"},
		{Name: "--debug", Type: "bool", AppliesTo: "all"},
	}
}

// SchemaCommand is one command entry in the manifest (Descriptor + tier).
type SchemaCommand struct {
	Name        string      `json:"name"`
	Usage       string      `json:"usage,omitempty"`
	Args        []ArgSpec   `json:"args,omitempty"`
	Flags       []ArgSpec   `json:"flags,omitempty"`
	Examples    []string    `json:"examples,omitempty"`
	CommandType CommandType `json:"commandType"`
	Tier        string      `json:"tier,omitempty"`
	Stability   string      `json:"stability,omitempty"`
}

// Manifest is the full `rancher schema` output.
type Manifest struct {
	CLIVersion       string          `json:"cliVersion"`
	OutputAPIVersion string          `json:"outputApiVersion"`
	ErrorCodes       []string        `json:"errorCodes"`
	GlobalFlags      []GlobalFlag    `json:"globalFlags"`
	Commands         []SchemaCommand `json:"commands"`
}

// Resolver maps operation names to their current tier binding. provider
// implements it; the kind table composes into it in a later branch.
type Resolver interface {
	Resolve(op string) (safety.Binding, bool)
	All() map[string]safety.Binding
}

// Registry holds all commands and the tier resolver.
type Registry struct {
	resolver Resolver
	cmds     []Command
}

func NewRegistry(r Resolver) *Registry { return &Registry{resolver: r} }

func (reg *Registry) Register(c Command) { reg.cmds = append(reg.cmds, c) }

func (reg *Registry) Commands() []Command { return reg.cmds }

// Binding returns the op's binding (zero value if unknown; Validate enforces
// that mutating commands have one).
func (reg *Registry) Binding(op string) safety.Binding {
	b, _ := reg.resolver.Resolve(op)
	return b
}

// Validate enforces registry invariants; BuildRoot fails on any violation.
func (reg *Registry) Validate() error {
	seen := map[string]bool{}
	for _, c := range reg.cmds {
		d := c.Describe()
		if seen[d.Name] {
			return fmt.Errorf("duplicate command name %q", d.Name)
		}
		seen[d.Name] = true
		_, isMut := c.(MutatingCommand)
		_, isRead := c.(ReadCommand)
		if !isMut && !isRead {
			return fmt.Errorf("command %q implements neither ReadCommand nor MutatingCommand", d.Name)
		}
		if isMut && isRead {
			return fmt.Errorf("command %q implements both ReadCommand and MutatingCommand", d.Name)
		}
		if isMut != (d.CommandType == Mutating) {
			return fmt.Errorf("command %q commandType %q does not match implemented interface", d.Name, d.CommandType)
		}
		if isMut {
			if _, ok := reg.resolver.Resolve(d.Name); !ok {
				return fmt.Errorf("mutating command %q has no resolver binding", d.Name)
			}
		}
	}
	return nil
}

// Schema assembles the manifest, merging each Descriptor with the resolver tier.
func (reg *Registry) Schema(cliVersion string) Manifest {
	m := Manifest{
		CLIVersion:       cliVersion,
		OutputAPIVersion: output.APIVersion,
		ErrorCodes:       clierr.AllCodes(),
		GlobalFlags:      GlobalFlags(),
	}
	for _, c := range reg.cmds {
		d := c.Describe()
		sc := SchemaCommand{
			Name: d.Name, Usage: d.Usage, Args: d.Args, Flags: d.Flags,
			Examples: d.Examples, CommandType: d.CommandType,
		}
		if b, ok := reg.resolver.Resolve(d.Name); ok {
			sc.Tier = string(b.Tier)
			sc.Stability = string(b.Stability)
		}
		m.Commands = append(m.Commands, sc)
	}
	sort.Slice(m.Commands, func(i, j int) bool { return m.Commands[i].Name < m.Commands[j].Name })
	return m
}
