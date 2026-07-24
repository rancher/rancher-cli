package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/sirupsen/logrus"
	ucli "github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// entry is one command's declarative registration: its verb group and a single
// constructor taking runDeps. The constructor pulls whatever it needs (client
// factory, file source, config dir/store) off runDeps and ignores the rest, so
// adding a command is one row here — the compiler enforces the dependency set,
// with no per-shape adapter or wiring switch.
type entry struct {
	group string
	make  func(runDeps) runtime.Command
}

// buildEntries is the single source of command wiring: the bespoke config/token
// and create commands, plus generated get/delete commands for every kind in the
// table. Adding a command is one row here (plus a kind-table row for a new
// kind).
func buildEntries() []entry {
	entries := []entry{
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogin(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogout(d.dir) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewAuthCheck(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewTokenCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewListTokensCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewDeleteTokenCommand(d.dir, d.store) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextCurrent(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextSet(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewContextUse(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerLs(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerSwitch(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigDeleteServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigPath(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigEdit(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewSetCompletion(d.dir) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreate(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreateNamespace(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreateSecret(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return resources.NewCreate(d.clients, d.src) }},
		{group: "create", make: func(d runDeps) runtime.Command { return extres.NewCreateToken(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return extres.NewCreateKubeconfig(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return provisioning.NewCreateCluster(d.clients, d.roots) }},
		{group: "project", make: func(d runDeps) runtime.Command { return projects.NewAddNamespace(d.clients) }},
		{group: "project", make: func(d runDeps) runtime.Command { return projects.NewRemoveNamespace(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewPod(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewDeployment(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectProject(d.clients) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetKubernetesVersions(d.roots, d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewGrant(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewRevoke(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewImages(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewUsage(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return commands.NewKubeconfig(d.dir, d.store, d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return observe.NewLogs(d.clients) }},
	}
	ov := kindOverrides()
	for _, k := range kinds.All() {
		k := k
		o := ov[k.Key]
		entries = append(entries, entry{group: "get", make: func(d runDeps) runtime.Command {
			if o.get != nil {
				return o.get(k, d)
			}
			return resources.NewGet(k, d.clients)
		}})
		entries = append(entries, entry{group: "patch", make: func(d runDeps) runtime.Command {
			if o.patch != nil {
				return o.patch(k, d)
			}
			return resources.NewPatch(k, d.clients, d.src)
		}})
		if k.Deletable || delOverrideOnNonDeletable(k, o) {
			entries = append(entries, entry{group: "delete", make: func(d runDeps) runtime.Command {
				if o.del != nil {
					return o.del(k, d)
				}
				return resources.NewDelete(k, d.clients)
			}})
		}
	}
	return entries
}

// kindOverride replaces the GENERIC constructor for exactly the verbs it
// sets, for exactly one kind row. The row remains the single source of
// schema/completion/routing identity; only execution changes. A kind
// whose row would not generate a verb (Deletable:false + del override)
// still gets the override's command — that is how provisioningcluster
// delete ships guarded without a bare Deletable flip (spec D-R1-3).
type kindOverride struct {
	get   func(k kinds.Kind, d runDeps) runtime.Command
	patch func(k kinds.Kind, d runDeps) runtime.Command
	del   func(k kinds.Kind, d runDeps) runtime.Command
	// refusals marks override commands that are pure refusals (registered,
	// but declining at Plan), keyed by verb; the note is surfaced as the
	// verb's state in the schema manifest (SE-17). A verb listed here must
	// also have its constructor set above.
	refusals map[string]string
}

// delOverrideOnNonDeletable reports whether ov installs a del override for a
// kind whose own row stays Deletable: false — the one combination both
// buildEntries (does a delete command get generated at all) and BuildRoot's
// delOverrides loop (does an extra resolver binding need composing, since
// kinds.Bindings() never emits "delete <name>" for a non-Deletable row)
// need to agree on. Single source over the two underlying facts (k.Deletable,
// ov.del != nil) so an edit to one call site can't silently desync from the
// other.
func delOverrideOnNonDeletable(k kinds.Kind, ov kindOverride) bool {
	return ov.del != nil && !k.Deletable
}

// kindOverrideTable is the override registry, keyed by kinds.Kind.Key. A
// package var rather than a literal inside kindOverrides so tests can
// install a marker override and restore the original on cleanup.
//
// clusterregistrationtoken (Task 5, spec D-4): get is a Steve /v1 read (the
// only place the real token is spliced into the command/manifest fields);
// patch is refused outright — the row is read-only surface.
var kindOverrideTable = map[string]kindOverride{
	"clusterregistrationtoken": {
		get:      func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewGetCRT(k, d.roots) },
		patch:    func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewRefuseCRTPatch(k) },
		refusals: map[string]string{"patch": "read-only: consumed, not edited"},
	},
	// provisioningcluster (Task 8, spec D-5): patch keeps the generic
	// diff/redaction/RV-precondition behavior — only an advisory etcd-quorum
	// preview is added, via the EffectEnricher seam, never a gate.
	"provisioningcluster": {
		patch: func(k kinds.Kind, d runDeps) runtime.Command {
			return resources.NewPatch(k, d.clients, d.src, func(orig, patched *unstructured.Unstructured, eff *safety.Effect) {
				if delta, ok := provisioning.ComputeQuorumDelta(orig, patched); ok {
					eff.Warnings = append(eff.Warnings, delta.Note)
				}
			})
		},
		// del (Task 9, spec D-6): the row stays Deletable: false (no generic
		// verb would be generated), but the override still supplies a
		// bespoke, guarded delete command — the exact D-R1-3 point. See
		// BuildRoot's extra "delete provisioning-cluster" resolver, derived
		// from this row's own Binding: kinds.Bindings() never emits a
		// binding for this op since the row is not Deletable, so wireEntry
		// would otherwise reject this command at BuildRoot startup.
		del: func(k kinds.Kind, d runDeps) runtime.Command {
			return provisioning.NewDeleteProvisioningCluster(k, d.clients)
		},
	},
}

// kindOverrides returns the override registry. The FUNCTION is the loop's
// seam — buildEntries calls this, never the var directly — so later tasks
// (and tests) have exactly one place to swap the table.
func kindOverrides() map[string]kindOverride {
	return kindOverrideTable
}

// schemaKinds builds the manifest's kind rows (SE-8) with the effective verb
// surface (SE-17) from the same two sources buildEntries generates commands
// from — the kind table and the override registry — so the manifest cannot
// drift from what is actually registered: a refusals entry surfaces as
// "refused"; a del override on a Deletable: false row surfaces delete as
// "available" (the provisioningcluster case); a delete neither the row nor
// an override generates is "absent", carrying the row's NoDeleteReason.
func schemaKinds(rows []kinds.Kind, overrides map[string]kindOverride) []runtime.SchemaKind {
	out := make([]runtime.SchemaKind, 0, len(rows))
	for _, k := range rows {
		ov := overrides[k.Key]
		verbs := map[string]runtime.VerbState{
			"get":   registeredVerbState(k, "get", ov),
			"patch": registeredVerbState(k, "patch", ov),
		}
		if k.Deletable || delOverrideOnNonDeletable(k, ov) {
			verbs["delete"] = registeredVerbState(k, "delete", ov)
		} else {
			verbs["delete"] = runtime.VerbState{State: runtime.VerbAbsent, Note: k.NoDeleteReason}
		}
		out = append(out, runtime.SchemaKind{
			Name:             k.Name,
			Aliases:          k.Aliases,
			Key:              k.Key,
			Group:            k.GVR.Group,
			Version:          k.GVR.Version,
			Resource:         k.GVR.Resource,
			Scope:            string(k.Scope),
			NamespacePolicy:  string(k.Policy),
			DefaultNamespace: k.DefaultNamespace,
			Sensitive:        k.Sensitive,
			Completable:      k.Completable,
			WaitDefault:      k.WaitDefault,
			Verbs:            verbs,
		})
	}
	return out
}

// registeredVerbState describes a verb that HAS a registered command:
// available unless the override marks it a refusal. Tier/stability come from
// the row's own Binding — the same source kinds.Bindings() (and BuildRoot's
// del-override resolver) feed the resolver from.
func registeredVerbState(k kinds.Kind, verb string, ov kindOverride) runtime.VerbState {
	vs := runtime.VerbState{
		State:     runtime.VerbAvailable,
		Tier:      string(k.Binding.Tier),
		Stability: string(k.Binding.Stability),
	}
	if note, refused := ov.refusals[verb]; refused {
		vs.State = runtime.VerbRefused
		vs.Note = note
	}
	return vs
}

// wireEntry validates one entry and returns its urfave leaf plus the
// command's registered name (BuildRoot records it in the entry's root
// section — SE-18). It probes Describe() with zero deps (Describe must be
// dir/store/client-independent), checks the name's group matches the declared
// group, requires every engine command — read or mutating (CommandType !=
// Local) — to resolve to a binding, registers the probe, and wires it through
// the single run adapter.
func wireEntry(reg *runtime.Registry, resolver runtime.Resolver, e entry) (*ucli.Command, string, error) {
	if e.make == nil {
		return nil, "", fmt.Errorf("entry in group %q has no constructor", e.group)
	}
	d := e.make(runDeps{}).Describe()

	// The tree is two levels deep: a command name is exactly one token (a
	// top-level command) or two (group + leaf). Reject 3+ tokens with a clear
	// message rather than a misleading group mismatch.
	if n := strings.Count(d.Name, " "); n > 1 {
		return nil, "", fmt.Errorf("command name %q must be one or two space-separated tokens, has %d", d.Name, n+1)
	}
	if g := groupName(d.Name); g != e.group {
		return nil, "", fmt.Errorf("command %q is wired under group %q but its name implies %q", d.Name, e.group, g)
	}
	// Engine commands (read or mutating) must resolve to a binding; Local
	// config/auth commands carry no tier.
	if d.CommandType != runtime.Local {
		if _, ok := resolver.Resolve(d.Name); !ok {
			return nil, "", fmt.Errorf("engine command %q has no resolver binding", d.Name)
		}
	}

	reg.Register(e.make(runDeps{}))
	return adaptEntry(reg, e.make), d.Name, nil
}

// groupUsages documents the known verb groups. A group discovered from the
// entries but absent here still gets a (usage-less) group command — command
// reachability never depends on an allowlist.
var groupUsages = map[string]string{
	"auth":    "Auth operations",
	"config":  "Config operations",
	"create":  "Create resources",
	"get":     "Read resources",
	"patch":   "Patch resources",
	"delete":  "Delete resources",
	"inspect": "Inspect related resources together",
	"project": "Project namespace operations (membership is managed by 'rancher grant' and 'rancher revoke')",
}

// aliasAwareGroupComplete reproduces urfave's DefaultCompleteWithFlags for a
// verb group's own ShellComplete (the two branches: flag names when the
// current word starts with "-", subcommand suggestions otherwise) but
// additionally lists each leaf's Aliases: urfave's own default lists only
// each leaf's canonical Name (printCommandSuggestions), so `po<TAB>` under
// `get` would complete only toward the canonical "pod" form otherwise. The
// lastArg computation mirrors DefaultCompleteWithFlags' own (unexported)
// logic exactly, for the non-root case (cmd.parent != nil, cmd.Args().Slice()).
//
// Under v2, rawCurrentWord is classified the same way the leaf path does it
// (ParseArgTail/classifyCurrentWordV2): equals-form flag values, partial flag
// names, and bare words are each routed to the appropriate candidate
// generator. The classification lives in completeCurrentWordV2, shared with
// the root command's own ShellComplete; only the bare-word branch — the
// alias-aware subcommand listing — is supplied here.
func aliasAwareGroupComplete(ctx context.Context, cmd *ucli.Command) {
	if rawProtoV2 {
		completeCurrentWordV2(ctx, cmd, func(w io.Writer) {
			// Bare word (or empty): prefix-filter subcommand names and aliases
			// by rawCurrentWord independently. An empty rawCurrentWord matches
			// everything (strings.HasPrefix(s, "") is always true), preserving the
			// "list all kinds + aliases" behavior for `get <TAB>`. Name and each
			// alias are tested independently so an alias-only prefix (e.g. "ns" for
			// namespace) is not gated on the canonical name matching first.
			for _, leaf := range cmd.Commands {
				if leaf.Hidden {
					continue
				}
				if strings.HasPrefix(leaf.Name, rawCurrentWord) {
					printCompletionCandidate(w, leaf.Name, leaf.Usage)
				}
				for _, alias := range leaf.Aliases {
					if strings.HasPrefix(alias, rawCurrentWord) {
						printCompletionCandidate(w, alias, leaf.Usage)
					}
				}
			}
		})
		return
	}
	// v1 / subcommand path: derive lastArg from cmd.Args(), which in v1
	// shellCompletion mode still holds the partial word (preserved by urfave's
	// parseFlags when it sees "--" and shellCompletion is true). The subcommand
	// alias listing is also handled here for any non-flag currentWord.
	args := cmd.Args().Slice()
	lastArg := ""
	if n := len(args); n > 1 {
		lastArg = args[n-2]
	} else if n == 1 {
		lastArg = args[0]
	}
	if lastArg == "--generate-shell-completion" {
		lastArg = ""
	}
	if strings.HasPrefix(lastArg, "-") {
		ucli.DefaultCompleteWithFlags(ctx, cmd)
		return
	}
	w := cmd.Root().Writer
	for _, leaf := range cmd.Commands {
		if leaf.Hidden {
			continue
		}
		printCompletionCandidate(w, leaf.Name, leaf.Usage)
		for _, alias := range leaf.Aliases {
			printCompletionCandidate(w, alias, leaf.Usage)
		}
	}
}

// completeCurrentWordV2 is the single v2-protocol classification of the
// current word for non-leaf ShellComplete callbacks (verb groups and the root
// command): equals-form flag value, space-form flag value, partial flag name,
// or bare word. Flag-value sites route through the full dispatch pipeline
// (enum → live), partial flag names get flag-name candidates; only the
// bare-word branch differs per level, so the caller supplies it (a group
// lists its leaves with aliases, the root lists top-level commands).
func completeCurrentWordV2(ctx context.Context, cmd *ucli.Command, bareWord func(w io.Writer)) {
	w := cmd.Root().Writer
	if strings.HasPrefix(rawCurrentWord, "-") {
		if eq := strings.Index(rawCurrentWord, "="); eq > 0 {
			// Equals-form flag value (e.g. --output=json<TAB>, --context=prod<TAB>):
			// route through the full dispatch pipeline (enum → live), the same as
			// the leaf path does — BUT only when the flag is a recognized global
			// value-taking flag. resolveFlagCanonical falls back to the bare trimmed
			// name for any unknown flag, so without this guard a per-leaf flag like
			// --cluster= would resolve to FlagName:"cluster", get dispatched, and
			// fire a live cluster-list HTTP request before any kind is chosen.
			// An empty Descriptor is passed — enumCandidates and liveCandidates
			// both fall back to globalArgSpecs for global flags, so --output enum
			// and --context live candidates are found without any leaf flags.
			spelling := rawCurrentWord[:eq]
			canonical := resolveFlagCanonical(spelling, globalArgSpecs)
			if flagTakesValue(canonical, globalArgSpecs) {
				site := CompletionSite{
					Kind:         SiteFlagValue,
					FlagName:     canonical,
					FlagSpelling: spelling,
					EqualsForm:   true,
					Partial:      rawCurrentWord[eq+1:],
				}
				tail := stripRoutingPrefixByCmd(rawArgv, cmd)
				for _, cand := range dispatchSite(ctx, site, runtime.Descriptor{}, nil, cmd, tail) {
					_, _ = fmt.Fprintln(w, cand)
				}
			}
			// Per-leaf or bool flag in equals-form at this level: no candidates.
			// The shell falls back to file completion for the value.
			return
		}
		// Partial flag name (e.g. "--cl"): emit flag-name candidates.
		for _, cand := range nameLikeCandidates(cmd, rawCurrentWord) {
			_, _ = fmt.Fprintln(w, cand)
		}
		return
	}
	// Space-form flag value (e.g. `get --output <TAB>`, `rancher -o <TAB>`):
	// if the last token in the arg tail is a value-taking flag, the current
	// word is its partial value — route through the full dispatch pipeline
	// (enum → live), the same as the leaf path does, and skip the bare-word
	// branch. An empty Descriptor is sufficient; enum and live both fall back
	// to globalArgSpecs for global flags.
	tail := stripRoutingPrefixByCmd(rawArgv, cmd)
	if site := ParseArgTail(tail, rawCurrentWord, rawProtoV2, runtime.Descriptor{}, globalArgSpecs); site.Kind == SiteFlagValue {
		for _, cand := range dispatchSite(ctx, site, runtime.Descriptor{}, nil, cmd, tail) {
			_, _ = fmt.Fprintln(w, cand)
		}
		return
	}
	bareWord(w)
}

// printCompletionCandidate writes one completion candidate line using
// formatCandidate (the single format source), followed by a newline.
// Mirrors urfave's own printCommandSuggestions format ("name:usage", or a
// bare name when usage is empty) — unexported in urfave, so reproduced here
// rather than golden-tested against its exact output.
func printCompletionCandidate(w io.Writer, name, usage string) {
	_, _ = fmt.Fprintln(w, formatCandidate(name, usage))
}

// assembleCommands builds the root command list: the top-level commands
// followed by one group command per group in first-appearance order. Leaves in
// the "" group are attached at the top level.
func assembleCommands(topLevel []*ucli.Command, order []string, groups map[string][]*ucli.Command) []*ucli.Command {
	out := append([]*ucli.Command{}, topLevel...)
	for _, g := range order {
		leaves := groups[g]
		if len(leaves) == 0 {
			continue
		}
		if g == "" {
			out = append(out, leaves...)
			continue
		}
		out = append(out, &ucli.Command{Name: g, Usage: groupUsages[g], Commands: leaves, CommandNotFound: commandNotFound, OnUsageError: onUsageError})
	}
	return out
}

// BuildRoot constructs the root command from the declarative entry table. Kind
// validation and resolver composition fail fast; then one loop probes,
// validates, registers, and wires every command. `schema` is a fixed command
// backed by the same registry. Global flags are defined at root so subcommand
// actions can read them (c.Bool("dry-run"), c.String("config"), ...).
func BuildRoot(version string) (*ucli.Command, error) {
	if err := kinds.Validate(); err != nil {
		return nil, fmt.Errorf("kind table: %w", err)
	}
	resolver, err := runtime.ComposeResolvers(provider.NewResolver(), runtime.MapResolver(kinds.Bindings()))
	if err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}
	// A kindOverrideTable row whose del override sits on a Deletable: false
	// kind (D-R1-3) never gets a binding from kinds.Bindings() above — the
	// row is not Deletable, so kinds.Bindings() skips "delete <name>" for it —
	// which would otherwise fail wireEntry's check. Rather than hand-copy
	// each such row's Binding into a literal that could silently drift from
	// an edit to the row, derive one resolver entry per override, read from
	// the row itself at composition time; a Deletable: true row is skipped
	// since kinds.Bindings() already covers its "delete <name>" op, and
	// composing it again here would collide in ComposeResolvers below.
	delOverrides := runtime.MapResolver{}
	for key, ov := range kindOverrideTable {
		// Skip get/patch-only entries before the lookup, same as before this
		// predicate was shared: only a del-bearing entry can possibly need a
		// manual binding, so an invalid key on a get/patch-only override
		// (nothing to do with delete bindings) must not fail BuildRoot here.
		if ov.del == nil {
			continue
		}
		row, ok := kinds.Lookup(key)
		if !ok {
			return nil, fmt.Errorf("kind table: %q kind not found", key)
		}
		if !delOverrideOnNonDeletable(row, ov) {
			continue
		}
		delOverrides["delete "+row.Name] = row.Binding
	}
	resolver, err = runtime.ComposeResolvers(resolver, delOverrides)
	if err != nil {
		return nil, fmt.Errorf("resolver: %w", err)
	}
	reg := runtime.NewRegistry(resolver)
	reg.SetKinds(schemaKinds(kinds.All(), kindOverrides()))

	// support-bundle needs the registry (for its own rancher/schema.json
	// collector) in scope at construction time, so it is wired here rather
	// than in buildEntries — the same reason schema.go's fixed command is
	// built inline below instead of through the declarative table.
	entries := append(buildEntries(), entry{group: "", make: func(d runDeps) runtime.Command {
		return observe.NewSupportBundle(d.clients, func() any { return reg.Schema(version, config.MinRancherVersion) })
	}})

	groups := map[string][]*ucli.Command{}
	sectionCmds := map[string][]string{}
	var order []string
	for _, e := range entries {
		leaf, name, err := wireEntry(reg, resolver, e)
		if err != nil {
			return nil, err
		}
		if _, seen := groups[e.group]; !seen {
			order = append(order, e.group)
		}
		groups[e.group] = append(groups[e.group], leaf)
		sectionCmds[e.group] = append(sectionCmds[e.group], name)
	}

	// schema is a fixed command backed by the registry (no dir/store, no
	// binding). Its read-only resolver is config.ReadOnlyOverride itself —
	// the process-wide stash root's Before hook populates below, using the
	// SAME resolution buildIO reads for every other command.
	schemaCmd := runtime.NewSchemaCommand(reg, version, config.MinRancherVersion, config.ReadOnlyOverride)
	reg.Register(schemaCmd)

	// completion is a fixed command backed by the registry (no dir/store,
	// no binding): reg.Register puts it in the manifest — Adapt alone
	// registers nothing, and the command would silently vanish from
	// `rancher schema` with no test failure.
	completionCmd := commands.NewCompletion()
	reg.Register(completionCmd)

	// The manifest's ordered sections mirror assembleCommands below exactly
	// (SE-18): the fixed top-level commands lead, then each entry group at
	// first appearance — the ungrouped ("") run at its real position — with
	// commands in registration order. Empty groups are skipped, as in
	// assembleCommands.
	sections := []runtime.SchemaGroup{{Name: "", Commands: []string{
		schemaCmd.Describe().Name, completionCmd.Describe().Name,
	}}}
	for _, g := range order {
		if len(sectionCmds[g]) == 0 {
			continue
		}
		sections = append(sections, runtime.SchemaGroup{Name: g, Commands: sectionCmds[g]})
	}
	reg.SetGroups(sections)

	if err := reg.Validate(); err != nil {
		return nil, fmt.Errorf("command registry: %w", err)
	}

	commandsList := assembleCommands([]*ucli.Command{Adapt(reg, schemaCmd), Adapt(reg, completionCmd)}, order, groups)

	// Install aliasAwareGroupComplete on every verb group (any command that
	// has subcommands). For groups whose leaves carry Aliases this lists
	// canonical name + aliases; for others (config, auth, etc.) it lists
	// canonical names only — same as urfave's default, but with the
	// v2-protocol flag-name branch that DefaultCompleteWithFlags lacks (so
	// `config --<TAB>` and `get --<TAB>` both yield flag candidates under v2).
	for _, cmd := range commandsList {
		if len(cmd.Commands) > 0 {
			cmd.ShellComplete = aliasAwareGroupComplete
		}
	}

	rootShellComplete := func(ctx context.Context, c *ucli.Command) {
		// v2 protocol: use rawCurrentWord (stripped from argv), classified by
		// the same shared routing the verb groups use — so root-level flag-value
		// sites (`rancher -o <TAB>`, `rancher --output=<TAB>`) reach the dispatch
		// pipeline instead of the command listing. The bare-word branch keeps
		// the top-level command listing. For v1, fall back to
		// DefaultCompleteWithFlags which reads os.Args directly for the root
		// command (cmd.parent == nil path in DefaultCompleteWithFlags).
		if rawProtoV2 {
			completeCurrentWordV2(ctx, c, func(w io.Writer) {
				for _, cand := range nameLikeCandidates(c, rawCurrentWord) {
					_, _ = fmt.Fprintln(w, cand)
				}
			})
			return
		}
		ucli.DefaultCompleteWithFlags(ctx, c)
	}

	root := &ucli.Command{
		Name:          "rancher",
		ShellComplete: rootShellComplete,
		Usage:         "Rancher CLI",
		Version:       version,
		Flags: append([]ucli.Flag{
			&ucli.BoolFlag{Name: "debug", Usage: "debug logging"},
			&ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to rancher config directory",
				Sources: ucli.EnvVars(config.EnvConfigDir)},
		}, GlobalFlags()...),
		CommandNotFound: commandNotFound,
		OnUsageError:    onUsageError,
		// urfave's auto-appended completion command is renamed and kept hidden
		// machinery only: its subcommands are stripped so it can never emit
		// urfave's own (divergent) scripts — `completion <shell>` (wired below,
		// via Adapt like schema) is the one script-emitting surface.
		EnableShellCompletion:      true,
		ShellCompletionCommandName: "__completion",
		ConfigureShellCompletionCommand: func(c *ucli.Command) {
			c.Hidden = true
			c.Commands = nil
			c.CommandNotFound = commandNotFound
		},
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			if c.Bool("debug") {
				logrus.SetLevel(logrus.DebugLevel)
			}
			config.SetContextOverride(c.String("context"))
			config.SetReadOnlyOverride(resolveReadOnly(c))
			return ctx, nil
		},
		Commands: commandsList,
	}
	return root, nil
}
