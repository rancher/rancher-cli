package cli

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// universalShellCompleteFunc returns a urfave ShellCompleteFunc that
// dispatches TAB completions in order: (1) arg-tail site parse, (2) enum
// static, (3) config-local (ShellCompleter delegation), (4) live (gated on
// completion.network). Installed on EVERY adapted leaf (AC-17a).
func universalShellCompleteFunc(d runtime.Descriptor, sc runtime.ShellCompleter) ucli.ShellCompleteFunc {
	return func(ctx context.Context, c *ucli.Command) {
		// Strip command routing tokens from rawArgv using the invoked urfave
		// command's lineage depth — NOT the canonical descriptor name string.
		// This handles leading global flags (--output json get pod) and alias
		// invocations (inspect po → depth 2) correctly (finding 2).
		tail := stripRoutingPrefixByCmd(rawArgv, c)
		site := ParseArgTail(tail, rawCurrentWord, rawProtoV2, d, globalArgSpecs)

		if site.Kind == SiteNameLike {
			// v2 protocol: rawCurrentWord holds the real current word (stripped
			// from argv by CaptureArgv). urfave's DefaultCompleteWithFlags reads
			// from cmd.Args().Slice(), which never sees the stripped word, so it
			// always falls into the subcommand branch and emits nothing for leaves.
			// Handle it ourselves when rawProtoV2 is set.
			//
			// v1 protocol (rawProtoV2=false): urfave preserves the partial word in
			// parsedArgs (shellCompletion mode in command_parse.go), so
			// DefaultCompleteWithFlags reads it correctly from cmd.Args().Slice().
			if rawProtoV2 {
				w := c.Root().Writer
				for _, cand := range nameLikeCandidates(c, rawCurrentWord) {
					_, _ = fmt.Fprintln(w, cand)
				}
			} else {
				ucli.DefaultCompleteWithFlags(ctx, c)
			}
			return
		}

		candidates := dispatchSite(ctx, site, d, sc, c, tail)
		w := c.Root().Writer
		for _, cand := range candidates {
			_, _ = fmt.Fprintln(w, cand)
		}
	}
}

// nameLikeCandidates returns completion candidates for SiteNameLike sites,
// using rawCurrentWord and the invoked command's own flags plus the root's
// persistent globals. When currentWord starts with "-", flag-name candidates
// are returned (long-only when currentWord starts with "--"). Otherwise,
// subcommand-name candidates are returned (a leaf with no subcommands emits
// nothing). Format matches urfave's own output: "token:usage" when usage is
// non-empty, bare "token" otherwise.
func nameLikeCandidates(c *ucli.Command, currentWord string) []string {
	if strings.HasPrefix(currentWord, "-") {
		return flagNameCandidates(c, currentWord)
	}
	return subcommandCandidates(c, currentWord)
}

// formatCandidate returns the "name:usage" or bare "name" completion line,
// matching urfave's own printCommandSuggestions format (unexported upstream).
// All three emission paths — printCompletionCandidate, flagNameCandidates,
// and subcommandCandidates — call this so the format lives in one place.
func formatCandidate(name, usage string) string {
	if usage != "" {
		return name + ":" + usage
	}
	return name
}

// flagNameCandidates returns flag-name candidates for a "-" or "--" prefixed
// currentWord, covering ALL names (primary + aliases) of every de-duplicated
// flag on the command and the root's persistent globals.
//
// Hidden-flag exclusion: only *ucli.BoolFlag.Hidden is checked because urfave's
// Flag interface has no generic hidden-check method — urfave's own
// printFlagSuggestions has the identical gap. No hidden non-bool global flag
// exists today, but one added in the future would appear in suggestions.
// This is a latent limitation inherited from urfave, not a local oversight.
func flagNameCandidates(c *ucli.Command, currentWord string) []string {
	cur := strings.TrimLeft(currentWord, "-")
	longOnly := strings.HasPrefix(currentWord, "--")

	seen := make(map[string]bool)
	var out []string
	for _, f := range allFlagsDeduped(c) {
		// Skip hidden bool flags (matches urfave's printFlagSuggestions behavior;
		// see doc comment above for the inherited limitation on other flag types).
		if bf, ok := f.(*ucli.BoolFlag); ok && bf.Hidden {
			continue
		}
		usage := ""
		if dg, ok := f.(ucli.DocGenerationFlag); ok {
			usage = dg.GetUsage()
		}
		for _, name := range f.Names() {
			count := utf8.RuneCountInString(name)
			if longOnly && count == 1 {
				continue // "--" context: omit single-char aliases
			}
			prefix := "--"
			if count == 1 {
				prefix = "-"
			}
			if !strings.HasPrefix(name, cur) || name == cur {
				continue
			}
			token := prefix + name
			// Defensively dedupe emitted tokens: allFlagsDeduped dedupes by
			// primary name, so two flags with different primaries but the same
			// short alias both survive it and would double-emit that alias here.
			if seen[token] {
				continue
			}
			seen[token] = true
			out = append(out, formatCandidate(token, usage))
		}
	}
	return out
}

// subcommandCandidates returns subcommand-name candidates for a non-flag
// currentWord, prefix-filtered by currentWord. Hidden subcommands are skipped.
// A leaf with no subcommands returns nil.
func subcommandCandidates(c *ucli.Command, currentWord string) []string {
	var out []string
	for _, sub := range c.Commands {
		if sub.Hidden {
			continue
		}
		if !strings.HasPrefix(sub.Name, currentWord) {
			continue
		}
		out = append(out, formatCandidate(sub.Name, sub.Usage))
	}
	return out
}

// allFlagsDeduped returns the merged flags from c and c.Root(), de-duplicating
// by primary name (Names()[0]). Flags from c take precedence over root flags
// with the same primary name. When c is the root itself, only c.Flags is used.
//
// Latent gap: only c and c.Root() are merged — intermediate ancestor commands
// in the lineage are skipped. urfave's own flag resolution walks the full
// Lineage(), so a flag defined on an intermediate group node (not a leaf, not
// root) resolves at runtime but never appears in completion suggestions. No
// group node in the current tree sets its own Flags, so this is latent.
func allFlagsDeduped(c *ucli.Command) []ucli.Flag {
	seen := make(map[string]bool)
	var out []ucli.Flag
	addFlag := func(f ucli.Flag) {
		names := f.Names()
		if len(names) == 0 {
			return
		}
		primary := names[0]
		if seen[primary] {
			return
		}
		seen[primary] = true
		out = append(out, f)
	}
	for _, f := range c.Flags {
		addFlag(f)
	}
	if root := c.Root(); root != c {
		for _, f := range root.Flags {
			addFlag(f)
		}
	}
	return out
}

// dispatchSite resolves candidates for the given site through all layers.
// tail is the pre-stripped leaf arg tail computed by the caller.
func dispatchSite(
	ctx context.Context,
	site CompletionSite,
	d runtime.Descriptor,
	sc runtime.ShellCompleter,
	c *ucli.Command,
	tail []string,
) []string {
	// Layer 1: enum static (AC-13), for flag values AND positionals (MC-14).
	if site.Kind == SiteFlagValue {
		if cands := enumCandidates(site, d); len(cands) > 0 {
			return cands
		}
		// Machine-config `<kind>/` prefixes for create cluster: compiled-in
		// kind-table literals (kinds.MachineConfigGroup rows), not server
		// data — served statically like an enum, before (and regardless of)
		// the network gate. The live layer owns the `<kind>/partial` form.
		if cands := machineConfigPrefixCandidates(site, d); len(cands) > 0 {
			return cands
		}
		// create machine-config's --set keys: compiled-in kind-table field
		// metadata, kind-scoped from the typed tail. OWNERSHIP, not
		// first-match: the site is fully static, so even an empty result
		// terminates the dispatch here — no delegation, no live layer.
		if cands, owned := machineConfigSetKeyCandidates(site, d, tail); owned {
			return cands
		}
	}
	if site.Kind == SitePositional {
		// Ownership, not first-match: an enum-carrying positional is fully
		// static, so even an empty (unmatched-partial) result terminates
		// the dispatch here.
		if cands, owned := positionalEnumCandidates(site, d); owned {
			return cands
		}
	}

	// Layer 2: config-local (ShellCompleter delegation) for positionals.
	if site.Kind == SitePositional && sc != nil {
		view, _ := loadedConfigView()
		cands := sc.CompleteArgs(ctx, view)
		if len(cands) > 0 {
			return sanitizeCandidates(cands)
		}
	}

	// Layer 3: live (gated).
	if !completionNetworkEnabled() {
		completionDebugf("live completion off: enable with: rancher config set-completion --network=true")
		return nil
	}
	cands := liveCandidates(ctx, site, d, c, tail)
	completionDebugf("done: %d candidates", len(cands))
	return cands
}

// enumCandidates returns static enum candidates for a flag-value site.
// The output flag's enum is global (every command shares one ArgSpec), so
// it is filtered against the resolved leaf's OutputPolicy here — the one
// point every -o/--output completion passes through — rather than offering
// a value the command would reject.
func enumCandidates(site CompletionSite, d runtime.Descriptor) []string {
	all := append(append([]runtime.ArgSpec{}, d.Flags...), globalArgSpecs...)
	for _, f := range all {
		if f.Name == site.FlagName && len(f.Enum) > 0 {
			enum := f.Enum
			if f.Name == "output" {
				enum = allowedOutputEnum(enum, d.OutputPolicy)
			}
			return resolveStaticEnum(enum, site)
		}
	}
	return nil
}

// allowedOutputEnum filters enum down to the values d's OutputPolicy
// accepts, preserving today's behavior (the full enum) for the zero policy.
func allowedOutputEnum(enum []string, policy runtime.OutputPolicy) []string {
	if len(policy.AllowedFormats) == 0 {
		return enum
	}
	out := make([]string, 0, len(enum))
	for _, v := range enum {
		if policy.Allows(v) {
			out = append(out, v)
		}
	}
	return out
}

// positionalEnumCandidates returns static enum candidates for a positional
// site — Descriptor.Args[site.Index].Enum filtered by the typed partial
// (MC-14) — plus an OWNERSHIP flag: owned is true whenever the indexed arg
// carries an Enum, in which case the caller must terminate the dispatch
// with these candidates even when the filter matched nothing (an unmatched
// partial must not fall through to the delegation/live layers and
// resurface an unfiltered list). Index-aware by construction, so an
// enum-less positional (a <name> site, MC-15) or an index past the
// declared args is not owned. Compiled-in literals, same charset doctrine
// as resolveStaticEnum: the shell-safe sanitize gate does not apply.
func positionalEnumCandidates(site CompletionSite, d runtime.Descriptor) ([]string, bool) {
	if site.Index < 0 || site.Index >= len(d.Args) {
		return nil, false
	}
	enum := d.Args[site.Index].Enum
	if len(enum) == 0 {
		return nil, false
	}
	var out []string
	for _, v := range enum {
		if strings.HasPrefix(v, site.Partial) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, true
}

// liveCandidates issues at most two HTTP requests to fetch resource names
// (AC-17 steps 4-6, AC-24). Returns nil on any error (AC-18).
// tail is the pre-computed leaf arg tail from the caller (avoids re-stripping).
func liveCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, c *ucli.Command, tail []string) []string {
	// Role flag-value sites derive scope from the typed tail only
	// (roleVocabulary); the cluster fallback chain, segment validation,
	// and display-name resolution below must not run for them — an
	// inherited invalid or display-name cluster would otherwise suppress
	// role completion that never needed a cluster at all.
	// Twin requirement: siteNeedsCluster also carries an explicit
	// "case role: return false" so the clusterValue=="" early-return at
	// line 258 does not fire. Both guards must hold simultaneously.
	roleSite := site.Kind == SiteFlagValue && site.FlagName == "role"

	// Subject positional sites (index 0 of grant/revoke) fetch management
	// users and need no cluster. Apply the same preflight bypass as roleSite:
	// an inherited invalid cluster value (e.g. "prod cluster" with a space)
	// must not suppress subject completion that never needed a cluster.
	subjectSite := site.Kind == SitePositional && site.Index == 0 &&
		(d.Name == "grant" || d.Name == "revoke")

	// create cluster's node-driver flag sites (--machine-config,
	// --cloud-credential) fetch management-server-only data (fleet-default
	// machine configs, cattle-global-data credential secrets) and never
	// need a cluster. Same preflight bypass as roleSite/subjectSite: an
	// inherited invalid cluster value (e.g. "prod cluster" with a space)
	// must not suppress completion that never needed a cluster at all.
	// Twin requirement: siteNeedsCluster carries explicit false cases for
	// both flags so the clusterValue=="" early-return below cannot fire.
	nodeDriverFlagSite := site.Kind == SiteFlagValue && d.Name == "create cluster" &&
		(site.FlagName == "machine-config" || site.FlagName == "cloud-credential")

	// logs' --pod site is entirely self-contained: its cluster comes from the
	// curated target's LocalOnly/DownstreamOnly shape (observe.Targets), not a
	// typed --cluster, so it resolves its own capability and returns directly
	// rather than flowing through the generic clusterValue pipeline below.
	if site.Kind == SiteFlagValue && d.Name == "logs" && site.FlagName == "pod" {
		return logsPodCandidates(ctx, site, d, tail)
	}

	// A cluster-family verb's --pool is dependent on its own <cluster>
	// positional, not a typed --cluster flag — self-contained like
	// logsPodCandidates above.
	if site.Kind == SiteFlagValue && site.FlagName == "pool" && clusterFamilyPositionalKind[d.Name] {
		return clusterFamilyPoolCandidates(ctx, site, d, tail)
	}

	// Scope comes from the typed tail only — nothing inherited (no config, no
	// env) may place or filter completion. A typed --project is its own
	// announcement; absent flags leave the values empty.
	clusterValue := ""
	effectiveProject := ""
	if !roleSite && !subjectSite && !nodeDriverFlagSite {
		clusterValue = extractFlagValue(tail, "cluster")
		effectiveProject = extractFlagValue(tail, "project")
	}

	// Finding 1 (SECURITY): validate the cluster segment before it enters any
	// REST path. A value containing "/" or other non-DNS characters fails here;
	// the whole dispatch returns nil (fail closed, zero requests).
	if clusterValue != "" && !isValidSegment(clusterValue) {
		completionDebugf("stop: --cluster value %q is not a valid path segment", clusterValue)
		return nil
	}

	// Round-2 finding 5: before any HTTP dispatch, if the site requires a cluster
	// and none resolves, return nil with ZERO requests. This prevents
	// /k8s/clusters//... calls (an empty cluster ID in the URL).
	if siteNeedsCluster(site, d) && clusterValue == "" {
		completionDebugf("stop: site needs --cluster and none was typed")
		return nil
	}

	// Apply deadline (AC-24).
	ctx, cancel := completionDeadline(ctx)
	defer cancel()

	cap := resolveCompletionToken(clusterValue)
	if cap.Token == "" {
		return nil
	}

	// Finding 9: build ONE HTTP client per dispatch (not per fetch call).
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		completionDebugf("stop: HTTP client: %v", err)
		return nil
	}

	// AC-17 step 5 (finding 11): if cluster value is not ID-shaped and management
	// token is available, issue the optional cluster-resolution request to map
	// display name → ID. Exec-cred-only (IDOnly) skips this (can't authorize
	// the management cluster list).
	// Finding 6: gate resolution on siteNeedsCluster — cluster/context VALUE
	// sites don't need a resolved ID (they fetch the list anyway) and must not
	// issue two cluster-list requests.
	resolvedClusterID := clusterValue
	if clusterValue != "" && !isClusterIDShaped(clusterValue) && !cap.IDOnly && siteNeedsCluster(site, d) {
		if id := resolveDisplayNameToClusterID(ctx, hc, cap, clusterValue); id != "" {
			resolvedClusterID = id
		} else {
			// Display name unresolvable: can't continue for cluster-dependent site.
			completionDebugf("stop: cluster display name %q did not resolve to one id", clusterValue)
			return nil
		}
	}

	switch site.Kind {
	case SiteFlagValue:
		// Round-2 finding 4: pass effectiveProject so --namespace is suppressed
		// when a project scope is active (AC-9/AC-15).
		return liveFlagValueCandidates(ctx, hc, site, cap, resolvedClusterID, effectiveProject, tail)
	case SitePositional:
		return livePositionalCandidates(ctx, hc, site, d, cap, resolvedClusterID, effectiveProject, tail)
	}
	return nil
}

// isClusterIDShaped reports whether s looks like a Rancher management cluster ID
// (starts with "c-" or is "local"). Display names rarely start with "c-".
func isClusterIDShaped(s string) bool {
	return s == "local" || strings.HasPrefix(s, "c-")
}

// siteNeedsCluster reports whether a site requires a resolved cluster ID to
// produce meaningful candidates. Derived from the kind row's Scope field
// (finding 5), with an explicit carve-out for project: it is Management scope
// but lists projects in a cluster's namespace, so it also needs a cluster ID.
func siteNeedsCluster(site CompletionSite, d runtime.Descriptor) bool {
	if site.Kind == SiteFlagValue {
		switch site.FlagName {
		case "project", "namespace", "node":
			return true
		case "role":
			// Role completion derives scope from the typed tail only (roleVocabulary).
			// The roleSite bypass in liveCandidates skips the tail cluster
			// extraction, leaving
			// clusterValue="". This explicit false prevents liveCandidates' cluster
			// early-return from silently killing role completion when clusterValue is
			// empty. Both guards must hold together — changing either one without the
			// other breaks role TAB.
			return false
		case "machine-config", "cloud-credential":
			// create cluster's node-driver sites are management-server-only
			// fetches; the nodeDriverFlagSite bypass in liveCandidates leaves
			// clusterValue="". Same twin-guard contract as role above.
			return false
		}
		return false
	}
	k, ok := kindForDescriptor(d)
	if !ok {
		return false
	}
	// Downstream kinds served by a downstream K8s proxy need a cluster ID, but
	// only when the kind is Completable — resolution is pointless for a kind
	// livePositionalCandidates will reject (round-4 finding 3).
	// Management NSCluster (project) lists resources in the cluster's namespace
	// via the management API — also needs a cluster ID (explicit carve-out).
	return (k.Scope == kinds.Downstream && k.Completable) || (k.Scope == kinds.Management && k.Policy == kinds.NSCluster)
}

// resolveDisplayNameToClusterID issues one management cluster list request to
// map a display name to a cluster ID (AC-17 step 5 optional request, finding 11).
// Returns "" when not found, ambiguous, or the request fails.
// hc is the single per-dispatch client (finding 9).
func resolveDisplayNameToClusterID(ctx context.Context, hc *http.Client, cap TokenCapability, displayName string) string {
	url := cap.ServerURL + "/k8s/clusters/local/apis/management.cattle.io/v3/clusters"
	var col struct {
		Items []map[string]any `json:"items"`
	}
	if err := getJSON(ctx, hc, cap.Token, url, &col); err != nil {
		return ""
	}
	var matches []string
	for _, item := range col.Items {
		meta, _ := item["metadata"].(map[string]any)
		if meta == nil {
			continue
		}
		id, _ := meta["name"].(string)
		spec, _ := item["spec"].(map[string]any)
		var name string
		if spec != nil {
			name, _ = spec["displayName"].(string)
		}
		if name == displayName {
			matches = append(matches, id)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return "" // not found or ambiguous
}

// roleVocab identifies which role vocabulary a --role completion should
// offer, derived from the scope flags on the typed line.
type roleVocab int

const (
	roleVocabNone roleVocab = iota
	roleVocabGlobal
	roleVocabProject
	roleVocabCluster
)

// roleVocabulary maps the typed tail's scope flags to a role vocabulary,
// reproducing rbac.ResolveScope's scope→vocabulary mapping and conflict
// REJECTION — but by flag PRESENCE (flagPresent), a deliberate mid-edit
// heuristic, not ResolveScope's value parsing: a completion line is
// unfinished by nature ("--project= <TAB>" is about to receive a value),
// and completion only suggests — a heuristic mismatch costs an unhelpful
// list, never a wrong mutation. The context/env/config cluster fallbacks
// are deliberately NOT consulted: grant/revoke refuse implicit scope, so
// completion must not suggest a vocabulary the command would reject.
func roleVocabulary(tail []string) roleVocab {
	global := boolFlagSet(tail, "global") // boolean: "--global=false" is a negation, not mid-edit
	project := flagPresent(tail, "project")
	cluster := flagPresent(tail, "cluster")
	switch {
	case global && (project || cluster):
		return roleVocabNone // ResolveScope rejects the combination
	case global:
		return roleVocabGlobal
	case project:
		return roleVocabProject
	case cluster:
		return roleVocabCluster
	}
	return roleVocabNone
}

// liveFlagValueCandidates handles live flag-value sites (--cluster, --project,
// --namespace, --node AC-7/AC-8/AC-9/AC-11/AC-14, --role AC-role).
// effectiveProject is passed to suppress --namespace when a typed --project
// scope is active (AC-9/AC-15; round-2 finding 4).
// tail is threaded from liveCandidates for role scope detection (roleVocabulary).
// hc is the single per-dispatch client (finding 9).
// For role sites, clusterID and effectiveProject are intentionally empty and
// ignored — role scope is derived exclusively from tail via roleVocabulary,
// never from inherited env/config values.
func liveFlagValueCandidates(ctx context.Context, hc *http.Client, site CompletionSite, cap TokenCapability, clusterID, effectiveProject string, tail []string) []string {
	var raw []string
	switch site.FlagName {
	case "cluster":
		raw = fetchClusters(ctx, hc, cap)
	case "project":
		raw = fetchProjects(ctx, hc, cap, clusterID)
	case "namespace":
		// AC-9/AC-15: suppress namespace completion when a project scope is active
		// (finding 5: routed through kinds.ProjectScopable for the namespace kind).
		if nsKind, ok := kinds.Lookup("namespace"); ok && kinds.ProjectScopable(nsKind) && effectiveProject != "" {
			return nil
		}
		raw = fetchK8sResources(ctx, hc, cap, clusterID, "/api/v1/namespaces", "")
	case "node":
		raw = fetchK8sResources(ctx, hc, cap, clusterID, "/api/v1/nodes", "")
	case "machine-config":
		// Live `<kind>/partial` form only — the bare `<kind>/` prefixes are
		// compiled-in and served statically by machineConfigPrefixCandidates
		// before the network gate. Returns directly: the composed tokens
		// carry a "/" that the shared sanitize/equals-form tail below would
		// drop (the trust boundary lives inside the helper).
		return machineConfigNameCandidates(ctx, hc, site, cap)
	case "cloud-credential":
		raw = fetchCloudCredentials(ctx, hc, cap)
	case "role":
		// Scope comes from the typed tail ONLY (roleVocabulary): the
		// clusterID/effectiveProject parameters carry context/env/config
		// fallbacks that grant/revoke's explicit-scope rule forbids here.
		switch roleVocabulary(tail) {
		case roleVocabGlobal:
			raw = fetchGlobalRoles(ctx, hc, cap)
		case roleVocabProject:
			raw = fetchRoleTemplates(ctx, hc, cap, "project")
		case roleVocabCluster:
			raw = fetchRoleTemplates(ctx, hc, cap, "cluster")
		default:
			return nil // no scope or conflicting scope: empty, zero requests
		}
	default:
		return nil
	}
	if site.EqualsForm {
		return equalsFormCandidates(site.FlagSpelling, raw)
	}
	return raw
}

// logsPodCandidates completes `logs <target> --pod`: it resolves the typed
// target positional to its curated namespace/selector (observe.Targets) and
// lists pods there. fleet-agent's namespace is resolved dynamically at
// Execute time, not from the static table, so it has nothing to complete
// from here and returns no candidates — a degradation by design, not a
// failure, matching the uncurated-machine-config precedent. A DownstreamOnly
// target (agent, kube-api-auth) needs a cluster typed on the tail; a
// display name is resolved to its cluster id the same way the generic
// pipeline does it — completion must work with whichever spelling the
// operator used for --cluster.
func logsPodCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, tail []string) []string {
	targetTok := firstPositionalToken(tail, d.Flags)
	target, ok := observe.Lookup(targetTok)
	if !ok || target.Namespace == "" {
		return nil
	}

	clusterID := engine.LocalCluster
	if target.DownstreamOnly {
		clusterID = extractFlagValue(tail, "cluster")
		if clusterID == "" || !isValidSegment(clusterID) {
			return nil
		}
	}

	ctx, cancel := completionDeadline(ctx)
	defer cancel()

	cap := resolveCompletionToken(clusterID)
	if cap.Token == "" {
		return nil
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		completionDebugf("stop: HTTP client: %v", err)
		return nil
	}

	// The typed --cluster value may be a display name; resolve it exactly
	// like the generic pipeline. An id-shaped value matches no display
	// name and passes through unchanged.
	if target.DownstreamOnly {
		if id := resolveDisplayNameToClusterID(ctx, hc, cap, clusterID); id != "" {
			clusterID = id
		}
	}

	raw := fetchPods(ctx, hc, cap, clusterID, target.Namespace, target.Selector)
	if site.EqualsForm {
		return equalsFormCandidates(site.FlagSpelling, raw)
	}
	return raw
}

// livePositionalCandidates handles live positional sites (resource names,
// AC-10). Effective-project suppression uses kinds.ProjectScopable (finding 5).
// Paths are derived generically from k.GVR (finding 4): flipping Completable:true
// on a new downstream kind row needs NO dispatch edit.
// tail is the already-computed leaf arg tail from the callback (round-2
// finding 6: do not re-derive from rawArgv using the old canonical-prefix
// helper — that breaks alias invocations and leading global flags).
// Round-2 finding 7: only get/patch/delete leaves get live positional completion
// (AC-10); inspect leaves do NOT (AC-15a is aliases only, not live completion).
// hc is the single per-dispatch client (finding 9).
func livePositionalCandidates(
	ctx context.Context,
	hc *http.Client,
	site CompletionSite,
	d runtime.Descriptor,
	cap TokenCapability,
	clusterID, effectiveProject string,
	tail []string,
) []string {
	// Bespoke single-kind ops (the oidc-client lifecycle verb group):
	// derivedOpKinds supplies safety Binding only, never completion — this
	// is the data-driven completion seam commit 3 adds. Index 0 (the
	// client name) reuses the SAME generic management-kind listing path
	// get/patch/delete use; index 1 (secret-name, rotate/remove only) is a
	// DEPENDENT positional needing a client-scoped GET, so it cannot share
	// the index-0-only gate below.
	if kindKey, ok := bespokeSingleKindOps[d.Name]; ok {
		return bespokeSingleKindPositional(ctx, hc, site, d, cap, kindKey, tail)
	}

	// Cluster-family verbs' <cluster> positional names a provisioning
	// cluster, not the generic "cluster" kind AC-10/inspect exceptions
	// below resolve — handled before that gate, which these command names
	// would otherwise fail.
	if clusterFamilyPositionalKind[d.Name] {
		if site.Index != 0 {
			return nil
		}
		return provisioningClusterPositionalCandidates(ctx, hc, cap, tail)
	}

	if site.Index != 0 {
		return nil // only first positional completes for resource names
	}

	// Subject positional completion for grant/revoke (AC-subject):
	// fetch management users and return the union of name + username.
	// --type group → no listable group source; return nil, zero requests.
	// Bare --type with no value yields "" from extractFlagValue → treat as user.
	if d.Name == "grant" || d.Name == "revoke" {
		if extractFlagValue(tail, "type") == "group" {
			return nil
		}
		return fetchUsers(ctx, hc, cap)
	}

	// Gate on AC-10 verbs (get/patch/delete only), with three named
	// exceptions — inspect cluster, inspect project, and inspect user.
	// Every other inspect leaf retains the deny.
	if !isAC10Verb(d.Name) && d.Name != inspectClusterCompletionException &&
		d.Name != inspectProjectCompletionException && d.Name != inspectUserCompletionException {
		return nil
	}
	k, ok := kindForDescriptor(d)
	if !ok || !k.Completable {
		return nil
	}

	// Management-plane kinds (cluster, project) use the Rancher management API
	// paths, which differ from the downstream K8s proxy pattern. Keep an
	// explicit management branch; new management Completable kinds need a case.
	if k.Scope == kinds.Management {
		switch k.Name {
		case "cluster":
			return fetchClusters(ctx, hc, cap)
		case "project":
			return fetchProjects(ctx, hc, cap, clusterID)
		case "nodedriver":
			// Management-plane list served by the local cluster; requires a
			// management token — exec-cred-only capabilities (IDOnly) cannot
			// authorize it, same boundary as fetchClusters (zero requests).
			if cap.IDOnly {
				return nil
			}
			return fetchK8sResources(ctx, hc, cap, "local", gvrPath(k), "")
		case "user":
			// Kind-positional user completion (get/delete/patch): address by
			// metadata.name only — bare usernames are rejected verbatim by these
			// commands. Grant/revoke subject completion (above) keeps the full
			// id+username union. System accounts filtered.
			return fetchUserIDs(ctx, hc, cap)
		case "provisioning-cluster":
			return provisioningClusterPositionalCandidates(ctx, hc, cap, tail)
		default:
			// Any other Completable NSNone management kind (e.g. oidcclient):
			// list via the local cluster, the same shape as nodedriver — a
			// full management token is required, since exec-cred-only
			// (IDOnly) capabilities cannot authorize a management-plane list.
			if k.Policy != kinds.NSNone {
				return nil
			}
			if cap.IDOnly {
				return nil
			}
			return fetchK8sResources(ctx, hc, cap, "local", gvrPath(k), "")
		}
	}

	// Downstream kinds: derive path generically from k.GVR (finding 4).
	// kinds.ProjectScopable covers namespace (NSNone carve-out), pod, deployment,
	// and any future NSPlain Downstream kind (finding 5).
	if kinds.ProjectScopable(k) && effectiveProject != "" {
		return nil // AC-15: suppress when project scope is active
	}

	if k.Policy == kinds.NSNone {
		// Cluster-scoped resource.
		return fetchK8sResources(ctx, hc, cap, clusterID, gvrPath(k), "")
	}

	// Namespace-scoped resource: requires --namespace flag.
	// Finding 1: validate the namespace segment before it enters the REST path.
	ns := extractFlagValue(tail, "namespace")
	if ns == "" || !isValidSegment(ns) {
		return nil
	}
	return fetchK8sResources(ctx, hc, cap, clusterID, gvrNamespacedPath(k), ns)
}

// provisioningClusterPositionalCandidates lists provisioning.cattle.io/v1
// Cluster names on the local management cluster, honoring --namespace or
// falling back to the kind's DefaultNamespace (fleet-default) — shared by
// get/delete provisioning-cluster's generic positional (via the
// "provisioning-cluster" case above) and every cluster-family bespoke
// verb's <cluster> positional (via clusterFamilyPositionalKind below).
// Requires a full management token — IDOnly exec-cred tokens cannot
// authorize management-plane lists (same boundary as nodedriver).
func provisioningClusterPositionalCandidates(ctx context.Context, hc *http.Client, cap TokenCapability, tail []string) []string {
	if cap.IDOnly {
		return nil
	}
	k, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		return nil
	}
	ns := extractFlagValue(tail, "namespace")
	if ns == "" {
		ns = k.DefaultNamespace
	}
	if !isValidSegment(ns) {
		return nil
	}
	return fetchK8sResources(ctx, hc, cap, "local", gvrNamespacedPath(k), ns)
}

// clusterFamilyPositionalKind is the closed set of cluster-family bespoke
// verbs whose index-0 positional is a provisioning-cluster name (not the
// generic get/patch/delete "cluster" kind, which names a MANAGEMENT
// cluster) — the completion-only sibling of root.go's derivedOpKinds,
// consulted by livePositionalCandidates before the generic AC-10 gate,
// which these bespoke command names would otherwise fail.
var clusterFamilyPositionalKind = map[string]bool{
	"cluster scale":              true,
	"cluster set-autoscaling":    true,
	"cluster clear-autoscaling":  true,
	"cluster pause-autoscaling":  true,
	"cluster resume-autoscaling": true,
}

// clusterFamilyPoolCandidates completes a cluster-family verb's --pool
// flag: resolves the typed <cluster> positional to its provisioning Cluster
// object (namespace from --namespace or fleet-default) and lists its
// machine pool names — a DEPENDENT flag site, the same shape
// bespokeSingleKindPositional's index-1 secret-name completion uses for
// oidc-client, but keyed off a positional rather than another flag. Fully
// self-contained like logsPodCandidates: there is no --cluster flag on
// these commands, so it never enters the generic clusterValue pipeline.
func clusterFamilyPoolCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, tail []string) []string {
	clusterName := firstPositionalToken(tail, d.Flags)
	if clusterName == "" || !isValidSegment(clusterName) {
		return nil
	}
	k, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		return nil
	}
	ns := extractFlagValue(tail, "namespace")
	if ns == "" {
		ns = k.DefaultNamespace
	}
	if !isValidSegment(ns) {
		return nil
	}

	ctx, cancel := completionDeadline(ctx)
	defer cancel()
	cap := resolveCompletionToken(engine.LocalCluster)
	if cap.Token == "" || cap.IDOnly {
		return nil
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		completionDebugf("stop: HTTP client: %v", err)
		return nil
	}
	raw := fetchClusterFamilyPoolNames(ctx, hc, cap, k, ns, clusterName)
	if site.EqualsForm {
		return equalsFormCandidates(site.FlagSpelling, raw)
	}
	return raw
}

// machineConfigPrefixCandidates serves the `<kind>/` prefixes for create
// cluster's --machine-config flag while the typed value has no "/" yet.
// The kinds are compiled-in literals — every kinds.MachineConfigGroup row,
// the same single source createcluster.go's VALIDATION hint and schema desc
// derive from — not server data, so like resolveStaticEnum the AC-21
// shell-safe charset does not gate them: the "/" (and trailing "/") must
// survive. Returns nil for every other site; the live layer
// (machineConfigNameCandidates) owns the `<kind>/partial` form.
func machineConfigPrefixCandidates(site CompletionSite, d runtime.Descriptor) []string {
	if d.Name != "create cluster" || site.FlagName != "machine-config" ||
		strings.Contains(site.Partial, "/") {
		return nil
	}
	var out []string
	for _, k := range kinds.All() {
		if k.GVR.Group != kinds.MachineConfigGroup {
			continue
		}
		tok := k.Name + "/"
		if site.EqualsForm {
			tok = site.FlagSpelling + "=" + tok
		}
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// machineConfigNameCandidates completes the `<kind>/partial` form of create
// cluster's --machine-config: it lists that kind's CRs in the fleet-default
// workspace (the fixed namespace create cluster provisions into) and
// composes `<kind>/<name>` tokens.
//
// TRUST BOUNDARY (AC-21): the NAME segments are server data and are
// sanitized with the shell-safe charset (inside fetchK8sResources) BEFORE
// composition; the `<kind>/` prefix is a compiled-in kind-table literal
// appended afterwards — the composed token deliberately carries a "/"
// without widening the global charset, the same doctrine as
// resolveStaticEnum's compiled-in enum values.
//
// Management-plane list on the local cluster: requires a management token —
// exec-cred-only capabilities (IDOnly) return nil with zero requests, the
// same boundary as fetchClusters.
func machineConfigNameCandidates(ctx context.Context, hc *http.Client, site CompletionSite, cap TokenCapability) []string {
	kindTok, _, hasSlash := strings.Cut(site.Partial, "/")
	if !hasSlash {
		return nil // no "/" yet: the static prefix layer owns this site
	}
	k, found := kinds.Lookup(kindTok)
	if !found || k.GVR.Group != kinds.MachineConfigGroup {
		return nil // unknown or non-machine-config kind: zero requests
	}
	if cap.IDOnly {
		return nil
	}
	names := fetchK8sResources(ctx, hc, cap, "local", gvrNamespacedPath(k), k.DefaultNamespace)
	out := make([]string, 0, len(names))
	for _, n := range names { // names arrive sanitized and sorted
		tok := k.Name + "/" + n
		if site.EqualsForm {
			tok = site.FlagSpelling + "=" + tok
		}
		out = append(out, tok)
	}
	return out
}

// machineConfigSetKeyCandidates serves kind-scoped `key=` continuation
// tokens for create machine-config's --set flag (MC-16): the kind is read
// from the typed tail's first positional, and the candidates are that
// kind's Known driver fields MINUS the MC-8 credential set MINUS the
// curated-flag fields — MC-7 rejects a curated key via --set
// unconditionally and MC-8 rejects credential keys, so neither may ever
// be suggested (suggestions valid verbatim). Uncurated kinds (empty
// Known) get no candidates.
//
// OWNERSHIP: the returned bool is true for EVERY --set value site of
// create machine-config — the site is fully static (no live source
// exists), so the dispatcher must terminate with the result even when it
// is empty: an unmatched partial, a missing/unknown kind, an uncurated
// kind, or a value-in-progress (Partial carrying "=") all end the
// dispatch with zero candidates and zero lower-layer activity.
//
// Compiled-in literals, not server data: like resolveStaticEnum the
// shell-safe charset does not gate them (the trailing "=" must survive),
// and no network gate applies. Curated-value completion is a ledgered
// follow-up (MC-17).
func machineConfigSetKeyCandidates(site CompletionSite, d runtime.Descriptor, tail []string) ([]string, bool) {
	if d.Name != "create machine-config" || site.FlagName != "set" {
		return nil, false
	}
	if strings.Contains(site.Partial, "=") {
		return nil, true
	}
	kindTok := firstPositionalToken(tail, d.Flags)
	if kindTok == "" {
		return nil, true
	}
	k, found := kinds.Lookup(kindTok)
	if !found || k.GVR.Group != kinds.MachineConfigGroup {
		return nil, true
	}
	meta, ok := kinds.MachineConfigFieldsFor(k.Name)
	if !ok {
		return nil, true
	}
	excluded := map[string]bool{}
	for _, c := range meta.Credential {
		excluded[c] = true
	}
	for _, c := range meta.Curated {
		excluded[c] = true
	}
	var out []string
	for _, key := range meta.Known {
		if excluded[key] || !strings.HasPrefix(key, site.Partial) {
			continue
		}
		tok := key + "="
		if site.EqualsForm {
			tok = site.FlagSpelling + "=" + tok
		}
		out = append(out, tok)
	}
	sort.Strings(out)
	return out, true
}

// bespokeSingleKindOps maps a bespoke op whose surface is a single kind
// row's resource to that row's key, for completion — a data-driven
// mapping, never per-name exceptions.
// derivedOpKinds (root.go) is the sibling mapping for safety Binding
// derivation — kept separate on purpose, since a future bespoke op could
// need one without the other.
var bespokeSingleKindOps = map[string]string{
	"oidc-client add-secret":    "oidcclient",
	"oidc-client rotate-secret": "oidcclient",
	"oidc-client remove-secret": "oidcclient",
	// create password-change-request's <user> positional names an EXISTING
	// user by id — completes against the user kind exactly like get/patch/
	// delete user's own positional. create user's <username> positional is
	// deliberately absent here: it is a NEW username the operator is
	// choosing, never an existing one to complete against.
	"create password-change-request": "user",
}

// bespokeSingleKindPositional completes a bespoke op's positionals for a
// kind named in bespokeSingleKindOps: index 0 (the resource name) lists via
// the same generic management-kind path get/patch/delete use; index 1
// (present only on rotate-secret/remove-secret's secret-name) is a
// DEPENDENT positional resolved via a client-scoped GET of index 0's
// already-typed value — a missing or unresolvable first positional
// completes to nothing, never an error into the TAB stream.
func bespokeSingleKindPositional(ctx context.Context, hc *http.Client, site CompletionSite, d runtime.Descriptor, cap TokenCapability, kindKey string, tail []string) []string {
	k, ok := kinds.Lookup(kindKey)
	if !ok || !k.Completable {
		return nil
	}
	switch site.Index {
	case 0:
		if cap.IDOnly {
			return nil
		}
		return fetchK8sResources(ctx, hc, cap, "local", gvrPath(k), "")
	case 1:
		clientName := firstPositionalToken(tail, d.Flags)
		if clientName == "" || !isValidSegment(clientName) {
			return nil
		}
		if cap.IDOnly {
			return nil
		}
		return fetchOIDCClientSecretNames(ctx, hc, cap, k, clientName)
	default:
		return nil
	}
}

// firstPositionalToken returns the first non-flag token in the leaf tail,
// consuming flag values by arity (leaf flags + globals), or "" when none.
func firstPositionalToken(tail []string, leafFlags []runtime.ArgSpec) string {
	all := append(append([]runtime.ArgSpec{}, leafFlags...), globalArgSpecs...)
	for i := 0; i < len(tail); i++ {
		arg := tail[i]
		if strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "=") {
				continue
			}
			canonical := resolveFlagCanonical(arg, all)
			if flagTakesValue(canonical, all) {
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

// gvrPath returns the cluster-scoped K8s API path for a kind's GVR.
// Core group (Group==""): /api/<version>/<resource>
// Named group: /apis/<group>/<version>/<resource>
func gvrPath(k kinds.Kind) string {
	if k.GVR.Group == "" {
		return fmt.Sprintf("/api/%s/%s", k.GVR.Version, k.GVR.Resource)
	}
	return fmt.Sprintf("/apis/%s/%s/%s", k.GVR.Group, k.GVR.Version, k.GVR.Resource)
}

// gvrNamespacedPath returns the namespace-scoped K8s API path for a kind's GVR.
// The literal "{namespace}" is replaced by fetchK8sResources with the real value.
// Core group: /api/<version>/namespaces/{namespace}/<resource>
// Named group: /apis/<group>/<version>/namespaces/{namespace}/<resource>
func gvrNamespacedPath(k kinds.Kind) string {
	if k.GVR.Group == "" {
		return fmt.Sprintf("/api/%s/namespaces/{namespace}/%s", k.GVR.Version, k.GVR.Resource)
	}
	return fmt.Sprintf("/apis/%s/%s/namespaces/{namespace}/%s", k.GVR.Group, k.GVR.Version, k.GVR.Resource)
}

// inspectClusterCompletionException is IC-3's narrow carve-out from the
// AC-10 verb gate: exactly this leaf, never the inspect group. inspect
// cluster's positional IS a management cluster name-or-id, so it completes
// via the same fetchClusters path get cluster's positional uses; inspect
// pod/deployment remain excluded (AC-15a is aliases only).
// inspectProjectCompletionException is the second exception (members-ergonomics
// slice 4): inspect project's positional completes project names via the
// resolved cluster.
// inspectUserCompletionException is the third exception: inspect user's
// positional is a user id, completed by the same "user" kind branch below
// get/patch/delete already use (case "user"), which excludes system
// accounts and returns ids only, never ambiguous.
const (
	inspectClusterCompletionException = "inspect cluster"
	inspectProjectCompletionException = "inspect project"
	inspectUserCompletionException    = "inspect user"
)

// isAC10Verb reports whether the descriptor name starts with an AC-10 verb
// (get, patch, delete). inspect leaves do NOT get live positional
// completion through this predicate — the three narrow exceptions are
// inspectClusterCompletionException (IC-3), inspectProjectCompletionException,
// and inspectUserCompletionException, each checked by name at the gate;
// AC-15a adds inspect aliases, not live-name completion.
func isAC10Verb(descriptorName string) bool {
	parts := strings.Fields(descriptorName)
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "get", "patch", "delete":
		return true
	}
	return false
}

// kindForDescriptor resolves the kinds.Kind corresponding to a leaf
// Descriptor (by matching the leaf name against the kinds table).
func kindForDescriptor(d runtime.Descriptor) (kinds.Kind, bool) {
	// Leaf name is "get pod", "patch deployment", "delete namespace", etc.
	// The kind name is the last token.
	parts := strings.Fields(d.Name)
	if len(parts) < 2 {
		return kinds.Kind{}, false
	}
	return kinds.Lookup(parts[len(parts)-1])
}

// stripRoutingPrefixByCmd removes command routing tokens from argv using the
// invoked urfave command's Lineage depth (finding 2). This correctly handles:
//   - alias invocations: `inspect po arg` (c.Lineage depth=2, skip "inspect","po")
//   - leading global flags: `--output json get pod --cluster x` (skip 2 non-flag tokens)
func stripRoutingPrefixByCmd(argv []string, c *ucli.Command) []string {
	// Lineage returns [leaf, ..., root]. depth = non-root levels.
	depth := len(c.Lineage()) - 1
	return stripRoutingPrefixByDepth(argv, depth)
}

// stripRoutingPrefixByDepth skips `depth` non-flag routing tokens from argv,
// transparently consuming flag tokens (using globalArgSpecs for arity).
// Used by stripRoutingPrefixByCmd and directly testable without a *ucli.Command.
func stripRoutingPrefixByDepth(argv []string, depth int) []string {
	if depth <= 0 {
		return argv
	}
	count := 0
	i := 0
	for i < len(argv) && count < depth {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			// Flag token: skip it plus its value if it takes one.
			if eq := strings.Index(arg, "="); eq > 0 {
				i++ // --flag=value: single token
			} else {
				canonical := resolveFlagCanonical(arg, globalArgSpecs)
				if flagTakesValue(canonical, globalArgSpecs) && i+1 < len(argv) {
					i += 2 // --flag value
				} else {
					i++ // bool flag
				}
			}
		} else {
			// Non-flag token: this is one routing token.
			count++
			i++
		}
	}
	if count < depth {
		return nil
	}
	return argv[i:]
}
