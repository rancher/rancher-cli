package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	ucli "github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/commands/auth"
	"github.com/rancher/rancher-cli/internal/commands/configcmd"
	"github.com/rancher/rancher-cli/internal/commands/skill"
	"github.com/rancher/rancher-cli/internal/commands/telemetrycmd"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/extres"
	"github.com/rancher/rancher-cli/internal/engine/inspect"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/kubeconfig"
	"github.com/rancher/rancher-cli/internal/engine/managementtoken"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/engine/oidcclient"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/provisioning"
	"github.com/rancher/rancher-cli/internal/engine/rbac"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/user"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/token"
)

// entry is one command's declarative registration: its verb group and a single
// constructor taking runDeps. The constructor pulls whatever it needs (client
// factory, file source, config dir/store) off runDeps and ignores the rest, so
// adding a command is one row here - the compiler enforces the dependency set,
// with no per-shape adapter or wiring switch.
type entry struct {
	group string
	// bare marks commands adaptEntry constructs with only runDeps.version set:
	// it skips resolveDeps/newClientFactory/newFileSource entirely. Two usages
	// exist: commands that need no Rancher config, credential store, or client
	// factory at all (the skill entries), and commands that resolve config/store
	// themselves, lazily, inside Execute on their own schedule (the token shim,
	// which must not let a broken config.yaml block a warm cache hit). bare can
	// never supply a client factory - a command needing one cannot be bare.
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
		{group: "auth", make: func(d runDeps) runtime.Command { return auth.NewLogin(d.dir) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return auth.NewLogout(d.dir) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return auth.NewCheck(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewTokenCommand(d.dir, d.store, d.anchor) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewListTokensCommand(d.dir, d.store) }},
		{group: "auth", make: func(d runDeps) runtime.Command { return token.NewDeleteTokenCommand(d.dir, d.store) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewServerLs(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewServerSwitch(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigAddServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigUpdateServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigDeleteServer(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigPath(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigInfo(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewConfigEdit(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewSetCompletion(d.dir) }},
		{group: "config", make: func(d runDeps) runtime.Command { return configcmd.NewSetTokenReuse(d.dir) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return skill.NewInstall(d.version) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return skill.NewUninstall(d.version) }},
		{group: "skill", bare: true, make: func(d runDeps) runtime.Command { return skill.NewStatus(d.version) }},
		{group: "telemetry", make: func(d runDeps) runtime.Command { return telemetrycmd.NewStatus(d.dir, d.experiments) }},
		{group: "telemetry", make: func(d runDeps) runtime.Command { return telemetrycmd.NewOn(d.dir) }},
		{group: "telemetry", make: func(d runDeps) runtime.Command { return telemetrycmd.NewOff(d.dir) }},
		{group: "telemetry", make: func(d runDeps) runtime.Command { return telemetrycmd.NewReset(d.dir) }},
		{group: "telemetry", make: func(d runDeps) runtime.Command { return telemetrycmd.NewShow(d.dir) }},
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
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectProject(d.clients, d.roots) }},
		{group: "inspect", make: func(d runDeps) runtime.Command { return inspect.NewInspectUser(d.clients, d.roots) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewCan(d.clients, nil) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewList(d.clients, nil) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewWhoCan(d.clients, nil) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewExplain(d.clients, nil) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewClusters(d.clients, d.roots, nil) }},
		{group: "access", make: func(d runDeps) runtime.Command { return access.NewProjects(d.clients, d.roots, nil) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetCloudCredential(d.clients) }},
		{group: "get", make: func(d runDeps) runtime.Command { return provisioning.NewGetKubernetesVersions(d.roots, d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewGrant(d.clients) }},
		{group: "", make: func(d runDeps) runtime.Command { return rbac.NewRevoke(d.clients) }},
		// Bare entries are invoked with zero runDeps (adaptEntry never calls
		// resolveDeps for them), so dir/store are passed explicitly zero here:
		// the shim resolves both lazily in Execute.
		{group: "", bare: true, make: func(runDeps) runtime.Command { return token.NewLegacyTokenCommand("", nil, nil) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewImages(d.clients, d.roots) }},
		{group: "", make: func(d runDeps) runtime.Command { return inspect.NewUsage(d.clients, d.roots) }},
		{group: "", make: func(d runDeps) runtime.Command { return kubeconfig.NewCommand(d.dir, d.store, d.clients, d.roots) }},
		{group: "", make: func(d runDeps) runtime.Command { return observe.NewLogs(d.clients, d.roots) }},
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
// still gets the override's command - that is how provisioningcluster
// delete ships guarded without a bare Deletable flip.
type kindOverride struct {
	get   func(k kinds.Kind, d runDeps) runtime.Command
	patch func(k kinds.Kind, d runDeps) runtime.Command
	del   func(k kinds.Kind, d runDeps) runtime.Command
	// refusals marks override commands that are pure refusals (registered,
	// but declining at Plan), keyed by verb; the note is surfaced as the
	// verb's state in the schema manifest. A verb listed here must
	// also have its constructor set above.
	refusals map[string]string
}

// delOverrideOnNonDeletable reports whether ov installs a del override for a
// kind whose own row stays Deletable: false - the one combination both
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
// clusterregistrationtoken: get is a Steve /v1 read (the
// only place the real token is spliced into the command/manifest fields);
// patch is refused outright - the row is read-only surface.
// featureRestartWarning returns the advisory for a feature whose read
// status.dynamic is false: the server must restart to apply the change, and
// until the server is available again the next command can fail confusingly.
func featureRestartWarning(orig *unstructured.Unstructured) (string, bool) {
	dynamic, found, _ := unstructured.NestedBool(orig.Object, "status", "dynamic")
	if !found || dynamic {
		return "", false
	}
	return "this feature flag is not dynamic: Rancher restarts to apply it - wait until the server is available again before running more commands", true
}

// primeFeatureCommunityWarning warns that a rendered Feature's VALUE column
// reads enabled - via spec.value or the status.default fallback - only
// because that reading never reflects a community server's own build gate:
// Rancher's own Feature.Enabled() disables any status.prime:true feature on
// a non-Prime build ahead of spec.value/status.default/lockedValue (see
// decideCapability), so the object's own enabled reading is honest about
// what is stored but inert about what the server will do. clients must be
// the concrete client factory that exposes ServerInfo; any other
// implementation (never true in production) gets a no-op enricher. Unknown
// edition never warns - the fact asserted here is "this server is
// confirmed community", never guessed.
func primeFeatureCommunityWarning(clients engine.ClusterClientFactory) resources.GetEnricher {
	f, ok := clients.(*clientFactory)
	if !ok {
		return func(context.Context, *unstructured.Unstructured) (string, bool) { return "", false }
	}
	return func(ctx context.Context, obj *unstructured.Unstructured) (string, bool) {
		enabled, prime := featureState(obj)
		if enabled == nil || !*enabled {
			return "", false
		}
		info, err := f.ServerInfo(ctx)
		if err != nil || !primeInertOnCommunity(prime, info.Edition) {
			return "", false
		}
		name, _, _ := unstructured.NestedString(obj.Object, "metadata", "name")
		return name + " is enabled on this server but inactive without Rancher Prime", true
	}
}

var kindOverrideTable = map[string]kindOverride{
	"clusterregistrationtoken": {
		get:      func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewGetCRT(k, d.roots) },
		patch:    func(k kinds.Kind, d runDeps) runtime.Command { return provisioning.NewRefuseCRTPatch(k) },
		refusals: map[string]string{"patch": "read-only: the CLI only reads it, never edits it"},
	},
	// managementtoken: patch is refused - Rancher sets the key, the TTL and the
	// user of a token itself; the CLI does not change them. Delete is guarded
	// against removing the token backing the current session.
	"managementtoken": {
		patch: func(k kinds.Kind, d runDeps) runtime.Command { return managementtoken.NewRefusePatch(k) },
		del: func(k kinds.Kind, d runDeps) runtime.Command {
			return managementtoken.NewDeleteGuard(k, d.clients, d.sessionNamer)
		},
		refusals: map[string]string{"patch": "Rancher sets these token fields itself (the key, a TTL within the server's maximum TTL, and the user the token belongs to); the CLI does not change them"},
	},
	// feature: patching a flag whose status.dynamic is false takes effect
	// only after Rancher restarts - on a single-node server that is a real
	// outage window, and the CLI otherwise says nothing. Advisory only,
	// never a gate. get carries the community-edition counterpart: a
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
	// project: delete lists the namespaces Rancher deletes and keeps, and
	// --delete-namespaces deletes them with the project.
	"project": {
		del: func(k kinds.Kind, d runDeps) runtime.Command {
			return resources.NewDeleteProject(k, d.clients, d.roots)
		},
	},
	// provisioningcluster: patch keeps the generic
	// diff/redaction/RV-precondition behavior - only an advisory etcd-quorum
	// preview is added, via the EffectEnricher seam, never a gate.
	"provisioningcluster": {
		patch: func(k kinds.Kind, d runDeps) runtime.Command {
			return resources.NewPatch(k, d.clients, d.src, func(orig, patched *unstructured.Unstructured, eff *safety.Effect) {
				if delta, ok := provisioning.ComputeQuorumDelta(orig, patched); ok {
					eff.Warnings = append(eff.Warnings, delta.Note)
				}
			})
		},
		// del: the row stays Deletable: false (no generic
		// verb would be generated), but the override still supplies a
		// bespoke, guarded delete command. See
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
// seam - buildEntries calls this, never the var directly - so later tasks
// (and tests) have exactly one place to swap the table.
func kindOverrides() map[string]kindOverride {
	return kindOverrideTable
}

// schemaKinds builds the manifest's kind rows with the effective verb
// surface from the same two sources buildEntries generates commands
// from - the kind table and the override registry - so the manifest cannot
// drift from what is actually registered: a refusals entry surfaces as
// "refused"; a del override on a Deletable: false row surfaces delete as
// "available" (the provisioningcluster case); a delete neither the row nor
// an override generates is "absent", carrying the row's NoDeleteReason.
//
// "create" verb state: only kinds with NoCreateViaManifest set carry a
// "create" entry (state=refused, note=the restriction message). Kinds without
// the field are omitted from "create" - absence means no reviewed restriction
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
// the row's own Binding - the same source kinds.Bindings() (and BuildRoot's
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
// section). It probes Describe() with zero deps (Describe must be
// dir/store/client-independent), checks the name's group matches the declared
// group, requires every engine command - read or mutating (CommandType !=
// Local) - to resolve to a binding, registers the probe, and wires it through
// the single run adapter.
func wireEntry(reg *runtime.Registry, resolver runtime.Resolver, version string, e entry) (*ucli.Command, string, error) {
	if e.make == nil {
		return nil, "", fmt.Errorf("internal error: the entry in group %q has no function that creates its command", e.group)
	}
	d := e.make(runDeps{}).Describe()

	// The tree is two levels deep: a command name is exactly one part (a
	// top-level command) or two (group + leaf). Reject 3 or more parts with a
	// clear message rather than a misleading group mismatch.
	if n := strings.Count(d.Name, " "); n > 1 {
		return nil, "", fmt.Errorf("internal error: command name %q must have one or two space-separated parts, has %d", d.Name, n+1)
	}
	if g := groupName(d.Name); g != e.group {
		return nil, "", fmt.Errorf("internal error: command %q is listed under group %q but its name belongs to group %q", d.Name, e.group, g)
	}
	// Engine commands (read or mutating) must resolve to a binding; Local
	// config/auth commands carry no contract.
	if d.CommandType != runtime.Local {
		if _, ok := resolver.Resolve(d.Name); !ok {
			return nil, "", fmt.Errorf("internal error: command %q has no contract and stability set", d.Name)
		}
	}

	reg.Register(e.make(runDeps{}))
	return adaptEntry(reg, version, e.bare, e.make), d.Name, nil
}

// groupUsages documents the known verb groups. A group discovered from the
// entries but absent here still gets a (usage-less) group command - command
// reachability never depends on an allowlist.
var groupUsages = map[string]string{
	"auth":        "Auth commands",
	"config":      "Config commands",
	"create":      "Create resources",
	"get":         "Get resources",
	"patch":       "Patch resources",
	"delete":      "Delete resources",
	"inspect":     "Inspect a cluster, workload or project with its related objects",
	"project":     "Project namespace commands (project membership is managed with `rancher grant` and `rancher revoke`)",
	"skill":       "Install, remove and check the agent skills that come with rancher-cli",
	"telemetry":   "Local usage recording: status, turn on, turn off, delete and list events",
	"oidc-client": "Manage OIDC client secrets (`rancher create oidc-client` creates the client itself)",
	"cluster":     "Scale a machine pool or manage cluster-autoscaler settings on a provisioning cluster (create one with `rancher create cluster` and delete it with `rancher delete provisioning-cluster`)",
	"access":      "Which clusters a subject can reach, which projects it can see into, and what it can do, as the server answers (never inferred from bindings)",
}

// aliasAwareGroupComplete is a verb group's own ShellComplete. urfave's
// DefaultCompleteWithFlags lists only each leaf's canonical Name
// (printCommandSuggestions), so `po<TAB>` under `get` would complete only
// toward the canonical "pod" form; this lists each leaf's Aliases as well.
//
// rawCurrentWord is classified the same way the leaf path does it
// (ParseArgTail): equals-form flag values, partial flag names, and bare words
// are each routed to the appropriate candidate generator. The classification
// lives in completeCurrentWord, shared with the root command's own
// ShellComplete; only the bare-word branch - the alias-aware subcommand
// listing - is supplied here.
func aliasAwareGroupComplete(ctx context.Context, cmd *ucli.Command) {
	completeCurrentWord(ctx, cmd, func(w io.Writer) {
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
}

// completeCurrentWord is the single classification of the current word for
// non-leaf ShellComplete callbacks (verb groups and the root command):
// equals-form flag value, space-form flag value, partial flag name, or bare
// word. Flag-value sites get the static enum values of the global flags (no
// request: a live source for a global flag, such as the project list, needs a
// cluster, and the only way to name one is a leaf flag that a root or group
// rejects), partial flag names get flag-name candidates; only the bare-word
// branch differs per level, so the caller supplies it (a group lists its
// leaves with aliases, the root lists top-level commands).
func completeCurrentWord(ctx context.Context, cmd *ucli.Command, bareWord func(w io.Writer)) {
	// A flag this level does not accept, or a word that names no command,
	// sits between the command path and the word being completed: the
	// command rejects the line whatever is completed next (a command name,
	// a flag or a flag's value), so offer nothing.
	if lineRejected(cmd, rawArgv, 0) {
		return
	}
	w := cmd.Root().Writer
	if strings.HasPrefix(rawCurrentWord, "-") {
		if eq := strings.Index(rawCurrentWord, "="); eq > 0 {
			// Equals-form flag value (e.g. --output=json<TAB>): the static enum
			// values, as the leaf path offers - but only for a recognized global
			// value-taking flag. An empty Descriptor is passed: enumCandidates
			// falls back to globalArgSpecs, so --output enum candidates are
			// found without any leaf flags.
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
				for _, cand := range enumCandidates(site, runtime.Descriptor{}) {
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
	// word is its partial value - offer its static enum values, the same as
	// the leaf path does, and skip the bare-word branch. An empty Descriptor
	// is sufficient; the enum falls back to globalArgSpecs for global flags.
	tail := stripRoutingPrefixByCmd(rawArgv, cmd)
	if site := ParseArgTail(tail, rawCurrentWord, runtime.Descriptor{}, globalArgSpecs); site.Kind == SiteFlagValue {
		for _, cand := range enumCandidates(site, runtime.Descriptor{}) {
			_, _ = fmt.Fprintln(w, cand)
		}
		return
	}
	bareWord(w)
}

// lineRejected reports whether the words typed before the current one make
// a line the command rejects whatever is completed next. argv is the
// command line after the program name, and cmd the command it reaches. In
// the words after cmd's path: a flag cmd does not accept (urfave fails
// with "flag provided but not defined"), more than maxWords words that are
// neither a flag nor a flag's value, or a word typed after a bool flag
// that accepts it as its value (lineRefusal refuses it in any position).
// Anywhere on the line: a bool flag typed with "=" and an empty value
// (`--dry-run=`) or one of only whitespace, which lineRefusal refuses, a
// second instance of a flag that may be given once (urfave fails with
// "can't duplicate this flag"), and a bare "-": a line with a word after it
// is refused before the command runs, and the word being completed would be
// that word. maxWords is 0 at the root or a group (urfave enters a
// subcommand that word names, so it names none) and the number of
// positional arguments a leaf declares. A rejected line is offered nothing,
// and no request is made for it.
func lineRejected(cmd *ucli.Command, argv []string, maxWords int) bool {
	words, _, undefined := tailWords(stripRoutingPrefixByCmd(argv, cmd), levelFlags(cmd))
	if undefined || len(words) > maxWords {
		return true
	}
	head, typed := scanLine(argv, cmd)
	if slices.ContainsFunc(head, isEmptyBool) || slices.ContainsFunc(typed, isEmptyBool) {
		return true
	}
	if hasBareDash(head) || hasBareDash(typed) {
		return true
	}
	once := onceFlagNames(cmd)
	given := map[string]bool{}
	for _, w := range slices.Concat(head, typed) {
		if name, ok := once[w.name]; ok && w.kind == flagWord {
			if given[name] {
				return true
			}
			given[name] = true
		}
	}
	for i := range typed {
		if isBoolValue(typed, i) {
			return true
		}
	}
	return false
}

// printCompletionCandidate writes one completion candidate line using
// formatCandidate (the single format source), followed by a newline.
// Mirrors urfave's own printCommandSuggestions format ("name:usage", or a
// bare name when usage is empty) - unexported in urfave, so reproduced here
// rather than golden-tested against its exact output.
func printCompletionCandidate(w io.Writer, name, usage string) {
	_, _ = fmt.Fprintln(w, formatCandidate(name, usage))
}

// groupPrimeMetadata flags a group Prime when every one of its leaves is -
// an all-Prime group carries the same marker as its members do, a mixed one
// carries none. It reads the leaves' own metadata rather than the schema
// manifest on purpose: the help rows under this group row are rendered from
// that same metadata, so the group row and its rows agree by construction,
// with no second source that could drift from what the rows show. The schema
// renderer's groupAllPrime states the same rule over manifest commands, and
// TestPrimeMarkerConformance asserts the two agree on the real manifest.
func groupPrimeMetadata(leaves []*ucli.Command) map[string]any {
	if len(leaves) == 0 {
		return nil
	}
	for _, leaf := range leaves {
		if !commandIsPrime(leaf) {
			return nil
		}
	}
	return map[string]any{primeMetadataKey: true}
}

// groupCommandAction is the Action of the root and of every group command.
// A group has no work of its own: run with no word it shows its help card,
// and with a first word that names no subcommand, an empty word included,
// it hands the word to showCommandHelp, which reports an unknown command.
// urfave's default action skips that lookup for an empty word and shows the
// card with exit 0, which would answer an unset shell variable with help
// and success.
func groupCommandAction(ctx context.Context, c *ucli.Command) error {
	switch {
	case c.Args().Present():
		return showCommandHelp(ctx, c, c.Args().First())
	case c == c.Root():
		return ucli.ShowRootCommandHelp(c)
	default:
		return ucli.ShowSubcommandHelp(c)
	}
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
		// With StopOnNthArg 1 the group parses no flag after its first word:
		// a known subcommand parses the rest itself, and an unknown first
		// word is reported before any flag typed after it.
		out = append(out, &ucli.Command{Name: g, Usage: groupUsages[g], Commands: leaves, Metadata: groupPrimeMetadata(leaves), CommandNotFound: commandNotFound, OnUsageError: onUsageError, StopOnNthArg: new(1), Action: groupCommandAction})
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
// composition time - never hand-copied literals in provider.NewResolver -
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
// TestPublicContractOpsAreListed consumes it both ways: an op here must
// resolve public, and a public op must be here or back a public kind row -
// so widening the public surface is always a reviewed edit to this file or
// the kind table, never silent.
var bespokePublicOps = map[string]bool{
	"grant":  true,
	"revoke": true,
	// The user-management workflow: the two-resource User+Secret create
	// and the create-only ext.cattle.io PasswordChangeRequest - the same
	// public surface family as the ext token/kubeconfig commands.
	"create user":                    true,
	"create password-change-request": true,
	// The authorization reviews are a public Kubernetes API with no kind
	// row to back them: a review is posted, never listed or deleted.
	"access can":     true,
	"access list":    true,
	"access who-can": true,
	"access explain": true,
	// access clusters reads the steve cluster collection and posts one
	// SelfSubjectReview per cluster: reads and reviews, no kind row.
	"access clusters": true,
	// access projects adds the steve project and namespace collections and
	// one SelfSubjectAccessReview per project namespace: reads and reviews.
	"access projects": true,
}

// lookupRow resolves a kind-table key to its row, shared by the derived-op
// and delete-override loops so the lookup-and-fail shape and its error
// wording exist once.
func lookupRow(key string) (kinds.Kind, error) {
	row, ok := kinds.Lookup(key)
	if !ok {
		return kinds.Kind{}, fmt.Errorf("internal error: kind %q not found", key)
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
	// kind never gets a binding from kinds.Bindings() above - the
	// row is not Deletable, so kinds.Bindings() skips "delete <name>" for it -
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
			return nil, fmt.Errorf("internal error: command contracts: %w", err)
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

// checkKindTable runs the kind table's own validation and gives its failure
// the "internal error: " lead, once.
func checkKindTable(validate func() error) error {
	if err := validate(); err != nil {
		return fmt.Errorf("internal error: kind definitions: %w", err)
	}
	return nil
}

// buildWiredRegistry constructs and validates the command registry for the
// full surface. It handles kind validation, resolver composition, entry wiring,
// and the fixed schema/completion registrations.
//
// readOnly is wired into the schema command; pass config.ReadOnlyOverride for
// the real CLI path and nil for callers that only need the manifest shape.
func buildWiredRegistry(version string, readOnly func() (bool, []string)) (wiredRegistry, error) {
	if err := checkKindTable(kinds.Validate); err != nil {
		return wiredRegistry{}, err
	}
	resolver, err := composeBindings()
	if err != nil {
		return wiredRegistry{}, err
	}
	reg := runtime.NewRegistry(resolver)
	reg.SetKinds(schemaKinds(kinds.All(), kindOverrides()))

	// support-bundle needs the registry (for its own rancher/schema.json
	// collector) in scope at construction time, so it is wired here rather
	// than in buildEntries - the same reason schema.go's fixed command is
	// built inline below instead of through the declarative table.
	entries := append(buildEntries(), entry{group: "", make: func(d runDeps) runtime.Command {
		return observe.NewSupportBundle(d.clients, d.roots, func() any { return reg.Schema(version, config.MinRancherVersion) })
	}})

	groups := map[string][]*ucli.Command{}
	sectionCmds := map[string][]string{}
	var order []string
	// leaves and completers feed the live completion setup check below:
	// every adapted command's descriptor, and the source of every command
	// that owns live completion, taken from the same zero-dependency
	// probe adaptEntry installs the TAB callback on.
	leaves := map[string]runtime.Descriptor{}
	completers := map[string]completion.Source{}
	targets := map[string]bool{}
	for _, e := range entries {
		leaf, name, err := wireEntry(reg, resolver, version, e)
		if err != nil {
			return wiredRegistry{}, err
		}
		probe := e.make(runDeps{})
		leaves[name] = probe.Describe()
		if lc, ok := probe.(completion.Completer); ok {
			completers[name] = lc.LiveSource()
			_, targets[name] = lc.(completionTargeter)
		}
		if _, seen := groups[e.group]; !seen {
			order = append(order, e.group)
		}
		groups[e.group] = append(groups[e.group], leaf)
		sectionCmds[e.group] = append(sectionCmds[e.group], name)
	}

	// schema is a fixed command backed by the registry (no dir/store, no
	// binding). Its read-only resolver is config.ReadOnlyOverride itself -
	// the process-wide stash root's Before hook populates below, using the
	// SAME resolution buildIO reads for every other command.
	schemaCmd := runtime.NewSchemaCommand(reg, version, config.MinRancherVersion, readOnly)
	reg.Register(schemaCmd)

	// completion is a fixed command backed by the registry (no dir/store,
	// no binding): reg.Register puts it in the manifest - Adapt alone
	// registers nothing, and the command would silently vanish from
	// `rancher schema` with no test failure.
	completion := newCompletion()
	reg.Register(completion)

	// groupDescs carries the operator-facing desc for each named group.
	// Unnamed ("") sections are positional bookmarks - no desc.
	groupDescs := map[string]string{
		"auth":   "Authenticate with a Rancher server, manage API tokens, and inspect the session.",
		"config": "Manage local CLI configuration: server list, active context, credential store, editor, and shell completion.",
		"skill":  "Install, remove, and check the agent skills that come with rancher-cli.",
		"telemetry": "Control and inspect local usage recording: status, turn on, turn off, delete and list events. " +
			"Recorded events stay on this machine.",
		"create": "Create Rancher resources. Most commands in this group have their own flags for one kind; `rancher create resource -f <file|->` creates any kind `rancher schema` lists (one document per run; a few kinds must be created with their dedicated command, and the refusal names it).",
		// "project" is both a command group and a kind. Resolution order is
		// command → group → kind, so rancher schema
		// project returns THIS group's card carrying a kind-view hint, and
		// --kind reaches the kind card. The collision is deliberate and
		// allowlisted (TestSchemaGroupKindCollisionsAreDeliberate): renaming
		// the group would churn the TOC section layout and every agent
		// prompt that references it.
		"project":     "Manage a project's namespaces: add namespaces of the project's cluster to a Rancher project, or remove them from it.",
		"oidc-client": "Manage OIDC client secrets for Rancher's embedded OIDC provider; `rancher create oidc-client` creates the client itself.",
		// "cluster" is both a command group and a management KIND name -
		// the same deliberate collision "project" already carries (see
		// TestSchemaGroupKindCollisionsAreDeliberate), covered by the same
		// allowlist. Lifecycle CRUD (`create cluster`, `delete
		// provisioning-cluster`, `get cluster`) stays where it is; `get
		// provisioning-cluster` is the read path for this family's target.
		"cluster": "Scale a machine pool or manage cluster-autoscaler settings on a provisioning cluster; " +
			"to create or delete one use `rancher create cluster` and `rancher delete provisioning-cluster`, and to read one use `rancher get provisioning-cluster`.",
		"inspect": "Show the current state, events and related objects of a cluster, workload or project.",
		"access": "Questions about access, answered by the server: whether a subject can perform a verb on a resource, the rules a subject has in a namespace, " +
			"which of the subjects named in the cluster's bindings the authorizer allows to perform a verb on a resource, why the authorizer decided as it did for one subject " +
			"(the chain of Rancher bindings and templates behind the binding it accepted), which clusters Rancher's proxy lets a subject into, " +
			"with the management bindings that grant that access, and which projects a subject can see into (the authorizer's answer to getting each " +
			"namespace of the project, summarized per project), kept apart from the ProjectRoleTemplateBindings behind them. " +
			"The authorizer questions are answered through the Rancher proxy by SubjectAccessReview, SelfSubjectAccessReview or SelfSubjectRulesReview; " +
			"which clusters a subject can reach is the proxy's own answer; the bindings are only read, and the client never evaluates them.",
		"get":    "Get one resource by name, or list all of a kind. Generated for every kind `rancher schema` lists; `rancher get cloud-credential` and `rancher get kubernetes-versions` are dedicated commands.",
		"patch":  "Apply a partial update to a resource from a patch file or stdin. Generated for every kind `rancher schema` lists.",
		"delete": "Delete a resource by name. Generated for kinds that support deletion; some kinds add extra checks or cascade warnings.",
	}

	// The manifest's ordered sections mirror assembleCommands below exactly:
	// the fixed top-level commands lead, then each entry group at
	// first appearance - the ungrouped ("") run at its real position - with
	// commands in registration order. Empty groups are skipped, as in
	// assembleCommands.
	sections := []runtime.SchemaGroup{{Name: "", Commands: []string{
		schemaCmd.Describe().Name, completion.Describe().Name,
	}}}
	for _, g := range order {
		if len(sectionCmds[g]) == 0 {
			continue
		}
		sections = append(sections, runtime.SchemaGroup{Name: g, Desc: groupDescs[g], Commands: sectionCmds[g]})
	}
	reg.SetGroups(sections)

	if err := validateWiring(reg, leaves, completers, targets, flagSources, kindSources, positionalKinds); err != nil {
		return wiredRegistry{}, err
	}
	return wiredRegistry{
		reg:    reg,
		fixed:  []*ucli.Command{Adapt(reg, schemaCmd), Adapt(reg, completion)},
		groups: groups,
		order:  order,
	}, nil
}

// validateWiring runs the checks of a built command tree: the registry's own
// rules, then the live completion tables and the completion targets. Each
// failure is a defect of the CLI and leads with "internal error: ".
func validateWiring(reg *runtime.Registry, leaves map[string]runtime.Descriptor, completers map[string]completion.Source, targets map[string]bool,
	flags, kindTable map[string]completion.Source, positional map[string]string) error {
	if err := reg.Validate(); err != nil {
		return fmt.Errorf("internal error: command definitions: %w", err)
	}
	if err := validateLiveWiring(leaves, completers, flags, kindTable, positional); err != nil {
		return fmt.Errorf("internal error: live completion setup: %w", err)
	}
	if err := validateCompletionTargets(leaves, completers, targets); err != nil {
		return fmt.Errorf("internal error: live completion setup: %w", err)
	}
	return nil
}

func BuildRoot(version string) (*ucli.Command, error) {
	wr, err := buildWiredRegistry(version, config.ReadOnlyOverride)
	if err != nil {
		return nil, err
	}
	return wr.root(version), nil
}

// root assembles the root command around the wired commands. Each call
// returns new root and group commands over the same leaves: urfave keeps
// the state of a run in the commands it ran, so a run needs a root no
// earlier run has used, and a leaf no earlier run has reached.
func (wr wiredRegistry) root(version string) *ucli.Command {
	commandsList := assembleCommands(wr.fixed, wr.order, wr.groups)

	// Install aliasAwareGroupComplete on every verb group (any command that
	// has subcommands). For groups whose leaves carry Aliases this lists
	// canonical name + aliases; for others (config, auth, etc.) it lists
	// canonical names only - same as urfave's default, but with the
	// flag-name branch that DefaultCompleteWithFlags lacks for the stripped
	// current word (so `config --<TAB>` and `get --<TAB>` both yield flag
	// candidates).
	for _, cmd := range commandsList {
		if len(cmd.Commands) > 0 {
			cmd.ShellComplete = noCompletionAfterDoubleDash(aliasAwareGroupComplete)
		}
	}

	rootShellComplete := func(ctx context.Context, c *ucli.Command) {
		// rawCurrentWord (stripped from argv) is classified by the same shared
		// routing the verb groups use - so root-level flag-value sites
		// (`rancher -o <TAB>`, `rancher --output=<TAB>`) reach the dispatch
		// pipeline instead of the command listing. The bare-word branch keeps
		// the top-level command listing.
		completeCurrentWord(ctx, c, func(w io.Writer) {
			for _, cand := range nameLikeCandidates(c, rawCurrentWord) {
				_, _ = fmt.Fprintln(w, cand)
			}
		})
	}

	return &ucli.Command{
		Name:          "rancher",
		ShellComplete: noCompletionAfterDoubleDash(rootShellComplete),
		Usage:         "rancher-cli",
		Version:       version,
		Flags: append([]ucli.Flag{
			// --debug is accepted and currently a no-op.
			&ucli.BoolFlag{Name: "debug", Usage: "accepted; currently no effect"},
			&ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to the config directory",
				Sources: ucli.EnvVars(config.EnvConfigDir)},
		}, GlobalFlags()...),
		CommandNotFound: commandNotFound,
		OnUsageError:    onUsageError,
		// urfave reads the exit handler only from the root, and by default
		// ends the process for an error that carries an exit code. This
		// one does nothing, so every such error returns from Run and the
		// caller decides how the process ends.
		ExitErrHandler: func(context.Context, *ucli.Command, error) {},
		// With StopOnNthArg 1 the root parses no flag after its first word:
		// a known command parses the rest itself, and an unknown first word
		// is reported before any flag typed after it.
		StopOnNthArg: new(1),
		Action:       groupCommandAction,
		// Inherited by every command: urfave adds no `help` subcommand
		// anywhere, so `help` is an ordinary unknown command name and a
		// positional argument named help always reaches its command.
		HideHelpCommand: true,
		// urfave's auto-appended completion command is renamed and kept hidden
		// machinery only: its subcommands are stripped so it can never emit
		// urfave's own (divergent) scripts - `completion <shell>` (wired below,
		// via Adapt like schema) is the one script-emitting surface.
		EnableShellCompletion:      true,
		ShellCompletionCommandName: "__completion",
		ConfigureShellCompletionCommand: func(c *ucli.Command) {
			c.Hidden = true
			c.Commands = nil
			c.CommandNotFound = commandNotFound
			c.OnUsageError = onUsageError
		},
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			config.SetReadOnlyOverride(resolveReadOnly(c))
			noteExperiments(version)
			return ctx, nil
		},
		Commands: commandsList,
	}
}
