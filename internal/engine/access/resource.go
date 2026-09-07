package access

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// ResourceRef is one discovered resource in its preferred version, the
// shape every review and output question is built from.
type ResourceRef struct {
	Group        string
	Version      string
	Resource     string
	Kind         string
	Namespaced   bool
	Subresources []string
}

// ResourceVerbs is the completion vocabulary for resource questions;
// validation accepts any well-formed token because authorizers may define
// more (Rancher's own verbs such as manage-users are ordinary rule verbs).
var ResourceVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection", "impersonate", "escalate", "bind"}

// HTTPVerbs is the closed grammar for --non-resource-url questions.
var HTTPVerbs = []string{"get", "head", "post", "put", "patch", "delete", "connect", "options", "trace"}

var verbShape = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// NormalizeVerb folds the verb to lowercase and validates it against the
// grammar the question uses.
func NormalizeVerb(verb string, nonResource bool) (string, error) {
	v := strings.ToLower(strings.TrimSpace(verb))
	if nonResource {
		if !slices.Contains(HTTPVerbs, v) {
			return "", clierr.New(clierr.CodeValidation,
				fmt.Sprintf("%q is not an HTTP verb; a --non-resource-url question uses one of %s", verb, strings.Join(HTTPVerbs, ", ")))
		}
		return v, nil
	}
	if !verbShape.MatchString(v) {
		return "", clierr.New(clierr.CodeValidation, fmt.Sprintf("%q is not a verb; use a lowercase word such as get, list, or delete", verb))
	}
	return v, nil
}

// ResolveResource maps what the user typed (plural, singular, short name,
// or resource.group) to one discovered resource. group, when set, breaks
// a tie between API groups. Subresource entries (pods/log) are folded into
// their base and are never candidates themselves.
//
// Discovery may be incomplete (the client returns the partial list with an
// *engine.DiscoveryIncomplete). Then: a name the healthy groups do not
// serve is a retryable error naming the failed groups, never "unknown
// resource"; a qualified name whose group failed is the same error; a
// bare name with exactly one healthy match resolves, and the returned
// warning says which groups were not consulted, so the caller shows it
// beside the answer. Core-group precedence is unchanged.
func ResolveResource(ctx context.Context, c engine.ClusterClient, arg, group string) (ResourceRef, string, error) {
	if arg == "" {
		return ResourceRef{}, "", clierr.New(clierr.CodeValidation, "a resource is required")
	}
	all, err := c.ServerResources(ctx)
	var incomplete *engine.DiscoveryIncomplete
	if err != nil {
		inc, partial := errors.AsType[*engine.DiscoveryIncomplete](err)
		if !partial {
			return ResourceRef{}, "", err
		}
		incomplete = inc
	}
	return resolveResource(all, incomplete, arg, group)
}

// ResolveResourceIn applies the resolution rules to an already fetched,
// complete discovery list. Completion uses it so a suggestion follows
// exactly the rules the command applies.
func ResolveResourceIn(all []engine.APIResource, arg, group string) (ResourceRef, error) {
	ref, _, err := resolveResource(all, nil, arg, group)
	return ref, err
}

// incompleteWarning is the caveat a resolved bare name carries when some
// groups were not described: the answer assumed they serve nothing by
// that name. A qualified name is exact, and a core-group match wins by
// rule whatever another group serves, so neither carries it.
func incompleteWarning(incomplete *engine.DiscoveryIncomplete, arg string, ref ResourceRef, groupSet bool) string {
	if incomplete == nil || groupSet || ref.Group == "" {
		return ""
	}
	return fmt.Sprintf("discovery for API group(s) %s failed; %s resolved to %s.%s assuming the failed group(s) serve no such resource",
		strings.Join(incomplete.Groups(), ", "), arg, ref.Resource, ref.Group)
}

// withDiscoveryWarning attaches the resolver's caveat to an answer, so
// every rendered answer built on a name resolved under incomplete
// discovery carries it. An empty warning leaves the result alone.
func withDiscoveryWarning(res runtime.Result, warning string) runtime.Result {
	if warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res
}

// resolveResource applies the resolution rules to a discovery list that
// may be incomplete.
func resolveResource(all []engine.APIResource, incomplete *engine.DiscoveryIncomplete, arg, group string) (ResourceRef, string, error) {
	if arg == "" {
		return ResourceRef{}, "", clierr.New(clierr.CodeValidation, "a resource is required")
	}
	name, dotGroup := arg, ""
	if i := strings.IndexByte(arg, '.'); i > 0 && !strings.Contains(arg, "/") {
		name, dotGroup = arg[:i], arg[i+1:]
	}
	wantGroup, groupSet := group, group != ""
	if dotGroup != "" {
		if groupSet && dotGroup != group {
			return ResourceRef{}, "", clierr.New(clierr.CodeValidation, fmt.Sprintf("%s names group %s but --group says %s", arg, dotGroup, group))
		}
		wantGroup, groupSet = dotGroup, true
	}
	if wantGroup == "core" {
		wantGroup = ""
	}
	bases := map[string]*ResourceRef{} // "group/resource" -> ref
	var order []string
	for _, r := range all {
		if strings.Contains(r.Name, "/") {
			continue
		}
		key := r.Group + "/" + r.Name
		if _, seen := bases[key]; seen {
			continue
		}
		ref := &ResourceRef{Group: r.Group, Version: r.Version, Resource: r.Name, Kind: r.Kind, Namespaced: r.Namespaced}
		bases[key] = ref
		order = append(order, key)
	}
	for _, r := range all {
		base, sub, ok := strings.Cut(r.Name, "/")
		if !ok {
			continue
		}
		if ref := bases[r.Group+"/"+base]; ref != nil {
			ref.Subresources = append(ref.Subresources, sub)
		}
	}
	var matches []*ResourceRef
	for _, key := range order {
		ref := bases[key]
		if groupSet && ref.Group != wantGroup {
			continue
		}
		if matchesAlias(all, ref, name) {
			matches = append(matches, ref)
		}
	}
	if len(matches) > 1 && !groupSet {
		// A bare name that the core group serves resolves to core; other
		// groups need --group or name.group (kubectl's rule).
		for _, m := range matches {
			if m.Group == "" {
				matches = []*ResourceRef{m}
				break
			}
		}
	}
	switch len(matches) {
	case 1:
		slices.Sort(matches[0].Subresources)
		return *matches[0], incompleteWarning(incomplete, arg, *matches[0], groupSet), nil
	case 0:
		if incomplete != nil && (!groupSet || incomplete.GroupFailed(wantGroup)) {
			// The healthy groups do not serve the name and a group the
			// server could not describe might: not unknown, undetermined.
			return ResourceRef{}, "", clierr.Retryable(clierr.CodeGeneric,
				fmt.Sprintf("%s was not found in the API groups the server could describe, and discovery for %s failed; it may live there - retry, or check those groups' APIServices",
					arg, strings.Join(incomplete.Groups(), ", ")))
		}
		return ResourceRef{}, "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("unknown resource %q on this cluster; closest names: %s", arg, strings.Join(closest(order, bases, name), ", ")))
	default:
		groups := make([]string, 0, len(matches))
		for _, m := range matches {
			g := m.Group
			if g == "" {
				g = "core"
			}
			groups = append(groups, g)
		}
		// Discovery order is not stable across servers; the message is.
		slices.Sort(groups)
		groups = slices.Compact(groups)
		return ResourceRef{}, "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("%s exists in groups %s; add --group <group> or write %s.<group>", name, strings.Join(groups, ", "), name))
	}
}

// matchesAlias applies the documented grammar only: plural, singular, or
// short name, each matched exactly as discovery spells it (the group comes
// from name.group or --group). A Kubernetes Kind spelling is not an alias,
// which is why the comparison is case sensitive.
func matchesAlias(all []engine.APIResource, ref *ResourceRef, name string) bool {
	if ref.Resource == name {
		return true
	}
	for _, r := range all {
		if r.Group != ref.Group || r.Name != ref.Resource {
			continue
		}
		if r.SingularName == name || slices.Contains(r.ShortNames, name) {
			return true
		}
	}
	return false
}

// closest returns up to three base resource names with the smallest edit
// distance to the typed name, for the validation message.
func closest(order []string, bases map[string]*ResourceRef, name string) []string {
	type cand struct {
		name string
		d    int
	}
	var cands []cand
	for _, key := range order {
		n := bases[key].Resource
		cands = append(cands, cand{n, editDistance(strings.ToLower(name), n)})
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.d != b.d {
			return a.d - b.d
		}
		return strings.Compare(a.name, b.name)
	})
	out := []string{}
	for _, c := range cands {
		if len(out) == 3 {
			break
		}
		if !slices.Contains(out, c.name) {
			out = append(out, c.name)
		}
	}
	return out
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
