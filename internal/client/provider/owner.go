package provider

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// Owner is auth login's check of a pasted token: one SelfSubjectReview on
// the local cluster's path, which proves the server accepts the token and
// reports its owner as status.userInfo.username, the name Rancher gives
// the request's user from the token. Failures map as Preflight's do.
func Owner(ctx context.Context, eff config.Effective) (string, error) {
	rc, err := localRESTConfig(eff)
	if err != nil {
		return "", err
	}
	dyn, err := kube.Dynamic(discoveryConfig(rc))
	if err != nil {
		return "", clierr.From(err)
	}
	gvr, _ := engine.ReviewGVR(engine.ReviewSelfSubjectIdentity)
	review := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}
	created, err := dyn.Resource(gvr).Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "", clierr.WrapUnlessCanceled(err, mapPreflightErr)
	}
	username, _, _ := unstructured.NestedString(created.Object, "status", "userInfo", "username")
	if username == "" {
		return "", clierr.New(clierr.CodeGeneric, "the server's SelfSubjectReview named no user")
	}
	return username, nil
}
