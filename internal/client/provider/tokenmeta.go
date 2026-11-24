package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var extTokensGVR = schema.GroupVersionResource{Group: "ext.cattle.io", Version: "v1", Resource: "tokens"}

// normanTokensPath is the path of Rancher's Norman token API, which lets a
// token's owner call it with no further permission.
const normanTokensPath = "/v3/tokens/"

// maxTokenBody bounds a token response: it lists the user's group principals,
// which can be long.
const maxTokenBody = 1 << 20

// TokenMetadata is the Token metadata (an ext.cattle.io/v1 Token or a v3
// token) `auth status`'s token probe reads back for the EFFECTIVE token (eff.Token, never the
// stored credential directly - RANCHER_CLI_TOKEN overrides the store and this
// must report on whichever one is actually authenticating the request).
// Kind carries the RAW spec.kind value ("session" for Rancher's own login
// flow, empty for CLI-derived tokens per createTokenCmd) - the command
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

// getToken fetches a single ext.cattle.io token object by name via
// bootstrapLocalDynamic, handling client setup and error mapping.
func getToken(ctx context.Context, eff config.Effective, name string) (*unstructured.Unstructured, error) {
	dyn, ctx, cancel, err := bootstrapLocalDynamic(ctx, eff)
	if err != nil {
		return nil, err
	}
	defer cancel()
	obj, err := dyn.Resource(extTokensGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, mapErr(ctx, http.MethodGet, eff, err)
	}
	return obj, nil
}

// EffectiveTokenMetadata GETs the ext.cattle.io/v1 Token named name - the id
// portion of eff.Token after its "ext/" prefix (the same id-shape sniff the
// token package's bearer validation applies to a token the login created,
// here applied to the long-lived effective credential). Callers pre-check
// the prefix themselves; a token without it is read with V3TokenMetadata.
// Always targets engine.LocalCluster, like RancherVersion and Identity.
// Errors are clierr-mapped for the caller's warn-only degradation - this
// probe's own success never gates auth status's `ok`.
func EffectiveTokenMetadata(ctx context.Context, eff config.Effective, name string) (TokenMetadata, error) {
	obj, err := getToken(ctx, eff, name)
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

// v3TokenResponse is the part of the response of Rancher's Norman token API
// that the CLI reads. Norman names the object in both name and id and has no
// metadata.name.
type v3TokenResponse struct {
	Name         string `json:"name"`
	ID           string `json:"id"`
	AuthProvider string `json:"authProvider"`
	ExpiresAt    string `json:"expiresAt"`
	IsDerived    bool   `json:"isDerived"`
	Current      bool   `json:"current"`
}

// V3TokenMetadata reads a management.cattle.io/v3 token with
// GET /v3/tokens/<name> on Rancher's Norman token API, which lets a token's
// owner call it with no further permission. The Kubernetes API of the local
// cluster allows that read only to administrators, so it is not used. The
// request goes over an HTTP client built from the REST config for the local
// cluster, like the ext token read, so it carries the CA of the connection and
// the record of the hosts that responded, and a certificate failure is
// classified the same way. isDerived is mapped
// to Kind ("derived" when true, "session" otherwise), so the return type
// matches EffectiveTokenMetadata and the command layer needs no per-path
// branching. Errors are clierr-mapped for the caller's warn-only degradation.
func V3TokenMetadata(ctx context.Context, eff config.Effective, name string) (TokenMetadata, error) {
	rc, err := localRESTConfig(eff)
	if err != nil {
		return TokenMetadata{}, err
	}
	hc, err := kube.HTTPClient(rc)
	if err != nil {
		return TokenMetadata{}, clierr.From(err)
	}
	path := normanTokensPath + url.PathEscape(name)
	ctx, cancel := opCtx(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, kube.BaseURL(rc.Host)+path, nil)
	if err != nil {
		return TokenMetadata{}, clierr.From(err)
	}
	req.Header.Set("Accept", "application/json")
	q := &clierr.Request{Server: config.DisplayServerURL(eff.URL), Login: eff.LoginRemedy}
	resp, body, tooLong, err := httpx.DoLimited(hc, req, maxTokenBody)
	if err != nil {
		return TokenMetadata{}, mapRequestErr(ctx, http.MethodGet, err, q)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return TokenMetadata{}, clierr.FromResponse("", http.MethodGet, resp.StatusCode, body, q)
	}
	if tooLong {
		return TokenMetadata{}, clierr.New(clierr.CodeGeneric, fmt.Sprintf("the response to GET %s is larger than 1 MiB", path))
	}
	var response v3TokenResponse
	if json.Unmarshal(body, &response) != nil || (response.Name == "" && response.ID == "") {
		return TokenMetadata{}, clierr.New(clierr.CodeGeneric, fmt.Sprintf("the response to GET %s is not a JSON object with a name or id", path))
	}
	tokenName := response.Name
	if tokenName == "" {
		tokenName = response.ID
	}
	kind := "session"
	if response.IsDerived {
		kind = "derived"
	}
	return TokenMetadata{
		Name:      tokenName,
		Kind:      kind,
		Provider:  response.AuthProvider,
		ExpiresAt: response.ExpiresAt,
		Current:   response.Current,
	}, nil
}
