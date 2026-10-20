package projects

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// projectKey names both the annotation and the label a namespace carries
// for its project. The annotation holds <cluster>:<project> and is what
// Rancher reads; the label holds the bare project id and only narrows a
// server-side list.
const projectKey = "field.cattle.io/projectId"

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
	_ = unstructured.SetNestedField(obj.Object, projectID, "metadata", "labels", projectKey)
	_ = unstructured.SetNestedField(obj.Object, clusterID+":"+projectID, "metadata", "annotations", projectKey)
}

// ClearMembership removes the project membership label and annotation from
// obj (a namespace) - SetMembership's inverse, kept beside it.
func ClearMembership(obj *unstructured.Unstructured) {
	unstructured.RemoveNestedField(obj.Object, "metadata", "labels", projectKey)
	unstructured.RemoveNestedField(obj.Object, "metadata", "annotations", projectKey)
}

// ProjectOf reads the project a namespace belongs to from its
// field.cattle.io/projectId annotation, <cluster>:<project>. ok is false
// when the annotation is absent or does not have that form. It is the only
// reader of the encoding: the label of the same key is not consulted.
func ProjectOf(ns *unstructured.Unstructured) (clusterID, projectID string, ok bool) {
	value := ns.GetAnnotations()[projectKey]
	clusterID, projectID, found := strings.Cut(value, ":")
	if !found || clusterID == "" || projectID == "" || strings.Contains(projectID, ":") {
		return "", "", false
	}
	return clusterID, projectID, true
}

// IsMember reports whether ns belongs to projectID on clusterID. A
// namespace whose annotation names another cluster is a member of nothing
// on this one.
func IsMember(ns *unstructured.Unstructured, clusterID, projectID string) bool {
	c, p, ok := ProjectOf(ns)
	return ok && c == clusterID && p == projectID
}

// MemberSelector renders the validated, correctly-escaped equality selector
// on the project label. It only narrows a server-side namespace list: the
// label is not the membership, so every caller filters the answer with
// IsMember. labels.NewRequirement rejects a malformed project id (e.g. one
// containing an unescaped comma) with a VALIDATION error instead of
// silently building a selector that matches something other than intended.
func MemberSelector(projectID string) (string, error) {
	req, err := labels.NewRequirement(projectKey, selection.Equals, []string{projectID})
	if err != nil {
		return "", clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid project id %q: %v; pass a valid label value", projectID, err))
	}
	return labels.NewSelector().Add(*req).String(), nil
}
