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
// mirroring ResolveCluster's contract but with an exact-id short-circuit:
// an id match wins immediately, before any display-name comparison.
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

	var displayMatches []string
	var cands []clierr.Candidate
	for i := range list.Items {
		id := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "displayName")
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
		if id == nameOrID {
			return id, nil
		}
		if display == nameOrID {
			displayMatches = append(displayMatches, id)
		}
	}

	switch len(displayMatches) {
	case 1:
		return displayMatches[0], nil
	case 0:
		return "", clierr.New(clierr.CodeNotFound, "project not found: "+nameOrID).
			WithCandidates(cands).
			WithHint(fmt.Sprintf("run: rancher get projects --cluster %s", clusterID))
	default:
		return "", clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d projects have display name %q", len(displayMatches), nameOrID)).
			WithCandidates(cands).WithHint("pass the project id")
	}
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
