package cli

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/engine/access"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	ucli "github.com/urfave/cli/v3"
)

// universalShellCompleteFunc returns a urfave ShellCompleteFunc that
// dispatches TAB completions in order: (1) arg-tail site parse, (2) enum
// static, (3) config-local (ShellCompleter delegation), (4) live (gated on
// completion.network). Installed on EVERY adapted leaf. lc is the
// command's own live source owner, nil for a command without one.
func universalShellCompleteFunc(d runtime.Descriptor, sc runtime.ShellCompleter, lc completion.Completer) ucli.ShellCompleteFunc {
	return func(ctx context.Context, c *ucli.Command) {
		// Strip command routing tokens from rawArgv using the invoked urfave
		// command's lineage depth - NOT the canonical descriptor name string.
		// This handles leading global flags (--output json get pod) and alias
		// invocations (inspect po → depth 2) correctly.
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
	// Layer 1: enum static, for flag values AND positionals.
	if site.Kind == SiteFlagValue {
		if cands := enumCandidates(site, d); len(cands) > 0 {
			return cands
		}
		// The access commands' --timeout: a static ladder of durations,
		// owned here like an enum (no network, no delegation).
		if accessTimeoutSite(site, d) {
			return resolveStaticEnum(accessTimeoutSuggestions, site)
		}
		// `auth login --credential-store`: the three stores that can be
		// named without knowing anything about the machine. The flag
		// carries no enum, because a typed exec:<name> has to be accepted.
		if credentialStoreSite(site, d) {
			return resolveStaticEnum(credentialStoreSuggestions, site)
		}
		// kubeconfig --exec-command: PATH names for a bare word, nothing
		// for a path, so the shell completes files. Owned even when empty.
		if execCommandSite(site, d) {
			return execCommandCandidates(site)
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

// completionNetworkEnabled reports whether live completion is configured;
// dispatchSite's live layer runs only when it is.
func completionNetworkEnabled() bool {
	cfg := storedConfigSnapshot()
	return cfg != nil && cfg.Completion.Network
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

// completionCharset is the allowlist for candidate values. The
// first character must not be a hyphen, which would parse as a flag.
var completionCharset = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// sanitizeCandidates drops values that do not conform to the shell-safe
// charset and returns the survivors sorted.
func sanitizeCandidates(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if completionCharset.MatchString(c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out
}

// resolveStaticEnum returns enum candidates for a flag-value site, sorted,
// as bare values in the space form and as full --flag=value tokens in the
// equals form. Static enums are compiled-in literals, not server data, so
// the shell-safe charset does not gate them: "template={{.}}" must
// survive. Server-derived values pass sanitizeCandidates instead: the
// config-local layer's in dispatchSite, a source's in emitResult.
func resolveStaticEnum(enum []string, site CompletionSite) []string {
	sorted := slices.Clone(enum)
	slices.Sort(sorted)
	if site.EqualsForm {
		out := make([]string, 0, len(sorted))
		for _, v := range sorted {
			out = append(out, site.FlagSpelling+"="+v)
		}
		return out
	}
	return sorted
}

// positionalEnumCandidates returns static enum candidates for a positional
// site - Descriptor.Args[site.Index].Enum filtered by the typed partial -
// plus an OWNERSHIP flag: owned is true whenever the indexed arg
// carries an Enum, in which case the caller must terminate the dispatch
// with these candidates even when the filter matched nothing (an unmatched
// partial must not fall through to the delegation/live layers and
// resurface an unfiltered list). Index-aware by construction, so an
// enum-less positional (a <name> site) or an index past the
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

// credentialStoreSuggestions are the static --credential-store
// candidates: the stores a value can name on its own. A credential
// helper is typed as exec:<name> and is never suggested, because a
// suggestion has to be valid verbatim and no helper name can be offered
// without probing for it.
var credentialStoreSuggestions = []string{"auto", "file", "keychain"}

// credentialStoreSite reports a --credential-store flag-value site on a
// command whose descriptor carries the flag. Ownership follows the
// descriptor, so a command without the flag gets no list.
func credentialStoreSite(site CompletionSite, d runtime.Descriptor) bool {
	return site.Kind == SiteFlagValue && site.FlagName == "credential-store" &&
		slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == "credential-store" })
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

// liveCandidates answers a live site through the completion source that
// claims it - the command's own, a shared flag's, or a kind's - under the
// source's declared needs and allowance. A site no source claims answers
// nothing: every live site has its owner, and the surface builds no
// endpoint path of its own. tail is the pre-computed leaf arg tail from
// the caller; lc is the command's live source owner, nil for none.
func liveCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, lc completion.Completer, tail []string) []string {
	cands, owned := liveSourceCandidates(ctx, site, d, lc, tail, flagSources, kindSources, positionalKinds)
	if !owned {
		completionDebugf("stop: no completion source claims the site")
	}
	return cands
}

// isClusterIDShaped reports whether s looks like a Rancher management cluster ID
// (starts with "c-" or is "local"). Display names rarely start with "c-".
func isClusterIDShaped(s string) bool {
	return s == "local" || strings.HasPrefix(s, "c-")
}

// resolveDisplayNameToClusterID issues the surface's one request of its
// own: the management cluster list, read through the seam under an
// allowance of one, to map a typed display name to a cluster id. Returns
// "" when no cluster or more than one carries the name, or the request
// fails. hc is the single per-dispatch client.
func resolveDisplayNameToClusterID(ctx context.Context, hc *http.Client, cap TokenCapability, displayName string) string {
	k, ok := kinds.ByKey("managementcluster")
	if !ok {
		return ""
	}
	p, err := completion.ManagementPath(k.GVR.Group, k.GVR.Version, k.GVR.Resource)
	if err != nil {
		return ""
	}
	var col completion.ItemList
	if err := tracedConnection(hc, cap, 1).GetJSON(ctx, p, "", &col); err != nil {
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

// machineConfigPrefixCandidates serves the `<kind>/` prefixes for create
// cluster's --machine-config flag while the typed value has no "/" yet.
// The kinds are compiled-in literals - every kinds.MachineConfigGroup row,
// the same single source createcluster.go's VALIDATION hint and schema desc
// derive from - not server data, so like resolveStaticEnum the
// shell-safe charset does not gate them: the "/" (and trailing "/") must
// survive. Returns nil for every other site; create cluster's own
// completion source owns the `<kind>/partial` form. This projection and
// the --set key projection below are the surface's only compiled-in
// knowledge of the machine-config kinds; no live path here names them.
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

// machineConfigSetKeyCandidates serves kind-scoped `key=` continuation
// tokens for create machine-config's --set flag: the kind is read
// from the typed tail's first positional, and the candidates are that
// kind's Known driver fields MINUS the credential fields MINUS the
// curated-flag fields - create machine-config rejects a curated key via --set
// unconditionally and rejects credential keys, so neither may ever
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
// and no network gate applies. Curated-value completion is not offered.
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

// positionals returns the non-flag tokens of the leaf tail in order,
// consuming value-taking flags by arity (leaf flags plus globals) and
// skipping --flag=value tokens. The surface alone knows the arity, so it
// fills completion.Request.Positionals from here for the sources whose
// site depends on an earlier positional.
func positionals(tail []string, leafFlags []runtime.ArgSpec) []string {
	all := append(append([]runtime.ArgSpec{}, leafFlags...), globalArgSpecs...)
	var out []string
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
		out = append(out, arg)
	}
	return out
}

// firstPositionalToken returns the first positional of the leaf tail, or
// "" when it has none.
func firstPositionalToken(tail []string, leafFlags []runtime.ArgSpec) string {
	if p := positionals(tail, leafFlags); len(p) > 0 {
		return p[0]
	}
	return ""
}

// isGeneratedVerb reports whether the descriptor name starts with a generated
// verb (get, patch, delete), whose first positional completes as the kind
// its last word names. A bespoke command completes only through the
// positionalKinds table.
func isGeneratedVerb(descriptorName string) bool {
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
// invoked urfave command's Lineage depth. This correctly handles:
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
