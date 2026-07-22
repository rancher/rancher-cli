package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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
	// Layer 1: enum static (AC-13).
	if site.Kind == SiteFlagValue {
		if cands := enumCandidates(site, d); len(cands) > 0 {
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
		return nil
	}
	return liveCandidates(ctx, site, d, c, tail)
}

// enumCandidates returns static enum candidates for a flag-value site.
func enumCandidates(site CompletionSite, d runtime.Descriptor) []string {
	all := append(append([]runtime.ArgSpec{}, d.Flags...), globalArgSpecs...)
	for _, f := range all {
		if f.Name == site.FlagName && len(f.Enum) > 0 {
			return resolveStaticEnum(f.Enum, site)
		}
	}
	return nil
}

// liveCandidates issues at most two HTTP requests to fetch resource names
// (AC-17 steps 4-6, AC-24). Returns nil on any error (AC-18).
// tail is the pre-computed leaf arg tail from the caller (avoids re-stripping).
func liveCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, c *ucli.Command, tail []string) []string {
	clusterValue := extractClusterID(tail)
	effectiveProject := extractEffectiveProject(tail)

	// Finding 1 (SECURITY): validate the cluster segment before it enters any
	// REST path. A value containing "/" or other non-DNS characters fails here;
	// the whole dispatch returns nil (fail closed, zero requests).
	if clusterValue != "" && !isValidSegment(clusterValue) {
		return nil
	}

	// Round-2 finding 5: before any HTTP dispatch, if the site requires a cluster
	// and none resolves, return nil with ZERO requests. This prevents
	// /k8s/clusters//... calls (an empty cluster ID in the URL).
	if siteNeedsCluster(site, d) && clusterValue == "" {
		return nil
	}

	// Apply deadline (AC-24).
	ctx, cancel := context.WithTimeout(ctx, completionTimeout())
	defer cancel()

	cap := resolveCompletionToken(tail, clusterValue)
	if cap.Token == "" {
		return nil
	}

	// Finding 9: build ONE HTTP client per dispatch (not per fetch call).
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
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
			return nil
		}
	}

	switch site.Kind {
	case SiteFlagValue:
		// Round-2 finding 4: pass effectiveProject so --namespace is suppressed
		// when a project scope is active (AC-9/AC-15).
		return liveFlagValueCandidates(ctx, hc, site, cap, resolvedClusterID, effectiveProject)
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
	global := flagPresent(tail, "global")
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
// --namespace, --node, --context AC-7/AC-8/AC-9/AC-11/AC-14).
// effectiveProject is passed to suppress --namespace when a project scope is
// active (AC-9/AC-15; round-2 finding 4).
// hc is the single per-dispatch client (finding 9).
func liveFlagValueCandidates(ctx context.Context, hc *http.Client, site CompletionSite, cap TokenCapability, clusterID, effectiveProject string) []string {
	var raw []string
	switch site.FlagName {
	case "cluster", "context":
		raw = fetchClusters(ctx, hc, cap, "")
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
	default:
		return nil
	}
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
	if site.Index != 0 {
		return nil // only first positional completes for resource names
	}
	// Round-2 finding 7: gate on AC-10 verbs (get/patch/delete only).
	// inspect leaves get NO live positional completion.
	if !isAC10Verb(d.Name) {
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
			return fetchClusters(ctx, hc, cap, "")
		case "project":
			return fetchProjects(ctx, hc, cap, clusterID)
		default:
			// Other management kinds: no generic downstream path applies.
			return nil
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

// isAC10Verb reports whether the descriptor name starts with an AC-10 verb
// (get, patch, delete). inspect leaves must NOT get live positional completion
// (round-2 finding 7; AC-15a adds inspect aliases, not live-name completion).
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

// extractClusterID returns the effective cluster from the arg tail and config,
// honouring the same precedence as resolveCompletionConnection (finding 12):
//  1. --cluster flag in arg tail
//  2. --context flag in arg tail (cluster part)
//  3. RANCHER_CLI_CONTEXT env var (cluster part)
//  4. Config current context
func extractClusterID(tail []string) string {
	if v := extractFlagValue(tail, "cluster"); v != "" {
		return v
	}
	if v := extractFlagValue(tail, "context"); v != "" {
		cl, _, _ := config.SplitContext(v)
		if cl != "" {
			return cl
		}
	}
	if v := os.Getenv(config.EnvContext); v != "" {
		cl, _, _ := config.SplitContext(v)
		if cl != "" {
			return cl
		}
	}
	cfg := storedConfigSnapshot()
	if cfg == nil {
		return ""
	}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		return ctx.Cluster
	}
	return ""
}

// extractEffectiveProject returns the effective project scope from the arg tail
// and config (AC-15 suppression rules, finding 12):
//  1. --project flag in arg tail (explicit "" clears: fall-through to inherited is skipped)
//  2. --context flag in arg tail (project part; explicit "c:" with empty project CLEARS it)
//  3. RANCHER_CLI_CONTEXT env var (project part; same clearing semantics)
//  4. Config current context
//
// Round-2 finding 4: an EXPLICIT empty project in "--context c:" or
// RANCHER_CLI_CONTEXT="c:" CLEARS the inherited project (mirrors Config.Effective
// semantics in resolve.go). Do NOT fall through to the config when the context
// flag explicitly supplies an empty project — return "" immediately.
//
// Returns "" when no project scope is active (suppression does not apply).
func extractEffectiveProject(tail []string) string {
	if flagPresent(tail, "project") {
		// Explicit --project in tail: return its value (may be "" to clear inherited scope).
		return extractFlagValue(tail, "project")
	}
	if v := extractFlagValue(tail, "context"); v != "" {
		_, pr, hasProject := config.SplitContext(v)
		if hasProject {
			// Explicit project part in --context: return it (may be "" to clear).
			// Do NOT fall through — the caller explicitly set (or cleared) the project.
			return pr
		}
	}
	if v := os.Getenv(config.EnvContext); v != "" {
		_, pr, hasProject := config.SplitContext(v)
		if hasProject {
			// Same clearing semantics for env override (round-2 finding 4).
			return pr
		}
	}
	cfg := storedConfigSnapshot()
	if cfg == nil {
		return ""
	}
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		return ctx.Project
	}
	return ""
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

// stripRoutingPrefix is kept for dispatch_test.go TestStripRoutingPrefix
// compatibility. New code uses stripRoutingPrefixByCmd.
func stripRoutingPrefix(argv []string, descriptorName string) []string {
	parts := strings.Fields(descriptorName)
	if len(argv) < len(parts) {
		return nil
	}
	for i, p := range parts {
		if i >= len(argv) || argv[i] != p {
			return argv
		}
	}
	return argv[len(parts):]
}
