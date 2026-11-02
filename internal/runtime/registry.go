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
	Name string   `json:"name"`
	Type string   `json:"type"`
	Enum []string `json:"enum,omitempty"`
	// AppliesTo names the commands the flag applies to: "all" is every
	// command; "mutating" is the commands that change server resources;
	// "server" is the commands that read or change server resources, and
	// the flag is refused on local commands; "declared" is the commands
	// that list the flag in their own flags.
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
		{Name: "--dry-run", Type: "bool", AppliesTo: "mutating", Desc: "preview the changes without applying them"},
		{Name: "--apply", Type: "bool", AppliesTo: "mutating", Desc: "apply the change"},
		{Name: "--yes", Type: "bool", AppliesTo: "mutating", Desc: "confirm a destructive change"},
		{Name: "--allow-deprecated", Type: "bool", AppliesTo: "mutating", Desc: "allow writing to a deprecated resource"},
		{Name: "--no-input", Type: "bool", AppliesTo: "all", Desc: "never prompt; fail with an error when the input is ambiguous"},
		{Name: "-o/--output", Type: "enum", Enum: OutputFormats, AppliesTo: "all", Desc: "'name' prints only the NAME column, one per line; 'wide' adds extra columns to the same rows and never filters rows; template=<go-template> renders a Go template over the `data` of the JSON output"},
		{Name: "--color", Type: "enum", Enum: ColorModes, AppliesTo: "all", Desc: "when table (the default) and wide output is styled: auto (default) styles only output sent to a terminal, always and never skip that check; under auto, NO_COLOR turns styling off and CLICOLOR_FORCE or FORCE_COLOR turn it on", Env: []string{"NO_COLOR", "CLICOLOR_FORCE", "FORCE_COLOR"}},
		{Name: "-c/--config", Type: "string", AppliesTo: "all", Desc: "override the config directory", Env: []string{"RANCHER_CLI_CONFIG_DIR"}},
		{Name: "--debug", Type: "bool", AppliesTo: "all", Desc: "In a development build whose run attempts a telemetry upload, prints on stderr one line before each request (event count, contract version, endpoint origin) and one after (HTTP status and refused count, or why there was no answer). Never prints event contents; prints nothing else."},
		{Name: "--project", Type: "string", AppliesTo: "declared", Desc: "the project, by id or display name, on the cluster given by --cluster; only the commands that take --project accept it, and every other command refuses it before sending a request"},
		{Name: "--read-only", Type: "bool", AppliesTo: "server", Desc: "refuse commands that make changes, for this run; once set it cannot be turned off; applies to commands that read or change server resources and is refused on local commands (the config setting readOnly and a non-empty RANCHER_CLI_READONLY apply to every run and leave local commands usable)", Env: []string{"RANCHER_CLI_READONLY"}},
	}
}

// EnvVar documents an environment variable the CLI reads. Flag names the flag
// for the same setting, on the commands that take it; env-only variables
// leave it empty.
type EnvVar struct {
	Name string `json:"name"`
	Desc string `json:"desc,omitempty"`
	Flag string `json:"flag,omitempty"`
}

// EnvVars is the complete list of the environment variables a user sets to
// change how the CLI behaves, discoverable via schema. Variables the CLI only
// reads to describe its caller or to find another tool (KUBERNETES_EXEC_INFO,
// the agent and CI variables, CODEX_HOME, PATH) are not listed. The names
// are literals here because internal/runtime must not import internal/config, where the consts live (see
// Schema); keep this in sync with internal/config/env.go and the raw reads in
// the token and editor code.
func EnvVars() []EnvVar {
	return []EnvVar{
		{Name: "RANCHER_CLI_CONFIG_DIR", Flag: "-c/--config", Desc: "config directory"},
		{Name: "RANCHER_CLI_URL", Flag: "--server", Desc: "Rancher server URL, used when --server is not given"},
		{Name: "RANCHER_CLI_TOKEN", Desc: "Rancher API token (access-key:secret-key) to authenticate without a prompt"},
		{Name: "RANCHER_CLI_CACERT", Flag: "--cacert", Desc: "PEM CA bundle, as a file path or as the PEM text itself"},
		{Name: "RANCHER_CLI_CREDENTIAL_STORE", Flag: "--credential-store", Desc: "credential store backend: auto, keychain, or file"},
		{Name: "RANCHER_CLI_READONLY", Flag: "--read-only", Desc: "when non-empty, every run refuses commands that make changes; local commands stay usable"},
		{Name: "NO_COLOR", Flag: "--color", Desc: "when non-empty, turn off styling under --color=auto"},
		{Name: "CLICOLOR_FORCE", Flag: "--color", Desc: "when set to a value other than \"0\", turn on styling under --color=auto"},
		{Name: "FORCE_COLOR", Flag: "--color", Desc: "same as CLICOLOR_FORCE; when both are set and one of them is \"0\", the one that is not \"0\" takes precedence"},
		{Name: "RANCHER_CLI_CONFIG_NO_LOCK", Desc: "when non-empty, do not take the locks that stop two CLI processes from writing the config directory at the same time; use it only on a filesystem with broken file locking"},
		{Name: "RANCHER_CLI_COMPLETION_DEBUG", Desc: "when non-empty, trace each phase of a shell completion request to stderr (config load, where the token comes from, time limit, requests, number of candidates); stdout is unchanged"},
		{Name: "RANCHER_CLI_OAUTH_AUTH_FLOW", Flag: "--auth-flow", Desc: "OAuth flow: devicecode or authcode"},
		{Name: "RANCHER_CLI_OAUTH_CALLBACK_PORT", Desc: "local callback port for the authcode OAuth flow"},
		{Name: "CATTLE_OAUTH_AUTH_FLOW", Flag: "--auth-flow", Desc: "older name of RANCHER_CLI_OAUTH_AUTH_FLOW; used only when that variable is not set or is empty"},
		{Name: "CATTLE_OAUTH_CALLBACK_PORT", Desc: "older name of RANCHER_CLI_OAUTH_CALLBACK_PORT; used only when that variable is not set or is empty"},
		{Name: "RANCHER_CLI_TELEMETRY", Desc: "true or false: turns usage recording on or off and overrides the telemetry key of config.yaml; empty counts as unset and any other value is ignored"},
		{Name: "RANCHER_CLI_EXPERIMENT", Desc: "experimental: a comma-separated list of name[=value] experiments, honored only in a development build; telemetryupload=<url> makes the build send the stored events of finished hours to that endpoint at most once an hour, after a recorded run, never from the kubectl exec plugin"},
		{Name: "XDG_CONFIG_HOME", Desc: "base directory for the default config directory"},
		{Name: "VISUAL", Desc: "preferred editor for config edit"},
		{Name: "EDITOR", Desc: "fallback editor for config edit"},
	}
}

// SchemaCommand is one command entry in the manifest (Descriptor + contract).
type SchemaCommand struct {
	Name             string       `json:"name"`
	Aliases          []string     `json:"aliases,omitempty"`
	Usage            string       `json:"usage,omitempty"`
	Desc             string       `json:"desc,omitempty"`
	Args             []ArgSpec    `json:"args,omitempty"`
	Flags            []ArgSpec    `json:"flags,omitempty"`
	Examples         []string     `json:"examples,omitempty"`
	CommandType      CommandType  `json:"commandType"`
	Destructive      string       `json:"destructive,omitempty"`
	Toolset          string       `json:"toolset,omitempty"`
	ServerAccess     ServerAccess `json:"serverAccess"`
	Auth             bool         `json:"auth"`
	Contract         string       `json:"contract,omitempty"`
	Stability        string       `json:"stability,omitempty"`
	SuccessExitCodes []int        `json:"successExitCodes,omitempty"`
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
			{Name: "objects", Type: "array"},
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
	// Telemetry is the usage contract version and the controls that decide
	// whether recording is on; fixed text, the same on every machine.
	Telemetry TelemetryManifest `json:"telemetry"`
	// Experiments is the table of experimental settings the
	// RANCHER_CLI_EXPERIMENT variable turns on; fixed text, the same on every
	// machine.
	Experiments []SchemaExperiment `json:"experiments"`
	Commands    []SchemaCommand    `json:"commands"`
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
			return fmt.Errorf("command %q has commandType %q, which does not match the interface it implements (ReadCommand or MutatingCommand)", d.Name, d.CommandType)
		}
		if err := validServerAccess(d); err != nil {
			return err
		}
		if isMut {
			if _, ok := reg.resolver.Resolve(d.Name); !ok {
				return fmt.Errorf("mutating command %q has no contract and stability set", d.Name)
			}
		}
		// Validate's contract is plain errors (every other failure here is
		// fmt.Errorf); the shared validator's text carries no "internal
		// error: " lead, which the build of the command tree adds once
		// (validateWiring in internal/cli/root.go).
		if problem := ValidCapabilityShape(d.Capability, d.Name); problem != "" {
			return fmt.Errorf("%s", problem)
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
				return fmt.Errorf("command %q: unknown delivery %q", d.Name, d.Delivery)
			}
			if d.CommandType != Mutating {
				return fmt.Errorf("command %q: delivery %q is allowed only on mutating commands", d.Name, d.Delivery)
			}
		}
		if err := validateProjectFlag(c, d); err != nil {
			return err
		}
		if err := validateFlagNames(d); err != nil {
			return err
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
					return fmt.Errorf("command %q: successExitCodes value %d is also an error exit code", d.Name, c)
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

// validServerAccess checks the declared server access against the command
// type: a mutating command only writes, a read-only command reads or never
// contacts the server, a local command declares any of the three.
func validServerAccess(d Descriptor) error {
	switch a := d.EffectiveServerAccess(); {
	case a != ServerAccessNone && a != ServerAccessRead && a != ServerAccessWrite:
		return fmt.Errorf("command %q: unknown serverAccess %q", d.Name, a)
	case d.CommandType == Mutating && a != ServerAccessWrite:
		return fmt.Errorf("command %q: a mutating command's serverAccess must be %q, not %q", d.Name, ServerAccessWrite, a)
	case d.CommandType == ReadOnly && a == ServerAccessWrite:
		return fmt.Errorf("command %q: a read-only command's serverAccess must be %q or %q, not %q", d.Name, ServerAccessRead, ServerAccessNone, a)
	}
	return nil
}

// validateProjectFlag checks how d takes the global --project flag: only as
// ProjectFlag's entry, never beside a refusal hint for it, and no other flag
// of d may reuse a global flag's name, which a value typed before the
// command name would not reach.
func validateProjectFlag(c Command, d Descriptor) error {
	globals := globalFlagNames()
	for _, f := range d.Flags {
		if f.Name == FlagProject {
			if f.Type != "string" || len(f.Aliases) > 0 || len(f.Enum) > 0 || len(f.Env) > 0 {
				return fmt.Errorf("command %q: the project flag must be runtime.ProjectFlag's entry (type string, no alias, enum or env)", d.Name)
			}
			continue
		}
		for _, n := range append([]string{f.Name}, f.Aliases...) {
			if globals[n] {
				return fmt.Errorf("command %q: flag %q reuses the name of a global flag", d.Name, n)
			}
		}
	}
	if _, ok := c.(ProjectRefusalHinter); ok && TakesProject(d) {
		return fmt.Errorf("command %q: takes --project and also implements ProjectRefusalHinter, which is for a command that does not take --project", d.Name)
	}
	return nil
}

// validateFlagNames checks that every name and alias of d's flags is
// declared once: two flags with one name, an alias equal to another flag's
// name or alias, and a flag that lists its own name as an alias all give
// the parser two flags for one spelling.
func validateFlagNames(d Descriptor) error {
	seen := map[string]bool{}
	for _, f := range d.Flags {
		for _, n := range slices.Concat([]string{f.Name}, f.Aliases) {
			if seen[n] {
				return fmt.Errorf("command %q: the flag name or alias %q is declared twice", d.Name, n)
			}
			seen[n] = true
		}
	}
	return nil
}

// globalFlagNames is every name and short form of GlobalFlags ("-o/--output"
// gives "o" and "output").
func globalFlagNames() map[string]bool {
	names := map[string]bool{}
	for _, g := range GlobalFlags() {
		for n := range strings.SplitSeq(g.Name, "/") {
			names[strings.TrimLeft(n, "-")] = true
		}
	}
	return names
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
		Telemetry:         telemetryManifest(),
		Experiments:       experimentsManifest(),
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
			Destructive: d.Destructive, Toolset: toolset, ServerAccess: d.EffectiveServerAccess(), Auth: d.Auth,
			Group:            groupOf[d.Name],
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
