package projects

import (
	"context"
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
// algorithm resolveUser and MatchOneOrError both need, mirroring
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
// -> switch assembly provisioning.resolveCredentialObj, the exec plugin's
// cluster lookup, and clientFactory.ResolveCluster all use: given the ids/displayOf/candidates
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
