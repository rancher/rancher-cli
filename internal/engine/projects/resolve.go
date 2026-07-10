package projects

import (
	"context"
	"fmt"

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

// ResolveProject maps a project display name or id to its metadata.name,
// mirroring ResolveCluster's collision semantics exactly (surface/cli's
// clientFactory.ResolveCluster): an id match and any display-name matches
// are collected as a union of candidate ids, not an id-wins short-circuit —
// a value matching one project's id AND a different project's displayName
// is ambiguous, not a silent id match.
func ResolveProject(ctx context.Context, clients engine.ClusterClientFactory, clusterID, nameOrID string) (string, error) {
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", err
	}
	projectKind, _ := kinds.Lookup("project")
	list, err := client.List(ctx, projectKind.GVR, clusterID, "")
	if err != nil {
		return "", err
	}

	var idMatch string
	var idFound bool
	var displayMatches []string
	var cands []clierr.Candidate
	for i := range list.Items {
		id := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "displayName")
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
		if id == nameOrID {
			idMatch, idFound = id, true
		}
		if display == nameOrID {
			displayMatches = append(displayMatches, id)
		}
	}

	matched := map[string]bool{}
	if idFound {
		matched[idMatch] = true
	}
	for _, id := range displayMatches {
		matched[id] = true
	}
	var ids []string
	for id := range matched {
		ids = append(ids, id)
	}

	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", clierr.New(clierr.CodeNotFound, "project not found: "+nameOrID).
			WithCandidates(cands).
			WithHint(fmt.Sprintf("run: rancher get projects --cluster %s", clusterID))
	default:
		msg := fmt.Sprintf("%d projects have display name %q", len(displayMatches), nameOrID)
		if idFound {
			msg = fmt.Sprintf("%q matches both a project id and a different project's display name", nameOrID)
		}
		return "", clierr.New(clierr.CodeAmbiguousSelection, msg).
			WithCandidates(cands).WithHint("pass the project id")
	}
}

// NotMemberError builds the canonical "namespace is not in project"
// VALIDATION error: the message format Route, usage, and project
// remove-namespace all enforce membership with, kept in one place so a
// wording change never drifts across call sites. hint is call-site
// specific (list-scoping advice differs from a mutation-target mismatch).
func NotMemberError(namespace, projectID, hint string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("namespace %q is not in project %q", namespace, projectID)).
		WithHint(hint)
}

// SetMembership stamps obj (a namespace) with the project membership label
// and annotation — the one place the label+annotation encoding lives, so
// create-namespace and add-namespace can never drift on what "member" means.
func SetMembership(obj *unstructured.Unstructured, clusterID, projectID string) {
	_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectLabel)
	_ = unstructured.SetNestedField(obj.Object, clusterID+":"+projectID, "metadata", "annotations", projectLabel)
}

// ClearMembership removes the project membership label and annotation from
// obj (a namespace) — SetMembership's inverse, kept beside it.
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
			fmt.Sprintf("invalid project id %q: %v", projectID, err)).
			WithHint("pass a valid label value")
	}
	return labels.NewSelector().Add(*req).String(), nil
}

// ProjectNamespaces returns the set of namespace names carrying the
// projectID membership label on the downstream cluster. An empty result is
// not an error — callers decide whether that means "no members" or
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
