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
	Env       []string `json:"env,omitempty"`
}

// OutputFormats is the canonical set of --output / -o literal values;
// template=<go-template> is additionally accepted. Referenced by GlobalFlags
// (the schema enum) and, via OutputFormatCompletions, by globalArgSpecs in
// the surface layer so there is one source of truth.
var OutputFormats = []string{"table", "json", "yaml", "name"}

// OutputFormatCompletions is what -o <TAB> suggests: the literal formats plus
// the template form — the bare prefix for discoverability and a seed template
// that runs as-is on any command (renders the whole data view) for the user
// to build on. The template entries are deliberately absent from the schema
// enum, which stays the closed literal set.
var OutputFormatCompletions = append(append([]string{}, OutputFormats...),
	output.TemplatePrefix, output.TemplatePrefix+"{{.}}")

// ShellCompletionShells is the single shared list of shells `completion
// <shell>` emits scripts for — used both to populate the schema manifest's
// shellCompletion field and by the completion command itself (its Enum,
// unknown-shell error, and its own --generate-shell-completion), so the list
// is never duplicated.
func ShellCompletionShells() []string {
	return []string{"bash", "zsh", "fish", "pwsh"}
}

// GlobalFlags is the confirmation/output model, discoverable via schema.
func GlobalFlags() []GlobalFlag {
	return []GlobalFlag{
		{Name: "--dry-run", Type: "bool", AppliesTo: "mutating", Desc: "preview Effect, no change"},
		{Name: "--apply", Type: "bool", AppliesTo: "mutating", Desc: "execute the mutation"},
		{Name: "--yes", Type: "bool", AppliesTo: "mutating", Desc: "confirm a destructive Effect"},
		{Name: "--allow-deprecated", Type: "bool", AppliesTo: "mutating", Desc: "permit a deprecated-tier write"},
		{Name: "--no-input", Type: "bool", AppliesTo: "all", Desc: "never prompt; ambiguity becomes an error"},
		{Name: "-o/--output", Type: "enum", Enum: OutputFormats, AppliesTo: "all", Desc: "'name' = ids only; template=<go-template> renders a Go template over the JSON view of data"},
		{Name: "-c/--config", Type: "string", AppliesTo: "all", Desc: "override the config directory", Env: []string{"RANCHER_CLI_CONFIG_DIR"}},
		{Name: "--debug", Type: "bool", AppliesTo: "all", Desc: "debug logging"},
		{Name: "--context", Type: "string", AppliesTo: "all", Desc: "scope: cluster[:project]; the :project scopes namespaced reads on downstream clusters (--project overrides it); a trailing colon (cluster:) clears it; precedence: flag, then RANCHER_CLI_CONTEXT, then config", Env: []string{"RANCHER_CLI_CONTEXT"}},
		{Name: "--project", Type: "string", AppliesTo: "all", Desc: "scope operations to project namespaces (downstream namespaced resources only); flag overrides context; rejected with an error where it does not apply"},
		{Name: "--read-only", Type: "bool", AppliesTo: "mutating", Desc: "refuse mutating commands for this session; one-way, no override (also: config readOnly, a non-empty RANCHER_CLI_READONLY)", Env: []string{"RANCHER_CLI_READONLY"}},
	}
}

// EnvVar documents an environment variable the CLI reads. Flag names the flag
// it backs, if any; env-only variables leave it empty.
type EnvVar struct {
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
	Flag string `json:"flag,omitempty"`
}

// EnvVars is the complete list of environment variables the CLI consumes,
// discoverable via schema. The names are literals here because internal/runtime
// must not import internal/config, where the consts live (see Schema); keep
// this in sync with internal/config/env.go and the raw reads in the token and
// editor code.
func EnvVars() []EnvVar {
	return []EnvVar{
		{Name: "RANCHER_CLI_CONFIG_DIR", Flag: "-c/--config", Desc: "config directory"},
		{Name: "RANCHER_CLI_URL", Flag: "--server", Desc: "Rancher server URL, used when --server is omitted"},
		{Name: "RANCHER_CLI_TOKEN", Desc: "API token (id:secret) for non-interactive auth"},
		{Name: "RANCHER_CLI_CACERT", Flag: "--cacert", Desc: "PEM CA bundle, a path or the literal PEM"},
		{Name: "RANCHER_CLI_CONTEXT", Flag: "--context", Desc: "default context (cluster[:project])"},
		{Name: "RANCHER_CLI_CREDENTIAL_STORE", Desc: "credential store backend: auto, keychain, or file"},
		{Name: "RANCHER_CLI_READONLY", Flag: "--read-only", Desc: "when non-empty, refuse commands that make changes"},
		{Name: "RANCHER_CLI_OAUTH_AUTH_FLOW", Flag: "--auth-flow", Desc: "OAuth flow: devicecode or authcode"},
		{Name: "RANCHER_CLI_OAUTH_CALLBACK_PORT", Desc: "local callback port for the authcode OAuth flow"},
		{Name: "XDG_CONFIG_HOME", Desc: "base directory for the default config location"},
		{Name: "VISUAL", Desc: "preferred editor for config edit"},
		{Name: "EDITOR", Desc: "fallback editor for config edit"},
	}
}

// SchemaCommand is one command entry in the manifest (Descriptor + tier).
type SchemaCommand struct {
	Name         string      `json:"name"`
	Aliases      []string    `json:"aliases,omitempty"`
	Usage        string      `json:"usage,omitempty"`
	Desc         string      `json:"desc,omitempty"`
	Args         []ArgSpec   `json:"args,omitempty"`
	Flags        []ArgSpec   `json:"flags,omitempty"`
	Examples     []string    `json:"examples,omitempty"`
	CommandType  CommandType `json:"commandType"`
	Destructive  string      `json:"destructive,omitempty"`
	Toolset      string      `json:"toolset,omitempty"`
	Network      bool        `json:"network"`
	Auth         bool        `json:"auth"`
	Tier         string      `json:"tier,omitempty"`
	Stability    string      `json:"stability,omitempty"`
	ProjectScope bool        `json:"projectScope,omitempty"`
}

// EffectField is one field of the Effect preview (name + JSON type).
type EffectField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// EffectSchema describes the --dry-run Effect payload so agents can parse it
// from the manifest alone: the closed Action enum and the Effect field set.
type EffectSchema struct {
	Actions []string      `json:"actions"`
	Fields  []EffectField `json:"fields"`
}

// effectSchema is the static Effect contract. Actions are built from the safety
// enum so a new Action cannot be silently omitted; fields mirror safety.Effect.
func effectSchema() EffectSchema {
	actions := []safety.Action{safety.ActionCreate, safety.ActionUpdate, safety.ActionDelete, safety.ActionScale}
	strs := make([]string, len(actions))
	for i, a := range actions {
		strs[i] = string(a)
	}
	return EffectSchema{
		Actions: strs,
		Fields: []EffectField{
			{Name: "action", Type: "string"},
			{Name: "target", Type: "string"},
			{Name: "server", Type: "string"},
			{Name: "cluster", Type: "string"},
			{Name: "diff", Type: "array"},
			{Name: "resourceVersion", Type: "string"},
			{Name: "conflictDetection", Type: "bool"},
			{Name: "reversible", Type: "bool"},
			{Name: "destructive", Type: "bool"},
			{Name: "tier", Type: "string"},
			{Name: "stability", Type: "string"},
		},
	}
}

// Manifest is the full `rancher schema` output.
type Manifest struct {
	CLIVersion string `json:"cliVersion"`
	// MinRancherVersion is the CLI's declared minimum supported server
	// version (PS-8, warn-only floor); always present.
	MinRancherVersion string `json:"minRancherVersion"`
	OutputAPIVersion  string `json:"outputApiVersion"`
	// ReadOnly reports this invocation's resolved read-only state (config +
	// env + flag OR'd) — an agent reading the manifest sees the session
	// cannot mutate BEFORE trying. ReadOnlySources names the active
	// source(s); empty when ReadOnly is false.
	ReadOnly        bool            `json:"readOnly"`
	ReadOnlySources []string        `json:"readOnlySources,omitempty"`
	ErrorCodes      []string        `json:"errorCodes"`
	GlobalFlags     []GlobalFlag    `json:"globalFlags"`
	EnvVars         []EnvVar        `json:"envVars"`
	EffectSchema    EffectSchema    `json:"effectSchema"`
	Commands        []SchemaCommand `json:"commands"`
	// ShellCompletion lists the shells `completion <shell>` emits scripts
	// for — one capability field so an agent or docs tool can discover
	// completion support without parsing the command list. Always present.
	ShellCompletion []string `json:"shellCompletion"`
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

// Schema assembles the manifest, merging each Descriptor with the resolver
// tier. minRancherVersion is threaded through from the caller (like
// cliVersion) rather than imported: internal/runtime must not import
// internal/config, where the const lives. ReadOnly/ReadOnlySources are left
// at their zero value here — that state is per-invocation, not registry
// data; the schema command overlays it after calling Schema.
func (reg *Registry) Schema(cliVersion, minRancherVersion string) Manifest {
	m := Manifest{
		CLIVersion:        cliVersion,
		MinRancherVersion: minRancherVersion,
		OutputAPIVersion:  output.APIVersion,
		ErrorCodes:        clierr.AllCodes(),
		GlobalFlags:       GlobalFlags(),
		EnvVars:           EnvVars(),
		EffectSchema:      effectSchema(),
		ShellCompletion:   ShellCompletionShells(),
	}
	for _, c := range reg.cmds {
		d := c.Describe()
		sc := SchemaCommand{
			Name: d.Name, Aliases: d.Aliases, Usage: d.Usage, Desc: d.Desc, Args: d.Args, Flags: d.Flags,
			Examples: d.Examples, CommandType: d.CommandType,
			Destructive: d.Destructive, Toolset: d.Toolset, Network: d.Network, Auth: d.Auth,
			ProjectScope: d.ProjectScope,
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
