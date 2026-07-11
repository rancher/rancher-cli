package provider

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var extTokensGVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "tokens"}

// TokenMetadata is the ext.cattle.io/v1 Token metadata `auth check`'s PS-2
// token probe reads back for the EFFECTIVE token (eff.Token, never the
// stored credential directly — RANCHER_CLI_TOKEN overrides the store and this
// must report on whichever one is actually authenticating the request).
// Kind carries the RAW spec.kind value ("session" for Rancher's own login
// flow, empty for CLI-derived tokens per createTokenCmd) — the command
// layer, not this probe, turns it into a display label, the same
// fetch-raw/interpret-in-the-command split ServerVersionSetting uses for
// checkMinRancherVersion.
type TokenMetadata struct {
	Name      string
	Kind      string
	Provider  string
	ExpiresAt string
	Current   bool
}

// EffectiveTokenMetadata GETs the ext.cattle.io/v1 Token named name — the id
// portion of eff.Token after its "ext/" prefix (the same id-shape sniff
// login_ext.go's revokeSessionToken applies to the transient session token,
// here applied to the long-lived effective credential). Callers pre-check
// the prefix themselves and never call this for a legacy/norman token (PS-2:
// no v3/legacy fallback). Always targets engine.LocalCluster, like
// ServerVersionSetting and Identity. Errors are clierr-mapped for the
// caller's warn-only degradation — this probe's own success never gates
// auth check's `ok`.
func EffectiveTokenMetadata(ctx context.Context, eff config.Effective, name string) (TokenMetadata, error) {
	dyn, ctx, cancel, err := bootstrapLocalDynamic(ctx, eff)
	if err != nil {
		return TokenMetadata{}, err
	}
	defer cancel()
	obj, err := dyn.Resource(extTokensGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return TokenMetadata{}, mapErr(err)
	}
	kind, _, _ := unstructured.NestedString(obj.Object, "spec", "kind")
	providerName, _, _ := unstructured.NestedString(obj.Object, "spec", "userPrincipal", "provider")
	expiresAt, _, _ := unstructured.NestedString(obj.Object, "status", "expiresAt")
	current, _, _ := unstructured.NestedBool(obj.Object, "status", "current")
	return TokenMetadata{
		Name:      obj.GetName(),
		Kind:      kind,
		Provider:  providerName,
		ExpiresAt: expiresAt,
		Current:   current,
	}, nil
}
