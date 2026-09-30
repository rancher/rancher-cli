package provider

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Identity is `auth status`'s identity probe. It POSTs an empty SelfUser
// to ext.cattle.io/v1 selfusers and returns status.userID. The store is
// create-only (rancher: pkg/ext/stores/selfuser/store.go, no Getter/Lister) -
// the server fills status.userID from the request's bearer identity, so
// an empty spec is all a caller ever sends. Always targets
// engine.LocalCluster, like RancherVersion: SelfUser is a
// management-plane singleton under the local cluster's proxy, not whichever
// downstream cluster eff.Cluster currently points at. Any error (RBAC
// denial - expected under a read-only token, since this is a create-verb
// request; a missing ext.cattle.io group/resource; connectivity) is
// returned clierr-mapped, already-classified for the caller's warn-only
// degradation - this probe's own success never gates auth status's `ok`.
func Identity(ctx context.Context, eff config.Effective) (string, error) {
	dyn, ctx, cancel, err := bootstrapLocalDynamic(ctx, eff)
	if err != nil {
		return "", err
	}
	defer cancel()
	created, err := dyn.Resource(selfuser.GVR).Create(ctx, selfuser.New(), metav1.CreateOptions{FieldManager: FieldManager})
	if err != nil {
		return "", mapErr(err)
	}
	return selfuser.UserID(created), nil
}
