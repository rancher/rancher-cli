package provider

import (
	"context"

	"github.com/rancher/rancher-cli/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var extTokensGVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "tokens"}

var mgmtTokensGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "tokens"}

// TokenMetadata is the ext.cattle.io/v1 Token metadata `auth check`'s PS-2
// token probe reads back for the EFFECTIVE token (eff.Token, never the
// stored credential directly — RANCHER_CLI_TOKEN overrides the store and this
// must report on whichever one is actually authenticating the request).
// Kind carries the RAW spec.kind value ("session" for Rancher's own login
// flow, empty for CLI-derived tokens per createTokenCmd) — the command
// layer, not this probe, turns it into a display label, the same
// fetch-raw/interpret-in-the-command split RancherVersion uses for
// checkMinRancherVersion.
type TokenMetadata struct {
	Name      string
	Kind      string
	Provider  string
	ExpiresAt string
	Current   bool
}

// getToken fetches a single token object from the given GVR by name via
// bootstrapLocalDynamic, handling client setup and error mapping. It is the
// shared core of EffectiveTokenMetadata and V3TokenMetadata, which differ
// only in which GVR they target and which root fields they read afterward.
func getToken(ctx context.Context, eff config.Effective, gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
	dyn, ctx, cancel, err := bootstrapLocalDynamic(ctx, eff)
	if err != nil {
		return nil, err
	}
	defer cancel()
	obj, err := dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapErr(err)
	}
	return obj, nil
}

// EffectiveTokenMetadata GETs the ext.cattle.io/v1 Token named name — the id
// portion of eff.Token after its "ext/" prefix (the same id-shape sniff
// login_ext.go's revokeSessionToken applies to the transient session token,
// here applied to the long-lived effective credential). Callers pre-check
// the prefix themselves; non-ext tokens fall through to V3TokenMetadata.
// Always targets engine.LocalCluster, like RancherVersion and Identity.
// Errors are clierr-mapped for the caller's warn-only degradation — this
// probe's own success never gates auth check's `ok`.
func EffectiveTokenMetadata(ctx context.Context, eff config.Effective, name string) (TokenMetadata, error) {
	obj, err := getToken(ctx, eff, extTokensGVR, name)
	if err != nil {
		return TokenMetadata{}, err
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

// V3TokenMetadata GETs the management.cattle.io/v3 Token named name for
// `auth check`'s PS-2 token probe fallback. The v3 Token carries all five
// output fields at the object root (not under spec/status): authProvider,
// expiresAt, isDerived, current, and metadata.name. isDerived is mapped to
// Kind ("derived" when true, "session" otherwise) so the return type is
// compatible with EffectiveTokenMetadata and the command layer needs no
// per-path branching. Always targets engine.LocalCluster. Errors are
// clierr-mapped for the caller's warn-only degradation.
func V3TokenMetadata(ctx context.Context, eff config.Effective, name string) (TokenMetadata, error) {
	obj, err := getToken(ctx, eff, mgmtTokensGVR, name)
	if err != nil {
		return TokenMetadata{}, err
	}
	authProvider, _, _ := unstructured.NestedString(obj.Object, "authProvider")
	expiresAt, _, _ := unstructured.NestedString(obj.Object, "expiresAt")
	isDerived, _, _ := unstructured.NestedBool(obj.Object, "isDerived")
	current, _, _ := unstructured.NestedBool(obj.Object, "current")
	kind := "session"
	if isDerived {
		kind = "derived"
	}
	return TokenMetadata{
		Name:      obj.GetName(),
		Kind:      kind,
		Provider:  authProvider,
		ExpiresAt: expiresAt,
		Current:   current,
	}, nil
}
