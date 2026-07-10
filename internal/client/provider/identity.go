package provider

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var selfUsersGVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "selfusers"}

// Identity is `auth check`'s PS-2 identity probe. It POSTs an empty SelfUser
// to ext.cattle.io/v1 selfusers and returns status.userID. The store is
// create-only (rancher: pkg/ext/stores/selfuser/store.go, no Getter/Lister)
// — the server fills status.userID from the request's bearer identity, so
// an empty spec is all a caller ever sends. Always targets
// engine.LocalCluster, like ServerVersionSetting: SelfUser is a
// management-plane singleton under the local cluster's proxy, not whichever
// downstream cluster eff.Cluster currently points at. Any error (RBAC
// denial — expected under a read-only token, since this is a create-verb
// request; a missing ext.cattle.io group/resource; connectivity) is
// returned clierr-mapped, already-classified for the caller's warn-only
// degradation — this probe's own success never gates auth check's `ok`.
func Identity(ctx context.Context, eff config.Effective) (string, error) {
	eff.Cluster = engine.LocalCluster
	rc, err := kube.RESTConfig(eff)
	if err != nil {
		return "", err
	}
	dyn, err := kube.Dynamic(rc)
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ext.cattle.io/v1",
		"kind":       "SelfUser",
	}}
	created, err := dyn.Resource(selfUsersGVR).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return "", mapErr(err)
	}
	userID, _, _ := unstructured.NestedString(created.Object, "status", "userID")
	return userID, nil
}
