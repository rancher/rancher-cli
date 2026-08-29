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

// CapabilityTier is the enum of gated capability tiers a command or kind may
// require. One value exists today; a future tier is added here, never as a
// second boolean. The enum is an extension POINT, not a claim of shared
// machinery: today's detector, outcomes, and error shape are deliberately
// Prime-specific, and a second tier brings its own decide function, outcome,
// and pipeline branch — the type exists so that addition is type-checked.
type CapabilityTier string

// CapabilityPrime is the only capability tier today: the command requires a
// Rancher Prime-licensed server.
const CapabilityPrime CapabilityTier = "prime"

// CapabilityRequirement is the single structured value a Prime-backed kind
// row or a bespoke command's Descriptor declares once. Every generated verb
// derives it from the kind row; the manifest's prime marker, help wording,
// and COMMANDS.md label are all projections of this value, never separately
// authored booleans. The zero value means no requirement.
type CapabilityRequirement struct {
	Tier CapabilityTier
	// Feature is the detector key: the v3 Feature resource name the runtime
	// checker probes. Required whenever Tier is set.
	Feature string
}

// IsSet reports whether a capability requirement is declared at all.
func (r CapabilityRequirement) IsSet() bool { return r.Tier != "" }

// CapabilityResolver is implemented by a command whose capability
// requirement depends on the invocation input (create resource -f: the
// manifest names the kind). Run calls it after validateInput and before
// Plan/Execute, only when the static descriptor carries no requirement. A
// zero return means no requirement applies to this invocation.
//
// Unrelated to Resolver, the existing op→binding resolver; prose says
// "capability resolver" for this interface to keep the two apart.
type CapabilityResolver interface {
	ResolveCapability(ctx context.Context, in Input) (CapabilityRequirement, error)
}

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

// OutputShape is a command's declared success-result shape: the closed-world
// contract every registered command commits to for its default-format
// rendering. ShapeUnspecified is the zero value and is never valid on a
// registered descriptor — an omitted field does not silently become
// Tabular; Registry.Validate rejects it at wiring time.
type OutputShape int

const (
	ShapeUnspecified OutputShape = iota // zero value — invalid, never valid on a registered descriptor
	ShapeTabular                        // Data implements output.Tabular
	ShapeText                           // Result.Text is set (Data may still accompany it for -o json)
	ShapeBytes                          // Result.RawBytes (kubeconfig, completion)
	ShapeExecJSON                       // Result.Raw (exec-credential only)
)

// String renders the shape name for diagnostics and the schema manifest.
func (s OutputShape) String() string {
	switch s {
	case ShapeTabular:
		return "tabular"
	case ShapeText:
		return "text"
	case ShapeBytes:
		return "bytes"
	case ShapeExecJSON:
		return "execJSON"
	default:
		return "unspecified"
	}
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
	// Capability is the command's capability requirement, if any (the zero
	// value means none). Generated verbs copy this from their kind row; a
	// bespoke command declares it here directly. Registry.Validate rejects a
	// set Tier with no detector Feature key and an unknown Tier value.
	Capability CapabilityRequirement
	// SuccessExitCodes declares every exit status a SUCCESSFUL result may
	// carry. Empty means {0}. When declared it must contain 0 exactly once;
	// additional values must be positive, unique, and disjoint from the
	// error exits in clierr.CodeExits() — Registry.Validate enforces the
	// declaration and finishResult rejects an undeclared returned status.
	SuccessExitCodes []int
	// OutputPolicy restricts which -o/--output formats this command accepts
	// (the zero value accepts every format in OutputFormats, today's
	// behavior). Run enforces it immediately after global format-syntax
	// validation and before capability resolution or any network work, so a
	// rejected format makes ZERO total requests. Registry.Validate rejects
	// an AllowedFormats entry outside OutputFormats/"template".
	OutputPolicy OutputPolicy
	// Delivery classifies a command whose successful Result carries a value
	// shown exactly once (DeliveryOnceOnly; the zero value is ordinary).
	// Registry.Validate permits it only when CommandType is Mutating. Run
	// selects BUFFERED rendering for it and requires a non-nil
	// Result.CommittedContext after a successful executor return — its
	// absence is a command-bug error, never a silent skip. OutputPolicy is
	// independent: a DeliveryOnceOnly command may still accept every
	// format (create kubeconfig's RawBytes payload is format-independent).
	Delivery Delivery
	// Shape declares the command's success Result shape (the closed-world
	// conformance contract): ShapeTabular, ShapeText, ShapeBytes, or
	// ShapeExecJSON. Registry.Validate rejects the zero value
	// (ShapeUnspecified) on every registered descriptor, and renderResult
	// verifies the actual Result against it at run time.
	Shape OutputShape
}

// Delivery classifies how a command's successful Result must be delivered.
type Delivery string

// DeliveryOnceOnly marks a command whose Result carries a value the caller
// can never retrieve again (a minted secret, a bearer token): finishResult
// buffers its render and requires committed-mutation context before any
// stdout write. The zero value (ordinary delivery) needs no declaration.
const DeliveryOnceOnly Delivery = "onceOnly"

// CommittedContext names the resource a DeliveryOnceOnly command's mutation
// committed on the server, and the non-mutating recovery a caller should
// take if the render that follows fails. Required on every successful
// executor return when the Descriptor's Delivery is DeliveryOnceOnly.
type CommittedContext struct {
	ResourceKind string
	ResourceName string
	// SecretName names the specific secret/key affected, when applicable
	// (empty for commands with no secret-name concept, e.g. create token).
	SecretName string
	// Recovery is the non-mutating action a caller should take after a
	// post-commit failure — never a suggestion to retry the mutation.
	Recovery string
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
	// CommittedContext is REQUIRED when the Descriptor's Delivery is
	// DeliveryOnceOnly: it names the resource this Result's mutation
	// committed on the server, so a render failure after this point can
	// still tell the caller what happened and how to recover without
	// retrying the mutation. Ignored for ordinary (non-DeliveryOnceOnly)
	// commands.
	CommittedContext *CommittedContext
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
