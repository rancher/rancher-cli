package projects

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// projectLabel is the Rancher label namespaces carry to mark their project
// membership (usage.go's mechanism, reused here).
const projectLabel = "field.cattle.io/projectId"

// MatchByIDOrDisplay implements the shared id-or-display-name collision
// algorithm ResolveProject and resolveUser both need, mirroring
// ResolveCluster's collision semantics exactly (internal/cli's
// clientFactory.ResolveCluster): an id match and any display-name matches
// are collected as a union of candidate ids, not an id-wins short-circuit -
// a value matching one item's id AND a different item's displayName is
// ambiguous, not a silent id match. Callers own the entity-specific error
// message; this returns the matched id set (0, 1, or 2+) plus idFound and
// the display-match count for wording it. Most callers want
// MatchOneOrError below, which wraps this and owns the error/candidate
// shaping; the create token path resolves through that wrapper too.
func MatchByIDOrDisplay(nameOrID string, ids []string, displayOf func(id string) string) (matched []string, idFound bool, displayMatchCount int) {
	set := map[string]bool{}
	for _, id := range ids {
		if id == nameOrID {
			set[id] = true
			idFound = true
		}
		if displayOf(id) == nameOrID {
			set[id] = true
			displayMatchCount++
		}
	}
	for id := range set {
		matched = append(matched, id)
	}
	return matched, idFound, displayMatchCount
}

// MatchOneOrError is the shared List-derived-candidates -> MatchByIDOrDisplay
// -> switch assembly ResolveProject, provisioning.resolveCredentialObj, and
// clientFactory.ResolveCluster all use: given the ids/displayOf/candidates
// already built from a List call, it runs MatchByIDOrDisplay and turns the
// result into either the single matched id, or a clierr NOT_FOUND /
// AMBIGUOUS_SELECTION error. Message wording stays entirely the caller's:
// notFoundMsg/notFoundHint are used verbatim, and ambiguousMsg is called
// with MatchByIDOrDisplay's own idFound/displayMatchCount so each caller
// keeps its own id-collision vs. N-way-display-collision wording.
//
// On NOT_FOUND, cands is returned as-is (discovery hints). On ambiguity,
// cands is narrowed to only the entries whose ID is in the matched set and
// sorted by ID for deterministic output - never the full list.
func MatchOneOrError(nameOrID string, ids []string, displayOf func(id string) string, cands []clierr.Candidate, notFoundMsg, notFoundHint string, ambiguousMsg func(idFound bool, displayMatchCount int) string, ambiguousHint string) (string, error) {
	matched, idFound, displayMatchCount := MatchByIDOrDisplay(nameOrID, ids, displayOf)
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", clierr.New(clierr.CodeNotFound, notFoundMsg).
			WithCandidates(cands).
			WithHint(notFoundHint)
	default:
		matchedSet := make(map[string]bool, len(matched))
		for _, id := range matched {
			matchedSet[id] = true
		}
		narrowed := make([]clierr.Candidate, 0, len(matched))
		for _, c := range cands {
			if matchedSet[c.ID] {
				narrowed = append(narrowed, c)
			}
		}
		sort.Slice(narrowed, func(i, j int) bool { return narrowed[i].ID < narrowed[j].ID })
		return "", clierr.New(clierr.CodeAmbiguousSelection, ambiguousMsg(idFound, displayMatchCount)).
			WithCandidates(narrowed).
			WithHint(ambiguousHint)
	}
}

// ResolveProject maps a project display name or id to its metadata.name.
// The list is the primary path: it resolves display names and enforces the
// id-vs-display ambiguity guard. A project MEMBER cannot list the
// collection - member grants are get-on-the-named-object, never
// list-on-the-collection - so a forbidden list falls back to a direct get
// by nameOrID: exact id only, no display-name resolution (impossible
// without the list). Without the fallback, `inspect project <id>` turned a
// question the member is allowed to ask into an RBAC error.
func ResolveProject(ctx context.Context, clients engine.ClusterClientFactory, clusterID, nameOrID string) (string, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", err
	}
	projectKind, _ := kinds.Lookup("project")
	list, err := client.List(ctx, projectKind.GVR, clusterID, "")
	if err != nil {
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeRBAC {
			if _, gerr := ResolveByGet(ctx, client, clusterID, nameOrID, ce); gerr != nil {
				return "", gerr
			}
			return nameOrID, nil
		}
		return "", err
	}

	var ids []string
	displayByID := map[string]string{}
	var cands []clierr.Candidate
	for i := range list.Items {
		id := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "displayName")
		ids = append(ids, id)
		displayByID[id] = display
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
	}

	return MatchOneOrError(nameOrID, ids, func(id string) string { return displayByID[id] }, cands,
		"project not found: "+nameOrID,
		fmt.Sprintf("rancher get projects --cluster %s", clierr.ShellQuote(clusterID)),
		func(idFound bool, displayMatchCount int) string {
			if idFound {
				return fmt.Sprintf("%q matches both a project id and a different project's display name; pass the project id", nameOrID)
			}
			return fmt.Sprintf("%d projects have display name %q; pass the project id", displayMatchCount, nameOrID)
		},
		"")
}

// ResolveByGet is the forbidden-list fallback shared by ResolveProject and
// usage's collectByProject: when a caller cannot LIST the projects
// collection (member grants are get-on-object, never list-on-collection),
// it fetches nameOrID directly. Returns the object on success. On failure:
// a NOT_FOUND when the id does not exist (a typo, or a display name -
// unresolvable without a list; exit 4, never masked as RBAC); listErr
// augmented with the exact-id hint when the get is also forbidden; the
// get's own error verbatim for anything else, so a transient failure keeps
// its code and retryability.
func ResolveByGet(ctx context.Context, client engine.ClusterClient, clusterID, nameOrID string, listErr *clierr.Error) (*unstructured.Unstructured, error) {
	projectKind, _ := kinds.Lookup("project")
	obj, gerr := client.Get(ctx, projectKind.GVR, clusterID, nameOrID)
	if gerr == nil {
		return obj, nil
	}
	ge, isClassified := errors.AsType[*clierr.Error](gerr)
	switch {
	case isClassified && ge.Code == clierr.CodeNotFound:
		return nil, clierr.New(clierr.CodeNotFound, "project not found: "+nameOrID+
			"; you cannot list projects, so display names cannot be resolved - "+
			"pass the exact project id (metadata.name)")
	case isClassified && ge.Code == clierr.CodeRBAC:
		return nil, listErr.WithMessage(listErr.Message +
			"; you cannot list projects; pass the exact project id " +
			"(metadata.name) of a project you have access to")
	default:
		return nil, gerr
	}
}

// BackingNamespace returns the namespace projectroletemplatebindings and
// project-scoped secrets live in for projectID: status.backingNamespace,
// falling back to the project id itself when that field is absent or empty
// (older Rancher, or a project not yet touched by the propagation
// controller) - Rancher's own GetProjectBackingNamespace semantics.
// statusEmpty reports whether that fallback fired, for callers that need to
// know precisely (a project could coincidentally set backingNamespace to
// the project id itself; comparing ns == projectID can't distinguish that
// from an actually-empty status field).
func BackingNamespace(ctx context.Context, clients engine.ClusterClientFactory, clusterID, projectID string) (ns string, statusEmpty bool, err error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", false, err
	}
	projectKind, _ := kinds.Lookup("project")
	obj, err := client.Get(ctx, projectKind.GVR, clusterID, projectID)
	if err != nil {
		return "", false, err
	}
	backing, _, _ := unstructured.NestedString(obj.Object, "status", "backingNamespace")
	if backing == "" {
		return projectID, true, nil
	}
	return backing, false, nil
}

// NotMemberError builds the canonical "namespace is not in project"
// VALIDATION error: the message format Route, usage, and project
// remove-namespace all enforce membership with, kept in one place so a
// wording change never drifts across call sites. detail is call-site
// specific prose (list-scoping advice differs from a mutation-target
// mismatch) appended to the message - never a hint, since neither call
// site's guidance is a single concrete command.
func NotMemberError(namespace, projectID, detail string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("namespace %q is not in project %q; %s", namespace, projectID, detail))
}

// SetMembership stamps obj (a namespace) with the project membership label
// and annotation - the one place the label+annotation encoding lives, so
// create-namespace and add-namespace can never drift on what "member" means.
func SetMembership(obj *unstructured.Unstructured, clusterID, projectID string) {
	_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectLabel)
	_ = unstructured.SetNestedField(obj.Object, clusterID+":"+projectID, "metadata", "annotations", projectLabel)
}

// ClearMembership removes the project membership label and annotation from
// obj (a namespace) - SetMembership's inverse, kept beside it.
func ClearMembership(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "metadata", "labels", projectLabel)
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", projectLabel)
}

// ProjectSelector renders the validated, correctly-escaped equality selector
// for the projectID membership label. labels.NewRequirement rejects a
// malformed project id (e.g. one containing an unescaped comma) with a
// VALIDATION error instead of silently building a selector that matches
// something other than intended.
func ProjectSelector(projectID string) (string, error) {
	req, err := labels.NewRequirement(projectLabel, selection.Equals, []string{projectID})
	if err != nil {
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid project id %q: %v; pass a valid label value", projectID, err))
	}
	return labels.NewSelector().Add(*req).String(), nil
}

// ProjectNamespaces returns the set of namespace names carrying the
// projectID membership label on the downstream cluster. An empty result is
// not an error - callers decide whether that means "no members" or
// "unknown project".
func ProjectNamespaces(ctx context.Context, client engine.ClusterClient, projectID string) (map[string]bool, error) {
	selector, err := ProjectSelector(projectID)
	if err != nil {
		return nil, err
	}

	nsKind, _ := kinds.Lookup("namespace")
	list, err := client.List(ctx, nsKind.GVR, "", selector)
	if err != nil {
		return nil, err
	}

	members := map[string]bool{}
	for i := range list.Items {
		members[list.Items[i].GetName()] = true
	}
	return members, nil
}
