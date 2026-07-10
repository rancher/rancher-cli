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

var serverVersionSettingGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "settings"}

// ServerVersionSetting reads the server-version Setting for `auth check`'s
// PS-8 posture check, through the same RESTConfig/Dynamic building blocks
// Preflight already uses in this package (auth check's existing client
// path — no direct HTTP here, unlike login). It always targets
// engine.LocalCluster regardless of eff.Cluster: Settings are
// management-plane singletons that live under the LOCAL cluster's proxy,
// not whichever downstream cluster the caller's context currently points
// at (unlike Preflight's discovery ServerVersion, which is deliberately
// scoped to the connected cluster). Any error (RBAC denial, a missing
// setting, connectivity) is returned as-is, already clierr-mapped — the
// caller folds it into PS-8's warn-only "cannot determine" posture rather
// than failing the check.
func ServerVersionSetting(ctx context.Context, eff config.Effective) (string, error) {
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
	obj, err := dyn.Resource(serverVersionSettingGVR).Get(ctx, "server-version", metav1.GetOptions{})
	if err != nil {
		return "", mapErr(err)
	}
	value, _, _ := unstructured.NestedString(obj.Object, "value")
	return value, nil
}
