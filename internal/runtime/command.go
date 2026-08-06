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
	Desc     string   `json:"desc,omitempty"`    // schema-manifest documentation; optional
	Env      []string `json:"env,omitempty"`     // env vars that back this flag, if any
}

// Descriptor is a command's self-description (no contract/stability — those come
// from the resolver at schema-generation time). Args are positional; Flags are
// named `--flags`. Both feed the schema manifest, and Flags are wired into the
// urfave command by the surface adapter.
type Descriptor struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"` // leaf aliases (plural/short forms)
	Usage   string   `json:"usage,omitempty"`
	// Desc is optional long-form documentation for the schema manifest only. It
	// is not shown in TTY help (which uses the terse Usage line), so it can
	// carry caveats a one-line usage cannot without bloating help.
	Desc        string      `json:"desc,omitempty"`
	Args        []ArgSpec   `json:"args,omitempty"`
	Flags       []ArgSpec   `json:"flags,omitempty"`
	Examples    []string    `json:"examples,omitempty"`
	CommandType CommandType `json:"commandType"`
	Destructive string      `json:"destructive,omitempty"` // "true"|"false"|"dependsOnInput"; empty for readOnly
	Toolset     string      `json:"toolset,omitempty"`     // MCP toolset filter ("core" today)
	Network     bool        `json:"network"`               // performs network I/O
	Auth        bool        `json:"auth"`                  // requires credentials
	// ProjectScope reports whether --project applies to this command (PC-3):
	// true for generated per-kind commands whose kind is
	// kinds.ProjectScopable; false (the zero value) for every fixed command.
	ProjectScope bool `json:"projectScope,omitempty"`
	// SuccessExitCodes declares every exit status a SUCCESSFUL result may
	// carry. Empty means {0}. When declared it must contain 0 exactly once;
	// additional values must be positive, unique, and disjoint from the
	// error exits in clierr.CodeExits() — Registry.Validate enforces the
	// declaration and finishResult rejects an undeclared returned status.
	SuccessExitCodes []int
}

// Input abstracts parsed flags/args so engine code never imports urfave/cli.
type Input interface {
	String(name string) string
	Bool(name string) bool
	// Strings returns every occurrence of a stringSlice flag, ordered, one
	// entry per occurrence — never comma-split (MC-6a).
	Strings(name string) []string
	// IsSet reports whether the named flag was explicitly provided on the
	// command line — the tri-state seam for boolean flags (MC-5a: an
	// omitted bool is distinguishable from an explicit false).
	IsSet(name string) bool
	Args() []string
}

// Result is a command's success payload, rendered into the output envelope.
// Raw=true tells the pipeline to emit Data as bare JSON with NO envelope — used
// only by the `auth get-token` exec-credential plugin, whose stdout kubectl parses as a
// single ExecCredential object.
type Result struct {
	Kind string
	Data any
	// Warnings render to stderr as "# warning: <w>" lines under format ""/
	// "table" for BOTH paths renderResult supports (the Text branch and the
	// envelope/Data branch), and also under name/template — their stdout is
	// raw (no envelope), so the stderr mirror is the only delivery path.
	// json/yaml carry Warnings inside the envelope; no stderr write, no
	// double print.
	Warnings []string
	Raw      bool
	// RawBytes, when non-nil, is written to stdout verbatim with NO envelope
	// and no encoding — used by `kubeconfig`, whose stdout must be a bare
	// YAML document. Mutually exclusive with Raw; renderResult rejects a
	// Result that sets both.
	RawBytes []byte
	// Text, when non-empty, is written verbatim to stdout under format ""/
	// "table" (with Warnings rendered to stderr per the field's doc above);
	// json/yaml/template/name render the envelope over Data as usual.
	// Mutually exclusive with RawBytes; renderResult rejects a Result that
	// sets both.
	Text string
	// Summary, when non-empty, lands in the -o json/-o yaml envelope's summary
	// field and, under format ""/table, prints as its own stdout line ahead of
	// the rendered Data. One line, no trailing newline needed.
	// -o name and -o template render env.Data alone and never see Summary.
	// Incompatible with Text, Raw, and RawBytes: setting Summary alongside any
	// of them is a command bug and renderResult rejects it.
	Summary string
	// ExitStatus is the process exit status for this SUCCESSFUL result.
	// Zero for every command that has not declared SuccessExitCodes. The
	// pipeline renders the result fully BEFORE the status is honored.
	ExitStatus int
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

// ConfigView is a minimal read-only snapshot of the loaded config, passed
// into CompleteArgs so completers never re-read the config file (AC-19).
// surface/cli implements this over the single Before-loaded *config.Config.
type ConfigView interface {
	ContextNames() []string
	ServerNames() []string
	ConfigDir() string
}

// ShellCompleter is an optional interface a Command implements to supply
// config-local shell completion candidates (e.g. context names, server names,
// fixed shell lists). Installed at adapter-wiring time; runs synchronously
// inside a shell TAB keystroke. view is an immutable snapshot of the config
// loaded by the root Before hook (one load total per TAB invocation, AC-19).
// CompleteArgs must never prompt, write, or touch the network.
type ShellCompleter interface {
	CompleteArgs(ctx context.Context, view ConfigView) []string
}
