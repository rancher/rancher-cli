package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/managementtoken"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/engine/oidcclient"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/user"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/token"
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
	// bare marks commands adaptEntry constructs with only runDeps.version set:
	// it skips resolveDeps/newClientFactory/newFileSource entirely. Two usages
	// exist: commands that need no Rancher config, credential store, or client
	// factory at all (the skill entries), and commands that resolve config/store
	// themselves, lazily, inside Execute on their own schedule (the token shim,
	// which must not let a broken config.yaml block a warm cache hit). bare can
	// never supply a client factory — a command needing one cannot be bare.
	// Schema, completion, and wireEntry validation are unaffected.
	bare bool
	make func(runDeps) runtime.Command
}

// buildEntries is the single source of command wiring: the bespoke config/token
// and create commands, plus generated get/delete commands for every kind in the
// table. Adding a command is one row here (plus a kind-table row for a new
// kind).
func buildEntries() []entry {
	entries := []entry{
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogin(d.dir) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewLogout(d.dir) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return commands.NewAuthCheck(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewTokenCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewListTokensCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewDeleteTokenCommand(d.dir, d.store) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerLs(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewServerSwitch(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigAddServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigUpdateServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigDeleteServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigPath(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigInfo(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewConfigEdit(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return commands.NewSetCompletion(d.dir) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return commands.NewSkillInstall(d.version) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return commands.NewSkillUninstall(d.version) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return commands.NewSkillStatus(d.version) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreate(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreateNamespace(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return projects.NewCreateSecret(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return resources.NewCreate(d.clients, d.src) }},
		{group: "create", make: func(d runDeps) runtime.Command { return extres.NewCreateToken(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return extres.NewCreateKubeconfig(d.clients) }},
		{group: "create", make: func(d runDeps) runtime.Command { return extres.NewCreatePasswordChangeRequest(d.clients, d.src) }},
		{group: "create", make: func(d runDeps) runtime.Command { return user.NewCreate(d.clients, d.src) }},
		{group: "create", make: func(d runDeps) runtime.Command { return oidcclient.NewCreate(d.clients, d.roots, d.serverBase) }},
		{group: "create", make: func(d runDeps) runtime.Command { return provisioning.NewCreateCluster(d.clients, d.roots) }},
		{group: "create", make: func(d runDeps) runtime.Command { return provisioning.NewCreateMachineConfig(d.clients) }},
		{group: "project", make: func(d runDeps) runtime.Command { return projects.NewAddNamespace(d.clients) }},
		{group: "project", make: func(d runDeps) runtime.Command { return projects.NewRemoveNamespace(d.clients) }},
		{group: "oidc-client", make: func(d runDeps) runtime.Command { return oidcclient.NewAddSecret(d.clients) }},
		{group: "oidc-client", make: func(d runDeps) runtime.Command { return oidcclient.NewRotateSecret(d.clients) }},
		{group: "oidc-client", make: func(d runDeps) runtime.Command { return oidcclient.NewRemoveSecret(d.clients) }},
		{group: "cluster", make: func(d runDeps) runtime.Command { return provisioning.NewScale(d.clients) }},
		{group: "cluster", make: func(d runDeps) runtime.Command { return provisioning.NewSetAutoscaling(d.clients) }},
		{group: "cluster", make: func(d runDeps) runtime.Command { return provisioning.NewClearAutoscaling(d.clients) }},
		{group: "cluster", make: func(d runDeps) runtime.Command { return provisioning.NewPauseAutoscaling(d.clients) }},
		{group: "cluster", make: func(d runDeps) runtime.Command { return provisioning.NewResumeAutoscaling(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectCluster(d.clients, d.roots) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewPod(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewDeployment(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectProject(d.clients) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectUser(d.clients) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetKubernetesVersions(d.roots, d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewGrant(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewRevoke(d.clients) }},
		// Bare entries are invoked with zero runDeps (adaptEntry never calls
		// resolveDeps for them), so dir/store are passed explicitly zero here:
		// the shim resolves both lazily in Execute.
		{group: "", bare: true, make: func(runDeps) runtime.Command { return token.NewLegacyTokenCommand("", nil) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewImages(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewUsage(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return commands.NewKubeconfig(d.dir, d.store, d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return observe.NewLogs(d.clients) }},
	}
	ov := kindOverrides()
	for _, k := range kinds.All() {
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
// featureRestartWarning returns the advisory for a feature whose read
// status.dynamic is false: the server must restart to apply the change, and
// until it has come back the next command can fail confusingly.
func featureRestartWarning(orig *unstructured.Unstructured) (string, bool) {
	dynamic, found, _ := unstructured.NestedBool(orig.Object, "status", "dynamic")
	if !found || dynamic {
		return "", false
	}
	return "this feature flag is not dynamic: the server restarts itself to apply it — wait for the server to come back before running more commands", true
}

// primeFeatureCommunityWarning warns that a rendered Feature's VALUE column
// reads enabled — via spec.value or the status.default fallback — only
// because that reading never reflects a community server's own build gate:
// Rancher's own Feature.Enabled() disables any status.prime:true feature on
// a non-Prime build ahead of spec.value/status.default/lockedValue (see
// decideCapability), so the object's own enabled reading is honest about
// what is stored but inert about what the server will do. clients must be
// the concrete client factory that exposes ServerInfo; any other
// implementation (never true in production) gets a no-op enricher. Unknown
// edition never warns — the fact asserted here is "this server is
// confirmed community", never guessed.
func primeFeatureCommunityWarning(clients engine.ClusterClientFactory) resources.GetEnricher {
	f, ok := clients.(*clientFactory)
	if !ok {
		return func(context.Context, *unstructured.Unstructured) (string, bool) { return "", false }
	}
	return func(ctx context.Context, obj *unstructured.Unstructured) (string, bool) {
		enabled, prime := featureState(obj)
		if prime == nil || !*prime || enabled == nil || !*enabled {
			return "", false
		}
		info, err := f.ServerInfo(ctx)
		if err != nil || info.Edition != provider.EditionCommunity {
			return "", false
		}
		name, _, _ := unstructured.NestedString(obj.Object, "metadata", "name")
		return name + " is enabled by default but inactive without Rancher Prime", true
	}
}

var kindOverrideTable = map[string]kindOverride{
	"clusterregistrationtoken": {
		get:      func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewGetCRT(k, d.roots) },
		patch:    func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewRefuseCRTPatch(k) },
		refusals: map[string]string{"patch": "read-only: consumed, not edited"},
	},
	// managementtoken: patch is refused — writes go through Rancher's auth
	// lifecycle; the CLI does not bypass it. Delete is guarded against removing
	// the token backing the current session.
	"managementtoken": {
		patch: func(k kinds.Kind, d runDeps) runtime.Command { return managementtoken.NewRefusePatch(k) },
		del: func(k kinds.Kind, d runDeps) runtime.Command {
			return managementtoken.NewDeleteGuard(k, d.clients, d.sessionNamer)
		},
		refusals: map[string]string{"patch": "writes go through Rancher's auth lifecycle (key generation, TTL clamp, identity derivation); the CLI does not bypass it"},
	},
	// feature: patching a flag whose status.dynamic is false takes effect
	// only after the server restarts itself — on a single-node server that
	// is a real outage window, and the CLI otherwise says nothing. Advisory
	// only, never a gate. get carries the community-edition counterpart: a
	// prime-classified feature that reads enabled is inert there regardless
	// of what the object says.
	"feature": {
		get: func(k kinds.Kind, d runDeps) runtime.Command {
			return resources.NewGet(k, d.clients, primeFeatureCommunityWarning(d.clients))
		},
		patch: func(k kinds.Kind, d runDeps) runtime.Command {
			return resources.NewPatch(k, d.clients, d.src, func(orig, _ *unstructured.Unstructured, eff *safety.Effect) {
				if w, ok := featureRestartWarning(orig); ok {
					eff.Warnings = append(eff.Warnings, w)
				}
			})
		},
	},
	// user: delete adds a cascade warning (Rancher controllers remove the
	// user's role bindings when the user goes) and a self-delete guard that
	// refuses deletion when the target matches the authenticated caller's own
	// user ID (SelfUser probe via the local cluster client).
	"user": {
		del: func(k kinds.Kind, d runDeps) runtime.Command { return rbac.NewDeleteUser(k, d.clients) },
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
//
// "create" verb state: only kinds with NoCreateViaManifest set carry a
// "create" entry (state=refused, note=the restriction message). Kinds without
// the field are omitted from "create" — absence means no reviewed restriction
// is recorded, not that create-resource is guaranteed to accept the kind.
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
		if k.NoCreateViaManifest != "" {
			verbs["create"] = runtime.VerbState{
				State:     runtime.VerbRefused,
				Contract:  string(k.Binding.Contract),
				Stability: string(k.Binding.Stability),
				Note:      k.NoCreateViaManifest,
			}
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
			Prime:            k.Capability.Tier == runtime.CapabilityPrime,
			Verbs:            verbs,
		})
	}
	return out
}

// registeredVerbState describes a verb that HAS a registered command:
// available unless the override marks it a refusal. Contract/stability come from
// the row's own Binding — the same source kinds.Bindings() (and BuildRoot's
// del-override resolver) feed the resolver from.
func registeredVerbState(k kinds.Kind, verb string, ov kindOverride) runtime.VerbState {
	vs := runtime.VerbState{
		State:     runtime.VerbAvailable,
		Contract:  string(k.Binding.Contract),
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
func wireEntry(reg *runtime.Registry, resolver runtime.Resolver, version string, e entry) (*ucli.Command, string, error) {
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
	// config/auth commands carry no contract.
	if d.CommandType != runtime.Local {
		if _, ok := resolver.Resolve(d.Name); !ok {
			return nil, "", fmt.Errorf("engine command %q has no resolver binding", d.Name)
		}
	}

	reg.Register(e.make(runDeps{}))
	return adaptEntry(reg, version, e.bare, e.make), d.Name, nil
}

// groupUsages documents the known verb groups. A group discovered from the
// entries but absent here still gets a (usage-less) group command — command
// reachability never depends on an allowlist.
var groupUsages = map[string]string{
	"auth":        "Auth operations",
	"config":      "Config operations",
	"create":      "Create resources",
	"get":         "Read resources",
	"patch":       "Patch resources",
	"delete":      "Delete resources",
	"inspect":     "Inspect related resources together",
	"project":     "Project namespace operations (membership is managed by 'rancher grant' and 'rancher revoke')",
	"skill":       "Agents-skill installation for coding-agent harnesses",
	"oidc-client": "OIDC client secret lifecycle (create oidc-client creates the client itself)",
	"cluster":     "Cluster scaling and autoscaling (lifecycle is 'create cluster' and 'delete provisioning-cluster')",
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
			// Equals-form flag value (e.g. --output=json<TAB>, --project=p1<TAB>):
			// route through the full dispatch pipeline (enum → live), the same as
			// the leaf path does — BUT only when the flag is a recognized global
			// value-taking flag. resolveFlagCanonical falls back to the bare trimmed
			// name for any unknown flag, so without this guard a per-leaf flag like
			// --cluster= would resolve to FlagName:"cluster", get dispatched, and
			// fire a live cluster-list HTTP request before any kind is chosen.
			// An empty Descriptor is passed — enumCandidates and liveCandidates
			// both fall back to globalArgSpecs for global flags, so --output enum
			// candidates are found without any leaf flags.
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
// derivedOpKinds maps every bespoke op whose surface IS a single kind row's
// resource to that row's key. Their bindings are derived from the rows at
// composition time — never hand-copied literals in provider.NewResolver —
// so an edit to a row's Binding propagates to every op it backs.
// TestCreateOpBindingsTrackKindRows ranges over this same map, so a new
// entry is covered the moment it is added. The namespace mutations ride the
// same core-v1 dynamic path as the generated namespace verbs and are bound
// to that row, not the project verbs they sit beside.
var derivedOpKinds = map[string]string{
	"create project":             "project",
	"create token":               "token",
	"create kubeconfig":          "kubeconfig",
	"create namespace":           "namespace",
	"project add-namespace":      "namespace",
	"project remove-namespace":   "namespace",
	"create secret":              "secret",
	"create cluster":             "provisioningcluster",
	"create oidc-client":         "oidcclient",
	"oidc-client add-secret":     "oidcclient",
	"oidc-client rotate-secret":  "oidcclient",
	"oidc-client remove-secret":  "oidcclient",
	"cluster scale":              "provisioningcluster",
	"cluster set-autoscaling":    "provisioningcluster",
	"cluster clear-autoscaling":  "provisioningcluster",
	"cluster pause-autoscaling":  "provisioningcluster",
	"cluster resume-autoscaling": "provisioningcluster",
}

// bespokePublicOps is the closed, reviewed set of ContractPublic ops with
// no single backing kind row (they span several listed Public API kinds).
// TestPublicContractOpsArePinned consumes it both ways: an op here must
// resolve public, and a public op must be here or back a public kind row —
// so widening the public surface is always a reviewed edit to this file or
// the kind table, never silent.
var bespokePublicOps = map[string]bool{
	"grant":  true,
	"revoke": true,
	// The user-management workflow: the two-resource User+Secret create
	// and the create-only ext.cattle.io PasswordChangeRequest — the same
	// public surface family as the ext token/kubeconfig commands.
	"create user":                    true,
	"create password-change-request": true,
}

// lookupRow resolves a kind-table key to its row, shared by the derived-op
// and delete-override loops so the lookup-and-fail shape and its error
// wording exist once.
func lookupRow(key string) (kinds.Kind, error) {
	row, ok := kinds.Lookup(key)
	if !ok {
		return kinds.Kind{}, fmt.Errorf("kind table: %q kind not found", key)
	}
	return row, nil
}

// composeBindings builds the full op→binding resolver BuildRoot registers:
// the bespoke provider map, the kind table's generated ops, the
// derivedOpKinds ops, and the delete overrides derived from their rows.
// Derivation over hand-copied literals is the rule throughout: an edit to a
// row's Binding propagates to every op that row backs, with nothing to keep
// in sync by hand.
func composeBindings() (runtime.Resolver, error) {
	derivedOps := runtime.MapResolver{}
	for op, key := range derivedOpKinds {
		row, err := lookupRow(key)
		if err != nil {
			return nil, err
		}
		derivedOps[op] = row.Binding
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
		row, err := lookupRow(key)
		if err != nil {
			return nil, err
		}
		if !delOverrideOnNonDeletable(row, ov) {
			continue
		}
		delOverrides["delete "+row.Name] = row.Binding
	}
	// One fold over every source, one error-wrap site: adding a fifth
	// source is one element here, not another compose-and-wrap copy.
	resolver := runtime.Resolver(runtime.MapResolver(provider.NewResolver()))
	for _, src := range []runtime.Resolver{
		runtime.MapResolver(kinds.Bindings()),
		derivedOps,
		delOverrides,
	} {
		var err error
		resolver, err = runtime.ComposeResolvers(resolver, src)
		if err != nil {
			return nil, fmt.Errorf("resolver: %w", err)
		}
	}
	return resolver, nil
}

// wiredRegistry is the validated command registry and its adapted CLI
// surface as built by buildWiredRegistry. Callers pull fields by name to
// avoid positional confusion across the former five return values.
type wiredRegistry struct {
	reg    *runtime.Registry
	fixed  []*ucli.Command
	groups map[string][]*ucli.Command
	order  []string
}

// buildWiredRegistry constructs and validates the command registry for the
// full surface. It handles kind validation, resolver composition, entry wiring,
// and the fixed schema/completion registrations.
//
// readOnly is wired into the schema command; pass config.ReadOnlyOverride for
// the real CLI path and nil for callers that only need the manifest shape.
func buildWiredRegistry(version string, readOnly func() (bool, []string)) (wiredRegistry, error) {
	if err := kinds.Validate(); err != nil {
		return wiredRegistry{}, fmt.Errorf("kind table: %w", err)
	}
	resolver, err := composeBindings()
	if err != nil {
		return wiredRegistry{}, err
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
		leaf, name, err := wireEntry(reg, resolver, version, e)
		if err != nil {
			return wiredRegistry{}, err
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
	schemaCmd := runtime.NewSchemaCommand(reg, version, config.MinRancherVersion, readOnly)
	reg.Register(schemaCmd)

	// completion is a fixed command backed by the registry (no dir/store,
	// no binding): reg.Register puts it in the manifest — Adapt alone
	// registers nothing, and the command would silently vanish from
	// `rancher schema` with no test failure.
	completionCmd := commands.NewCompletion()
	reg.Register(completionCmd)

	// groupDescs carries the operator-facing desc for each named group.
	// Unnamed ("") sections are positional bookmarks — no desc.
	groupDescs := map[string]string{
		"auth":   "Authenticate with a Rancher server, manage API tokens, and inspect the session.",
		"config": "Manage local CLI configuration: server list, active context, credential store, editor, and shell completion.",
		"skill":  "Install, remove, and inspect agent skills packaged with this CLI.",
		"create": "Create Rancher resources. Most sub-commands expose curated flags for one resource type; `create resource -f <file|->` is the generic path for kinds the kind table knows (one document per invocation; a few kinds require their curated command and say so when rejected).",
		// "project" is both a command group and a kind. Resolution order is
		// command → group → kind (revised 2026-08-20), so rancher schema
		// project returns THIS group's card carrying a kind-view hint, and
		// --kind reaches the kind card. The collision is deliberate and
		// allowlisted (TestSchemaGroupKindCollisionsAreDeliberate): renaming
		// the group would churn the TOC section layout and every agent
		// prompt that references it.
		"project":     "Manage project namespaces: add or remove downstream namespaces from a Rancher project.",
		"oidc-client": "Manage OIDC client secrets for Rancher's embedded OIDC provider; `create oidc-client` creates the client itself.",
		// "cluster" is both a command group and a management KIND name —
		// the same deliberate collision "project" already carries (see
		// TestSchemaGroupKindCollisionsAreDeliberate), covered by the same
		// allowlist. Lifecycle CRUD (`create cluster`, `delete
		// provisioning-cluster`, `get cluster`) stays where it is; `get
		// provisioning-cluster` is the read path for this family's target.
		"cluster": "Scale a machine pool or manage cluster-autoscaler settings on a provisioning cluster; " +
			"lifecycle is `create cluster` and `delete provisioning-cluster`, reads are `get provisioning-cluster`.",
		"inspect": "Deep-inspect a cluster, workload, or project for live state, events, and related objects.",
		"get":     "Fetch one resource by name, or list all. Generated for every kind the kind table knows; `get cloud-credential` and `get kubernetes-versions` are curated commands.",
		"patch":   "Apply a partial update to a resource using a diff file or stdin. Generated for every kind the kind table knows.",
		"delete":  "Delete a resource by name. Generated for kinds that support deletion; some kinds add guards or cascade warnings.",
	}

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
		sections = append(sections, runtime.SchemaGroup{Name: g, Desc: groupDescs[g], Commands: sectionCmds[g]})
	}
	reg.SetGroups(sections)

	if err := reg.Validate(); err != nil {
		return wiredRegistry{}, fmt.Errorf("command registry: %w", err)
	}
	return wiredRegistry{
		reg:    reg,
		fixed:  []*ucli.Command{Adapt(reg, schemaCmd), Adapt(reg, completionCmd)},
		groups: groups,
		order:  order,
	}, nil
}

func BuildRoot(version string) (*ucli.Command, error) {
	wr, err := buildWiredRegistry(version, config.ReadOnlyOverride)
	if err != nil {
		return nil, err
	}

	commandsList := assembleCommands(wr.fixed, wr.order, wr.groups)

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
			// --debug is accepted and currently a no-op.
			&ucli.BoolFlag{Name: "debug", Usage: "accepted; currently no effect"},
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
			config.SetReadOnlyOverride(resolveReadOnly(c))
			return ctx, nil
		},
		Commands: commandsList,
	}
	return root, nil
}
