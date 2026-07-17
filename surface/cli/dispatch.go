package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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
			// v1 path: urfave's DefaultCompleteWithFlags handles flag names
			// and subcommand names; no change needed.
			ucli.DefaultCompleteWithFlags(ctx, c)
			return
		}

		candidates := dispatchSite(ctx, site, d, sc, c, tail)
		w := c.Root().Writer
		for _, cand := range candidates {
			_, _ = fmt.Fprintln(w, cand)
		}
	}
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
		view := loadedConfigView()
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
	all := append(d.Flags, globalArgSpecs...)
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

	// AC-17 step 5 (finding 11): if cluster value is not ID-shaped and management
	// token is available, issue the optional cluster-resolution request to map
	// display name → ID. Exec-cred-only (IDOnly) skips this (can't authorize
	// the management cluster list).
	resolvedClusterID := clusterValue
	if clusterValue != "" && !isClusterIDShaped(clusterValue) && !cap.IDOnly {
		if id := resolveDisplayNameToClusterID(ctx, cap, clusterValue); id != "" {
			resolvedClusterID = id
		} else {
			// Display name unresolvable: can't continue for cluster-dependent sites.
			if siteNeedsCluster(site, d) {
				return nil
			}
		}
	}

	switch site.Kind {
	case SiteFlagValue:
		// Round-2 finding 4: pass effectiveProject so --namespace is suppressed
		// when a project scope is active (AC-9/AC-15).
		return liveFlagValueCandidates(ctx, site, cap, resolvedClusterID, effectiveProject)
	case SitePositional:
		return livePositionalCandidates(ctx, site, d, cap, resolvedClusterID, effectiveProject, tail)
	}
	return nil
}

// isClusterIDShaped reports whether s looks like a Rancher management cluster ID
// (starts with "c-" or is "local"). Display names rarely start with "c-".
func isClusterIDShaped(s string) bool {
	return s == "local" || strings.HasPrefix(s, "c-")
}

// siteNeedsCluster reports whether a site requires a resolved cluster ID to
// produce meaningful candidates.
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
	switch k.Name {
	case "project", "node", "namespace", "pod", "deployment":
		return true
	}
	return false
}

// resolveDisplayNameToClusterID issues one management cluster list request to
// map a display name to a cluster ID (AC-17 step 5 optional request, finding 11).
// Returns "" when not found, ambiguous, or the request fails.
func resolveDisplayNameToClusterID(ctx context.Context, cap TokenCapability, displayName string) string {
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		return ""
	}
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

// liveFlagValueCandidates handles live flag-value sites (--cluster, --project,
// --namespace, --node, --context AC-7/AC-8/AC-9/AC-11/AC-14).
// effectiveProject is passed to suppress --namespace when a project scope is
// active (AC-9/AC-15; round-2 finding 4).
func liveFlagValueCandidates(ctx context.Context, site CompletionSite, cap TokenCapability, clusterID, effectiveProject string) []string {
	var raw []string
	switch site.FlagName {
	case "cluster", "context":
		raw = fetchClusters(ctx, cap, "")
	case "project":
		raw = fetchProjects(ctx, cap, clusterID)
	case "namespace":
		// AC-9/AC-15: suppress namespace completion when a project scope is active
		// (round-2 finding 4). The effective project was NOT empty from any source.
		if effectiveProject != "" {
			return nil
		}
		raw = fetchK8sResources(ctx, cap, clusterID, "/api/v1/namespaces", "")
	case "node":
		raw = fetchK8sResources(ctx, cap, clusterID, "/api/v1/nodes", "")
	default:
		return nil
	}
	if site.EqualsForm {
		return equalsFormCandidates(site.FlagSpelling, raw)
	}
	return raw
}

// livePositionalCandidates handles live positional sites (resource names,
// AC-10). Effective-project suppression: AC-9 and AC-10 sites are suppressed
// when effective project is non-empty (AC-15 routing rules).
// tail is the already-computed leaf arg tail from the callback (round-2
// finding 6: do not re-derive from rawArgv using the old canonical-prefix
// helper — that breaks alias invocations and leading global flags).
// Round-2 finding 7: only get/patch/delete leaves get live positional completion
// (AC-10); inspect leaves do NOT (AC-15a is aliases only, not live completion).
func livePositionalCandidates(
	ctx context.Context,
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
	switch k.Name {
	case "cluster":
		return fetchClusters(ctx, cap, "")
	case "project":
		return fetchProjects(ctx, cap, clusterID)
	case "namespace":
		if effectiveProject != "" {
			return nil // suppressed (AC-15)
		}
		return fetchK8sResources(ctx, cap, clusterID, "/api/v1/namespaces", "")
	case "node":
		return fetchK8sResources(ctx, cap, clusterID, "/api/v1/nodes", "")
	case "pod":
		if effectiveProject != "" {
			return nil // suppressed (AC-15)
		}
		// Round-2 finding 6: use the caller-supplied tail, not a re-derived one.
		ns := extractFlagValue(tail, "namespace")
		if ns == "" {
			return nil
		}
		return fetchK8sResources(ctx, cap, clusterID,
			"/api/v1/namespaces/{namespace}/pods", ns)
	case "deployment":
		if effectiveProject != "" {
			return nil // suppressed (AC-15)
		}
		// Round-2 finding 6: use the caller-supplied tail.
		ns := extractFlagValue(tail, "namespace")
		if ns == "" {
			return nil
		}
		return fetchK8sResources(ctx, cap, clusterID,
			"/apis/apps/v1/namespaces/{namespace}/deployments", ns)
	}
	return nil
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
	if v := extractFlagValue(tail, "project"); v != "" {
		return v
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

// emitSorted writes candidates to w, one per line, sorted (AC-26).
func emitSorted(w io.Writer, candidates []string) { //nolint:unused
	sorted := make([]string, len(candidates))
	copy(sorted, candidates)
	sort.Strings(sorted)
	for _, c := range sorted {
		_, _ = fmt.Fprintln(w, c)
	}
}
