package runtime

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
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
	Aliases  []string `json:"aliases,omitempty"` // short flag forms (e.g. "f" for --filename)
}

// Descriptor is a command's self-description (no tier/stability — those come
// from the resolver at schema-generation time). Args are positional; Flags are
// named `--flags`. Both feed the schema manifest, and Flags are wired into the
// urfave command by the surface adapter.
type Descriptor struct {
	Name        string      `json:"name"`
	Aliases     []string    `json:"aliases,omitempty"` // leaf aliases (plural/short forms)
	Usage       string      `json:"usage,omitempty"`
	Args        []ArgSpec   `json:"args,omitempty"`
	Flags       []ArgSpec   `json:"flags,omitempty"`
	Examples    []string    `json:"examples,omitempty"`
	CommandType CommandType `json:"commandType"`
	Destructive string      `json:"destructive,omitempty"` // "true"|"false"|"dependsOnInput"; empty for readOnly
	Toolset     string      `json:"toolset,omitempty"`     // MCP toolset filter ("core" today)
	Network     bool        `json:"network"`               // performs network I/O
	Auth        bool        `json:"auth"`                  // requires credentials
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
	Kind     string
	Data     any
	Warnings []string
	Raw      bool
	// RawBytes, when non-nil, is written to stdout verbatim with NO envelope
	// and no encoding — used by `kubeconfig`, whose stdout must be a bare
	// YAML document. Mutually exclusive with Raw; renderResult rejects a
	// Result that sets both.
	RawBytes []byte
	// Text, when non-empty, is written verbatim to stdout under format ""/
	// "table" (with Warnings rendered to stderr as "# warning: <w>" lines);
	// json/yaml/template/name render the envelope over Data as usual.
	// Mutually exclusive with RawBytes; renderResult rejects a Result that
	// sets both.
	Text      string
	Tier      string
	Stability string
}

// Command is the minimal self-describing contract. Concrete commands
// implement exactly one of ReadCommand or MutatingCommand.
type Command interface {
	Describe() Descriptor
}

// ReadCommand executes without a Plan/gate phase (CommandType readOnly/local).
type ReadCommand interface {
	Command
	Execute(ctx context.Context, in Input, p Prompter) (Result, error)
}

// ExecuteFunc performs the mutation a Plan previewed. It closes over the
// planned object, resourceVersion, and client — executing anything other
// than what was planned is unrepresentable.
type ExecuteFunc func(ctx context.Context) (Result, error)

// MutatingCommand plans a mutation and returns the closure that performs it
// (CommandType mutating). No Prompter: mutation gates are flag-only.
type MutatingCommand interface {
	Command
	Plan(ctx context.Context, in Input) (safety.Effect, ExecuteFunc, error)
}
