package runtime

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
)

// CommandType selects the pipeline path. Exactly one per command.
type CommandType string

const (
	ReadOnly CommandType = "readOnly" // reads server state; no Plan/gates
	Mutating CommandType = "mutating" // writes server state; Plan/apply/gates
	Local    CommandType = "local"    // local config/creds; may call server; no Plan/gates
)

// ArgSpec describes a positional/named argument for the schema manifest.
type ArgSpec struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Enum     []string `json:"enum,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// Descriptor is a command's self-description (no tier/stability — those come
// from the resolver at schema-generation time). Args are positional; Flags are
// named `--flags`. Both feed the schema manifest, and Flags are wired into the
// urfave command by the surface adapter.
type Descriptor struct {
	Name        string      `json:"name"`
	Usage       string      `json:"usage,omitempty"`
	Args        []ArgSpec   `json:"args,omitempty"`
	Flags       []ArgSpec   `json:"flags,omitempty"`
	Examples    []string    `json:"examples,omitempty"`
	CommandType CommandType `json:"commandType"`
}

// Input abstracts parsed flags/args so engine code never imports urfave/cli.
type Input interface {
	String(name string) string
	Bool(name string) bool
	Args() []string
}

// Result is a command's success payload, rendered into the output envelope.
// Raw=true tells the pipeline to emit Data as bare JSON with NO envelope — used
// only by the `auth get-token` exec-credential plugin, whose stdout kubectl parses as a
// single ExecCredential object.
type Result struct {
	Kind      string
	Data      any
	Warnings  []string
	Raw       bool
	Tier      string
	Stability string
}

// Command is the A+B contract every command implements.
type Command interface {
	Describe() Descriptor
	Plan(ctx context.Context, in Input) (safety.Effect, error)
	Execute(ctx context.Context, in Input, eff safety.Effect, p *ui.Prompter) (Result, error)
}
