package runtime

import (
	"fmt"
	"slices"
	"sort"
	"strings"

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
// the surface layer so there is one source of truth. "wide" is presentation,
// not a scope change: on a payload with no wide-only columns it renders
// identically to "table" (kubectl's own -o wide contract).
var OutputFormats = []string{"table", "wide", "json", "yaml", "name"}

// ColorModes is the canonical set of --color literal values: the schema
// enum, globalArgSpecs' completion enum, and Run's validation all key off
// this one slice. "" (the flag omitted) is accepted as a spelling of
// "auto" but is not itself a completion candidate.
var ColorModes = []string{"auto", "always", "never"}

// OutputPolicy restricts which -o/--output formats a command accepts. The
// zero value (empty AllowedFormats) accepts every OutputFormats literal
// plus template=<src> - today's behavior for every command that does not
// opt in. A declared AllowedFormats is a closed allow-list: entries must be
// OutputFormats literals or the literal "template" (standing for any
// template=<src> value, regardless of source) - Registry.Validate rejects
// anything else.
type OutputPolicy struct {
	AllowedFormats []string
}

// Allows reports whether format is permitted under p. format "" (the
// default) is checked as its "table" spelling; a template=<src> value is
// checked against the literal "template" entry.
func (p OutputPolicy) Allows(format string) bool {
	if len(p.AllowedFormats) == 0 {
		return true
	}
	if format == "" {
		format = "table"
	}
	if strings.HasPrefix(format, output.TemplatePrefix) {
		format = "template"
	}
	return slices.Contains(p.AllowedFormats, format)
}

// OnceOnlyOutputPolicy is the restrictive OutputPolicy every
// Delivery: DeliveryOnceOnly descriptor declares: every format except
// "name", which would silently discard a once-only value under -o name's
// NAME-column-only projection. The ONE shared constructor every once-only
// command calls - never a second hand-authored copy of this list, so a
// future change to the once-only contract (adding or excluding a format)
// only has to be made here.
func OnceOnlyOutputPolicy() OutputPolicy {
	return OutputPolicy{AllowedFormats: []string{"table", "wide", "json", "yaml", "template"}}
}

// OutputFormatCompletions is what -o <TAB> suggests: the literal formats plus
// the template form - the bare prefix for discoverability and a seed template
// that runs as-is on any command (renders the whole data view) for the user
// to build on. The template entries are deliberately absent from the schema
// enum, which stays the closed literal set.
var OutputFormatCompletions = append(append([]string{}, OutputFormats...),
	output.TemplatePrefix, output.TemplatePrefix+"{{.}}")

// ShellCompletionShells returns the single shared list of shells
// `completion <shell>` emits scripts for - a fresh slice on every call, so
// no caller can mutate the canonical order out from under the others.
func ShellCompletionShells() []string { return []string{"bash", "zsh", "fish", "pwsh"} }

// GlobalFlags is the confirmation/output model, discoverable via schema.
func GlobalFlags() []GlobalFlag {
	return []GlobalFlag{
		{Name: "--dry-run", Type: "bool", AppliesTo: "mutating", Desc: "preview Effect, no change"},
		{Name: "--apply", Type: "bool", AppliesTo: "mutating", Desc: "execute the mutation"},
		{Name: "--yes", Type: "bool", AppliesTo: "mutating", Desc: "confirm a destructive Effect"},
		{Name: "--allow-deprecated", Type: "bool", AppliesTo: "mutating", Desc: "permit a deprecated-stability write"},
		{Name: "--no-input", Type: "bool", AppliesTo: "all", Desc: "never prompt; ambiguity becomes an error"},
		{Name: "-o/--output", Type: "enum", Enum: OutputFormats, AppliesTo: "all", Desc: "'name' = ids only; 'wide' adds extra columns to the same rows, never a filter; template=<go-template> renders a Go template over the JSON view of data"},
		{Name: "--color", Type: "enum", Enum: ColorModes, AppliesTo: "all", Desc: "when table/default/wide text is styled: auto (default) styles only a real terminal, always/never override detection; NO_COLOR turns styling off, CLICOLOR_FORCE/FORCE_COLOR turn it on, under auto", Env: []string{"NO_COLOR", "CLICOLOR_FORCE", "FORCE_COLOR"}},
		{Name: "-c/--config", Type: "string", AppliesTo: "all", Desc: "override the config directory", Env: []string{"RANCHER_CLI_CONFIG_DIR"}},
		{Name: "--debug", Type: "bool", AppliesTo: "all", Desc: "accepted; currently no effect"},
		{Name: "--project", Type: "string", AppliesTo: "all", Desc: "scope operations to project namespaces (downstream namespaced resources only); rejected with an error where it does not apply"},
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
		{Name: "RANCHER_CLI_CREDENTIAL_STORE", Desc: "credential store backend: auto, keychain, or file"},
		{Name: "RANCHER_CLI_READONLY", Flag: "--read-only", Desc: "when non-empty, refuse commands that make changes"},
		{Name: "NO_COLOR", Flag: "--color", Desc: "when non-empty, turn off styling under --color=auto"},
		{Name: "CLICOLOR_FORCE", Flag: "--color", Desc: "when set to a value other than \"0\", turn on styling under --color=auto"},
		{Name: "FORCE_COLOR", Flag: "--color", Desc: "same as CLICOLOR_FORCE; when both are set and disagree, whichever is not \"0\" wins"},
		{Name: "RANCHER_CLI_CONFIG_NO_LOCK", Desc: "when non-empty, skip the cross-process config write lock - escape hatch for filesystems with broken file locking"},
		{Name: "RANCHER_CLI_COMPLETION_DEBUG", Desc: "when non-empty, trace each phase of a shell completion request to stderr (config load, token source, budget, requests, candidate count); stdout is unchanged"},
		{Name: "RANCHER_CLI_OAUTH_AUTH_FLOW", Flag: "--auth-flow", Desc: "OAuth flow: devicecode or authcode"},
		{Name: "RANCHER_CLI_OAUTH_CALLBACK_PORT", Desc: "local callback port for the authcode OAuth flow"},
		{Name: "CATTLE_OAUTH_AUTH_FLOW", Flag: "--auth-flow", Desc: "legacy spelling honored after RANCHER_CLI_OAUTH_AUTH_FLOW"},
		{Name: "CATTLE_OAUTH_CALLBACK_PORT", Desc: "legacy spelling honored after RANCHER_CLI_OAUTH_CALLBACK_PORT"},
		{Name: "XDG_CONFIG_HOME", Desc: "base directory for the default config location"},
		{Name: "VISUAL", Desc: "preferred editor for config edit"},
		{Name: "EDITOR", Desc: "fallback editor for config edit"},
	}
}

// SchemaCommand is one command entry in the manifest (Descriptor + contract).
type SchemaCommand struct {
	Name             string      `json:"name"`
	Aliases          []string    `json:"aliases,omitempty"`
	Usage            string      `json:"usage,omitempty"`
	Desc             string      `json:"desc,omitempty"`
	Args             []ArgSpec   `json:"args,omitempty"`
	Flags            []ArgSpec   `json:"flags,omitempty"`
	Examples         []string    `json:"examples,omitempty"`
	CommandType      CommandType `json:"commandType"`
	Destructive      string      `json:"destructive,omitempty"`
	Toolset          string      `json:"toolset,omitempty"`
	Network          bool        `json:"network"`
	Auth             bool        `json:"auth"`
	Contract         string      `json:"contract,omitempty"`
	Stability        string      `json:"stability,omitempty"`
	ProjectScope     bool        `json:"projectScope,omitempty"`
	SuccessExitCodes []int       `json:"successExitCodes,omitempty"`
	// Prime is a projection of Descriptor.Capability: true when the command
	// carries the Rancher Prime capability classification (a product-tier
	// marker; whether the connected server gates it is decided at run
	// time). Never separately authored - the source of
	// truth is the requirement itself.
	Prime bool `json:"prime,omitempty"`
	// LearnMore rides beside Prime: agents reading the manifest get the one
	// stable, product-owned URL without parsing prose. Projected from
	// clierr.LearnMorePrimeURL, present exactly when Prime is.
	LearnMore string `json:"learnMore,omitempty"`
	// Gates lists the flags this command unconditionally requires before it
	// executes a mutation: --apply for every mutating command, --yes when
	// destructive is "true", --allow-deprecated on a deprecated binding.
	// A destructive "dependsOnInput" command may additionally demand --yes
	// at run time - the destructive field carries that signal. Empty for
	// read-only and local commands.
	Gates []string `json:"gates,omitempty"`
	// Group is the root section this command renders under (informational -
	// Manifest.Groups carries the ordered sections). "" for the two
	// positional-root sections (fixed top-level and ungrouped entries).
	Group string `json:"group,omitempty"`
	// OutputPolicy is a projection of Descriptor.OutputPolicy.AllowedFormats:
	// the closed set of -o/--output values this command accepts. Absent
	// means every OutputFormats literal (plus template=<src>) is accepted.
	OutputPolicy []string `json:"outputPolicy,omitempty"`
	// Delivery is a projection of Descriptor.Delivery. Absent means
	// ordinary delivery.
	Delivery string `json:"delivery,omitempty"`
	// Shape is a projection of Descriptor.Shape: the command's declared
	// success-result shape (tabular, text, bytes, execJSON). Always present -
	// Registry.Validate rejects an undeclared (unspecified) shape on
	// every registered command.
	Shape string `json:"shape"`
}

// Verb states for SchemaKind.Verbs: available and refused verbs are
// both registered commands (a refusal declines at Plan); an absent verb has
// no registered command.
const (
	VerbAvailable = "available"
	VerbRefused   = "refused"
	VerbAbsent    = "absent"
)

// VerbState is one generated verb's effective availability on a kind.
// Contract/Stability are set for registered verbs (available, refused); Note
// carries a refusal's wording or an absence's reviewed reason.
type VerbState struct {
	State     string `json:"state"`
	Contract  string `json:"contract,omitempty"`
	Stability string `json:"stability,omitempty"`
	Note      string `json:"note,omitempty"`
}

// SchemaKind is one kind row in the manifest: identity, routing, and
// the effective verb surface. Built by the surface layer (which sees both the
// kind table and its overrides) and handed to the registry at BuildRoot -
// runtime owns the DTO, not its construction.
type SchemaKind struct {
	Name             string   `json:"name"`
	Aliases          []string `json:"aliases,omitempty"`
	Key              string   `json:"key"`
	Group            string   `json:"group,omitempty"`
	Version          string   `json:"version,omitempty"`
	Resource         string   `json:"resource"`
	Scope            string   `json:"scope"`
	NamespacePolicy  string   `json:"namespacePolicy"`
	DefaultNamespace string   `json:"defaultNamespace,omitempty"`
	Sensitive        bool     `json:"sensitive,omitempty"`
	Completable      bool     `json:"completable,omitempty"`
	WaitDefault      string   `json:"waitDefault,omitempty"`
	// Prime is a projection of kinds.Kind.Capability: true when the kind
	// carries the Rancher Prime capability classification (product-tier
	// marker; the connected server decides availability at run time).
	Prime bool                 `json:"prime,omitempty"`
	Verbs map[string]VerbState `json:"verbs"`
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
	actions := []safety.Action{safety.ActionCreate, safety.ActionUpdate, safety.ActionDelete}
	strs := make([]string, len(actions))
	for i, a := range actions {
		strs[i] = string(a)
	}
	return EffectSchema{
		Actions: strs,
		Fields: []EffectField{
			{Name: "action", Type: "string"},
			{Name: "target", Type: "string"},
			{Name: "displayKind", Type: "string"},
			{Name: "displayName", Type: "string"},
			{Name: "server", Type: "string"},
			{Name: "cluster", Type: "string"},
			{Name: "diff", Type: "array"},
			{Name: "resourceVersion", Type: "string"},
			{Name: "conflictDetection", Type: "bool"},
			{Name: "reversible", Type: "bool"},
			{Name: "destructive", Type: "bool"},
		},
	}
}

// Manifest is the full `rancher schema` output.
type Manifest struct {
	CLIVersion string `json:"cliVersion"`
	// MinRancherVersion is the CLI's declared minimum supported server
	// version (a warn-only floor); always present.
	MinRancherVersion string `json:"minRancherVersion"`
	OutputAPIVersion  string `json:"outputApiVersion"`
	// ReadOnly reports this invocation's resolved read-only state (config +
	// env + flag OR'd) - an agent reading the manifest sees the session
	// cannot mutate BEFORE trying. ReadOnlySources names the active
	// source(s); empty when ReadOnly is false.
	ReadOnly        bool     `json:"readOnly"`
	ReadOnlySources []string `json:"readOnlySources,omitempty"`
	// ErrorCodes lists every stable error code with its exit status and its
	// meaning, so an agent learns the error vocabulary from the manifest
	// before it meets an error. ExitCodes is the same exit mapping as a
	// map; the table-of-contents view carries it as globalExitCodes.
	ErrorCodes   []clierr.CodeInfo `json:"errorCodes"`
	ExitCodes    map[string]int    `json:"exitCodes"`
	GlobalFlags  []GlobalFlag      `json:"globalFlags"`
	EnvVars      []EnvVar          `json:"envVars"`
	EffectSchema EffectSchema      `json:"effectSchema"`
	Commands     []SchemaCommand   `json:"commands"`
	// Kinds is the kind vocabulary with its effective verb surface;
	// Groups are the root's ordered sections. Both are set
	// by BuildRoot via SetKinds/SetGroups; registries built without them
	// (tests) omit the fields.
	Kinds  []SchemaKind  `json:"kinds,omitempty"`
	Groups []SchemaGroup `json:"groups,omitempty"`
	// ShellCompletion lists the shells `completion <shell>` emits scripts
	// for - one capability field so an agent or docs tool can discover
	// completion support without parsing the command list. Always present.
	ShellCompletion []string `json:"shellCompletion"`
}

// SchemaGroup is one ordered root section: sections appear in EXACT
// root-assembly order with commands in registration order. The two
// positional-root sections - the leading fixed top-level commands and the
// ungrouped-entry run - carry Name "" and are distinguished by position.
// Named groups carry a Desc that rancher schema <group> surfaces as the
// group card; unnamed sections are desc-less.
type SchemaGroup struct {
	Name     string   `json:"name"`
	Desc     string   `json:"desc,omitempty"`
	Commands []string `json:"commands"`
}

// Resolver maps operation names to their current contract binding. provider
// implements it; the kind table composes into it in a later branch.
type Resolver interface {
	Resolve(op string) (safety.Binding, bool)
	All() map[string]safety.Binding
}

// Registry holds all commands, the contract resolver, and the schema-only data
// the surface layer hands over at BuildRoot (kind rows, ordered sections).
type Registry struct {
	resolver Resolver
	cmds     []Command
	kinds    []SchemaKind
	groups   []SchemaGroup
}

func NewRegistry(r Resolver) *Registry { return &Registry{resolver: r} }

func (reg *Registry) Register(c Command) { reg.cmds = append(reg.cmds, c) }

// SetKinds installs the manifest's kind rows - built by the surface layer
// from the kind table and its overrides, same direction as resolver bindings.
func (reg *Registry) SetKinds(ks []SchemaKind) { reg.kinds = ks }

// SetGroups installs the manifest's ordered root sections, built by
// the surface layer from the same registration data that assembles the root.
func (reg *Registry) SetGroups(gs []SchemaGroup) { reg.groups = gs }

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
		// Validate's contract is plain errors (every other failure here is
		// fmt.Errorf); unwrap the shared validator's structured error to its
		// message rather than leaking one *clierr.Error through this seam.
		if ce := ValidCapabilityShape(d.Capability, d.Name); ce != nil {
			return fmt.Errorf("%s", ce.Message)
		}
		if d.Capability.IsSet() {
			if _, ok := c.(CapabilityResolver); ok {
				return fmt.Errorf("command %q: static capability requirement and CapabilityResolver are mutually exclusive", d.Name)
			}
		}
		for _, f := range d.OutputPolicy.AllowedFormats {
			if f != "template" && !slices.Contains(OutputFormats, f) {
				return fmt.Errorf("command %q: outputPolicy allows unknown format %q", d.Name, f)
			}
		}
		if d.Shape == ShapeUnspecified {
			return fmt.Errorf("command %q: output shape must be declared (ShapeUnspecified is never valid)", d.Name)
		}
		if d.Delivery != "" {
			if d.Delivery != DeliveryOnceOnly {
				return fmt.Errorf("command %q: unknown delivery classification %q", d.Name, d.Delivery)
			}
			if d.CommandType != Mutating {
				return fmt.Errorf("command %q: delivery %q is permitted only on Mutating commands", d.Name, d.Delivery)
			}
		}
		if codes := d.SuccessExitCodes; len(codes) > 0 {
			errExits := map[int]bool{}
			for _, e := range clierr.CodeExits() {
				errExits[e] = true
			}
			zeros, seen := 0, map[int]bool{}
			for _, c := range codes {
				switch {
				case c == 0:
					zeros++
				case c < 0:
					return fmt.Errorf("command %q: successExitCodes value %d must be positive", d.Name, c)
				case seen[c]:
					return fmt.Errorf("command %q: duplicate successExitCodes value %d", d.Name, c)
				case errExits[c]:
					return fmt.Errorf("command %q: successExitCodes value %d collides with error exit", d.Name, c)
				}
				if c != 0 {
					seen[c] = true
				}
			}
			if zeros != 1 {
				return fmt.Errorf("command %q: successExitCodes must contain 0 exactly once", d.Name)
			}
		}
	}
	return nil
}

// Schema assembles the manifest, merging each Descriptor with the resolver
// binding. minRancherVersion is threaded through from the caller (like
// cliVersion) rather than imported: internal/runtime must not import
// internal/config, where the const lives. ReadOnly/ReadOnlySources are left
// at their zero value here - that state is per-invocation, not registry
// data; the schema command overlays it after calling Schema.
func (reg *Registry) Schema(cliVersion, minRancherVersion string) Manifest {
	m := Manifest{
		CLIVersion:        cliVersion,
		MinRancherVersion: minRancherVersion,
		OutputAPIVersion:  output.APIVersion,
		ErrorCodes:        clierr.Codes(),
		ExitCodes:         clierr.CodeExits(),
		GlobalFlags:       GlobalFlags(),
		EnvVars:           EnvVars(),
		EffectSchema:      effectSchema(),
		ShellCompletion:   ShellCompletionShells(),
		Kinds:             reg.kinds,
		Groups:            reg.groups,
	}
	// Per-command Group is derived from Groups membership: the "" sections
	// contribute nothing, so their commands keep the zero value.
	groupOf := map[string]string{}
	for _, g := range reg.groups {
		if g.Name == "" {
			continue
		}
		for _, name := range g.Commands {
			groupOf[name] = g.Name
		}
	}
	for _, c := range reg.cmds {
		d := c.Describe()
		toolset := d.Toolset
		if toolset == "" {
			toolset = "core"
		}
		sc := SchemaCommand{
			Name: d.Name, Aliases: d.Aliases, Usage: d.Usage, Desc: d.Desc, Args: d.Args, Flags: d.Flags,
			Examples: d.Examples, CommandType: d.CommandType,
			Destructive: d.Destructive, Toolset: toolset, Network: d.Network, Auth: d.Auth,
			ProjectScope: d.ProjectScope, Group: groupOf[d.Name],
			SuccessExitCodes: d.SuccessExitCodes,
			Prime:            d.Capability.Tier == CapabilityPrime,
			OutputPolicy:     d.OutputPolicy.AllowedFormats,
			Delivery:         string(d.Delivery),
			Shape:            d.Shape.String(),
		}
		if sc.Prime {
			sc.LearnMore = clierr.LearnMorePrimeURL
		}
		if b, ok := reg.resolver.Resolve(d.Name); ok {
			sc.Contract = string(b.Contract)
			sc.Stability = string(b.Stability)
		}
		sc.Gates = Gates(sc.CommandType, sc.Destructive, sc.Stability)
		m.Commands = append(m.Commands, sc)
	}
	sort.Slice(m.Commands, func(i, j int) bool { return m.Commands[i].Name < m.Commands[j].Name })
	return m
}
