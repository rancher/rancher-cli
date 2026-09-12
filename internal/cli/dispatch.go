package cli

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	ucli "github.com/urfave/cli/v3"
)

// universalShellCompleteFunc returns a urfave ShellCompleteFunc that
// dispatches TAB completions in order: (1) arg-tail site parse, (2) enum
// static, (3) config-local (ShellCompleter delegation), (4) live (gated on
// completion.network). Installed on EVERY adapted leaf (AC-17a). lc is the
// command's own live source owner, nil for a command without one.
func universalShellCompleteFunc(d runtime.Descriptor, sc runtime.ShellCompleter, lc completion.Completer) ucli.ShellCompleteFunc {
	return func(ctx context.Context, c *ucli.Command) {
		// Strip command routing tokens from rawArgv using the invoked urfave
		// command's lineage depth - NOT the canonical descriptor name string.
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

		candidates := dispatchSite(ctx, site, d, sc, lc, tail)
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
// All three emission paths - printCompletionCandidate, flagNameCandidates,
// and subcommandCandidates - call this so the format lives in one place.
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
// Flag interface has no generic hidden-check method - urfave's own
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
// Latent gap: only c and c.Root() are merged - intermediate ancestor commands
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
// tail is the pre-stripped leaf arg tail computed by the caller; lc is the
// command's live source owner, nil for none.
func dispatchSite(
	ctx context.Context,
	site CompletionSite,
	d runtime.Descriptor,
	sc runtime.ShellCompleter,
	lc completion.Completer,
	tail []string,
) []string {
	// Layer 1: enum static (AC-13), for flag values AND positionals (MC-14).
	if site.Kind == SiteFlagValue {
		if cands := enumCandidates(site, d); len(cands) > 0 {
			return cands
		}
		// The access commands' --timeout: a static ladder of durations,
		// owned here like an enum (no network, no delegation).
		if accessTimeoutSite(site, d) {
			return resolveStaticEnum(accessTimeoutSuggestions, site)
		}
		// Machine-config `<kind>/` prefixes for create cluster: compiled-in
		// kind-table literals (kinds.MachineConfigGroup rows), not server
		// data - served statically like an enum, before (and regardless of)
		// the network gate. The live layer owns the `<kind>/partial` form.
		if cands := machineConfigPrefixCandidates(site, d); len(cands) > 0 {
			return cands
		}
		// create machine-config's --set keys: compiled-in kind-table field
		// metadata, kind-scoped from the typed tail. OWNERSHIP, not
		// first-match: the site is fully static, so even an empty result
		// terminates the dispatch here - no delegation, no live layer.
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
		if cands, owned := accessVerbCandidates(site, d, tail); owned {
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
	cands := liveCandidates(ctx, site, d, lc, tail)
	completionDebugf("done: %d candidates", len(cands))
	return cands
}

// enumCandidates returns static enum candidates for a flag-value site.
// The output flag's enum is global (every command shares one ArgSpec), so
// it is filtered against the resolved leaf's OutputPolicy here - the one
// point every -o/--output completion passes through - rather than offering
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
// site - Descriptor.Args[site.Index].Enum filtered by the typed partial
// (MC-14) - plus an OWNERSHIP flag: owned is true whenever the indexed arg
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

// accessQuestionCommand reports whether a command asks an authorization
// question with the shared verb/resource grammar, so its verb, resource,
// --group, and --subresource sites complete alike.
func accessQuestionCommand(name string) bool {
	return name == "access can" || name == "access who-can" || name == "access explain"
}

// accessVerbCandidates owns the verb positional of the access question
// commands: the HTTP verbs when --non-resource-url is on the line, the
// Kubernetes resource verbs otherwise. Compiled-in vocabularies, served
// like an enum, before the network gate. Ownership, not first-match: an
// unmatched partial ends the dispatch with no candidates.
func accessVerbCandidates(site CompletionSite, d runtime.Descriptor, tail []string) ([]string, bool) {
	if !accessQuestionCommand(d.Name) || site.Index != 0 {
		return nil, false
	}
	vocab := access.ResourceVerbs
	if completion.TailFlag(tail, "non-resource-url") != "" {
		vocab = access.HTTPVerbs
	}
	var out []string
	for _, v := range vocab {
		if strings.HasPrefix(v, site.Partial) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, true
}

// accessSubjectCommand reports whether d names an access command whose
// --as takes a management user: the commands that impersonate or review a
// named subject. who-can has no --as; clusters accepts users only.
func accessSubjectCommand(name string) bool {
	switch name {
	case "access can", "access list", "access explain", "access clusters", "access projects":
		return true
	}
	return false
}

// accessTimeoutSuggestions are the static --timeout candidates for the
// access commands: any positive duration is valid, so the suggestions are
// a ladder to pick from, each one a value the command accepts verbatim.
var accessTimeoutSuggestions = []string{"30s", "1m", "2m", "5m", "10m"}

// accessTimeoutSite reports a --timeout flag-value site on an access
// command whose descriptor carries the flag: the one value-taking flag of
// the group with no enum and no server-side source. Ownership follows the
// descriptor, so a command without the flag gets no ladder.
func accessTimeoutSite(site CompletionSite, d runtime.Descriptor) bool {
	return site.Kind == SiteFlagValue && site.FlagName == "timeout" &&
		strings.HasPrefix(d.Name, "access ") &&
		slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "timeout" })
}

// liveCandidates answers a live site. A site a live completion source
// claims - the command's own, a shared flag's, or a kind's - is served
// through liveSourceCandidates under the source's declared needs and
// allowance. Every other site takes the legacy path below, which issues at
// most two HTTP requests to fetch resource names (AC-17 steps 4-6, AC-24),
// with one exception: `access can`'s resource positional and its
// --group/--subresource values read the cluster's two discovery roots,
// plus, for a display-name --cluster, the one resolution request the layer
// makes for every cluster-dependent site - three in all. Returns nil on
// any error (AC-18). tail is the pre-computed leaf arg tail from the
// caller (avoids re-stripping); lc is the command's live source owner,
// nil for none.
func liveCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, lc completion.Completer, tail []string) []string {
	if cands, owned := liveSourceCandidates(ctx, site, d, lc, tail, flagSources, kindSources, positionalKinds); owned {
		return cands
	}

	// Subject positional sites (index 0 of grant/revoke) fetch management
	// users and need no cluster. The cluster fallback chain, segment
	// validation, and display-name resolution below must not run for them:
	// an inherited invalid cluster value (e.g. "prod cluster" with a space)
	// must not suppress subject completion that never needed a cluster.
	subjectSite := site.Kind == SitePositional && site.Index == 0 &&
		(d.Name == "grant" || d.Name == "revoke")

	// create cluster's node-driver flag sites (--machine-config,
	// --cloud-credential) fetch management-server-only data (fleet-default
	// machine configs, cattle-global-data credential secrets) and never
	// need a cluster. Same preflight bypass as subjectSite: an
	// inherited invalid cluster value (e.g. "prod cluster" with a space)
	// must not suppress completion that never needed a cluster at all;
	// siteNeedsCluster is false for these sites.
	nodeDriverFlagSite := site.Kind == SiteFlagValue && d.Name == "create cluster" &&
		(site.FlagName == "machine-config" || site.FlagName == "cloud-credential")

	// access can/list --as fetches management users and never needs a
	// cluster: same preflight bypass as subjectSite, so an unrelated
	// --cluster value on the line neither fails segment validation nor
	// selects a cluster-scoped credential.
	accessSubjectSite := site.Kind == SiteFlagValue && site.FlagName == "as" && accessSubjectCommand(d.Name)

	// logs' --pod site is entirely self-contained: its cluster comes from the
	// curated target's LocalOnly/DownstreamOnly shape (observe.Targets), not a
	// typed --cluster, so it resolves its own capability and returns directly
	// rather than flowing through the generic clusterValue pipeline below.
	if site.Kind == SiteFlagValue && d.Name == "logs" && site.FlagName == "pod" {
		return logsPodCandidates(ctx, site, d, tail)
	}

	// A cluster-family verb's --pool is dependent on its own <cluster>
	// positional, not a typed --cluster flag - self-contained like
	// logsPodCandidates above.
	if site.Kind == SiteFlagValue && site.FlagName == "pool" && positionalKinds[d.Name] == "provisioningcluster" {
		return clusterFamilyPoolCandidates(ctx, site, d, tail)
	}

	// Scope comes from the typed tail only - nothing inherited (no config, no
	// env) may place or filter completion; an absent flag leaves the value
	// empty.
	clusterValue := ""
	if !subjectSite && !nodeDriverFlagSite && !accessSubjectSite {
		clusterValue = completion.TailFlag(tail, "cluster")
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
	// Resolution is spent only for the sites that need an id: the access
	// questions' discovery-backed sites, the only legacy sites left with a
	// cluster need.
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
		return liveFlagValueCandidates(ctx, hc, site, d, cap, resolvedClusterID, tail)
	case SitePositional:
		return livePositionalCandidates(ctx, hc, site, d, cap, resolvedClusterID, tail)
	}
	return nil
}

// isClusterIDShaped reports whether s looks like a Rancher management cluster ID
// (starts with "c-" or is "local"). Display names rarely start with "c-".
func isClusterIDShaped(s string) bool {
	return s == "local" || strings.HasPrefix(s, "c-")
}

// siteNeedsCluster reports whether a legacy-path site requires a resolved
// cluster ID to produce meaningful candidates: the access questions'
// discovery-backed sites alone, now that every kind site is served by a
// source that declares its own cluster need.
func siteNeedsCluster(site CompletionSite, d runtime.Descriptor) bool {
	// The access question commands' resource positional and their
	// --group/--subresource values come from the target cluster's
	// discovery: a display name must resolve first. --as is deliberately
	// absent: it reads management users and needs no cluster.
	if accessQuestionCommand(d.Name) {
		switch site.Kind {
		case SitePositional:
			return site.Index == 1
		case SiteFlagValue:
			return site.FlagName == "group" || site.FlagName == "subresource"
		}
	}
	// Every other legacy site lists without a cluster.
	return false
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

// liveFlagValueCandidates handles the command-shaped flag-value sites the
// legacy path still serves (--machine-config, --cloud-credential, --as,
// --subresource, --group). tail is threaded from liveCandidates for the
// dependent sites. hc is the one HTTP client built per dispatch.
func liveFlagValueCandidates(ctx context.Context, hc *http.Client, site CompletionSite, d runtime.Descriptor, cap TokenCapability, clusterID string, tail []string) []string {
	var raw []string
	switch site.FlagName {
	case "machine-config":
		// Live `<kind>/partial` form only - the bare `<kind>/` prefixes are
		// compiled-in and served statically by machineConfigPrefixCandidates
		// before the network gate. Returns directly: the composed tokens
		// carry a "/" that the shared sanitize/equals-form tail below would
		// drop (the trust boundary lives inside the helper).
		return machineConfigNameCandidates(ctx, hc, site, cap)
	case "cloud-credential":
		raw = fetchCloudCredentials(ctx, hc, cap)
	case "as":
		if accessSubjectCommand(d.Name) {
			// Users only: every shell splits a candidate line at its first
			// ':' (token:description), so a group principal such as
			// github_org://eng cannot be offered by this protocol.
			raw = fetchUsers(ctx, hc, cap)
		}
	case "subresource":
		if accessQuestionCommand(d.Name) {
			raw = fetchSubresources(ctx, hc, cap, clusterID, positionalAt(tail, d.Flags, 1), completion.TailFlag(tail, "group"))
		}
	case "group":
		if accessQuestionCommand(d.Name) {
			raw = fetchAPIGroups(ctx, hc, cap, clusterID)
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
// from here and returns no candidates - a degradation by design, not a
// failure, matching the uncurated-machine-config precedent. A DownstreamOnly
// target (agent, kube-api-auth) needs a cluster typed on the tail; a
// display name is resolved to its cluster id the same way the generic
// pipeline does it - completion must work with whichever spelling the
// operator used for --cluster.
func logsPodCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, tail []string) []string {
	targetTok := firstPositionalToken(tail, d.Flags)
	target, ok := observe.Lookup(targetTok)
	if !ok || target.Namespace == "" {
		return nil
	}

	clusterID := engine.LocalCluster
	if target.DownstreamOnly {
		clusterID = completion.TailFlag(tail, "cluster")
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

// livePositionalCandidates handles the positional sites the legacy path
// still serves: the oidc-client secret verbs' dependent secret name, the
// access questions' resource, and grant's and revoke's subject. Every
// kind's name positional is served by its source before this runs. tail
// is the already-computed leaf arg tail from the callback. hc is the one
// HTTP client built per dispatch.
func livePositionalCandidates(
	ctx context.Context,
	hc *http.Client,
	site CompletionSite,
	d runtime.Descriptor,
	cap TokenCapability,
	clusterID string,
	tail []string,
) []string {
	// The oidc-client secret verbs' second positional is a DEPENDENT site:
	// the secret names of the client already typed as the first, read with
	// a client-scoped GET; a missing or unusable first positional completes
	// to nothing, never an error into the TAB stream.
	if positionalKinds[d.Name] == "oidcclient" && site.Index == 1 {
		return oidcClientSecretNameCandidates(ctx, hc, d, cap, tail)
	}

	if accessQuestionCommand(d.Name) {
		if site.Index != 1 || completion.TailFlag(tail, "non-resource-url") != "" {
			return nil
		}
		// clusterID is the RESOLVED id liveCandidates hands down (a display
		// name already mapped); the raw --cluster token is never a path.
		return fetchBaseResources(ctx, hc, cap, clusterID)
	}

	if site.Index != 0 {
		return nil
	}

	// Subject positional completion for grant/revoke (AC-subject):
	// fetch management users and return the union of name + username.
	// --type group has no listable group source: nil, zero requests.
	// Bare --type with no value yields "" from completion.TailFlag, so user.
	if d.Name == "grant" || d.Name == "revoke" {
		if completion.TailFlag(tail, "type") == "group" {
			return nil
		}
		return fetchUsers(ctx, hc, cap)
	}
	return nil
}

// clusterFamilyPoolCandidates completes a cluster-family verb's --pool
// flag: resolves the typed <cluster> positional to its provisioning Cluster
// object (namespace from --namespace or fleet-default) and lists its
// machine pool names - a DEPENDENT flag site, the same shape the
// oidc-client secret-name completion uses, but keyed off a positional
// rather than another flag. Fully self-contained like logsPodCandidates:
// there is no --cluster flag on these commands, so it never enters the
// generic clusterValue pipeline.
func clusterFamilyPoolCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, tail []string) []string {
	clusterName := firstPositionalToken(tail, d.Flags)
	if clusterName == "" || !isValidSegment(clusterName) {
		return nil
	}
	k, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		return nil
	}
	ns := completion.TailFlag(tail, "namespace")
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
// The kinds are compiled-in literals - every kinds.MachineConfigGroup row,
// the same single source createcluster.go's VALIDATION hint and schema desc
// derive from - not server data, so like resolveStaticEnum the AC-21
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
// appended afterwards - the composed token deliberately carries a "/"
// without widening the global charset, the same doctrine as
// resolveStaticEnum's compiled-in enum values.
//
// Management-plane list on the local cluster: requires a management token -
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
// curated-flag fields - MC-7 rejects a curated key via --set
// unconditionally and MC-8 rejects credential keys, so neither may ever
// be suggested (suggestions valid verbatim). Uncurated kinds (empty
// Known) get no candidates.
//
// OWNERSHIP: the returned bool is true for EVERY --set value site of
// create machine-config - the site is fully static (no live source
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

// oidcClientSecretNameCandidates completes the secret name of the client
// typed as the first positional of oidc-client rotate-secret or
// remove-secret: a GET of that one client, its status.clientSecrets keys.
// Requires a management token.
func oidcClientSecretNameCandidates(ctx context.Context, hc *http.Client, d runtime.Descriptor, cap TokenCapability, tail []string) []string {
	k, ok := kinds.Lookup("oidcclient")
	if !ok {
		return nil
	}
	clientName := firstPositionalToken(tail, d.Flags)
	if clientName == "" || !isValidSegment(clientName) {
		return nil
	}
	if cap.IDOnly {
		return nil
	}
	return fetchOIDCClientSecretNames(ctx, hc, cap, k, clientName)
}

// positionalAt returns the want-th non-flag token of the leaf tail,
// consuming value-taking flags by arity (leaf flags plus globals) and
// skipping --flag=value tokens, or "" when the tail has fewer positionals.
func positionalAt(tail []string, leafFlags []runtime.ArgSpec, want int) string {
	all := append(append([]runtime.ArgSpec{}, leafFlags...), globalArgSpecs...)
	pos := 0
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
		if pos == want {
			return arg
		}
		pos++
	}
	return ""
}

// firstPositionalToken returns the first non-flag token in the leaf tail,
// consuming flag values by arity (leaf flags + globals), or "" when none.
func firstPositionalToken(tail []string, leafFlags []runtime.ArgSpec) string {
	return positionalAt(tail, leafFlags, 0)
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

// isAC10Verb reports whether the descriptor name starts with a generated
// verb (get, patch, delete), whose first positional completes as the kind
// its last word names. A bespoke command completes only through the
// positionalKinds table.
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
