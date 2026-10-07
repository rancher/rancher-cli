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
