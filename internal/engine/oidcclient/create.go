package oidcclient

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// defaultTokenTTLSeconds/defaultRefreshTTLSeconds are the CLI's OWN defaults
// for the CRD-required TTL fields - the server does not default them: a
// create missing either field fails against a real server, on both the
// Prime and community editions.
const (
	defaultTokenTTLSeconds   = int64(3600)
	defaultRefreshTTLSeconds = int64(86400)
)

type createCmd struct {
	clients    engine.ClusterClientFactory
	roots      engine.RootClientFactory
	serverBase engine.ServerBaseURLProvider
}

// NewCreate builds `create oidc-client <name> [--description] [--redirect-uri]...
// [--scope]... [--token-ttl] [--refresh-ttl] [--timeout]` (mutating): the
// curated create for Rancher's embedded OIDC provider, structurally
// precedented by `create token` (a single-backing-row derived op with
// flag→spec mapping and a once-only sensitive value) - but, unlike the ext
// Token store, OIDCClient is a plain CRD with no name generation, so <name>
// is a required, user-chosen positional.
func NewCreate(clients engine.ClusterClientFactory, roots engine.RootClientFactory, serverBase engine.ServerBaseURLProvider) runtime.Command {
	return &createCmd{clients: clients, roots: roots, serverBase: serverBase}
}

func (createCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "create oidc-client",
		Usage: "Create an OIDC client for Rancher's embedded OIDC provider",
		Desc: "Creates the client, waits for the server to provision it, reads the initial " +
			"client secret once, and reads the live discovery document - printing a complete " +
			"relying-app configuration (clientID, clientSecret, issuer, endpoints) in one document.",
		CommandType: runtime.Mutating,
		Args: []runtime.ArgSpec{
			{Name: "name", Type: "string", Required: true, Desc: "OIDC client name (user-chosen; the server does not generate one)"},
		},
		Flags: []runtime.ArgSpec{
			{Name: "description", Type: "string", Desc: "note attached to the client for identification"},
			{Name: "redirect-uri", Type: "stringSlice", Desc: "allowed redirect `URI` (repeatable)"},
			{Name: "scope", Type: "stringSlice", Desc: "OAuth `scope` granted to the client (repeatable)"},
			{Name: "token-ttl", Type: "string", Desc: "access-token lifetime, a positive whole number of seconds as a duration, e.g. 1h (omit for the CLI default, 3600s/1h)"},
			{Name: "refresh-ttl", Type: "string", Desc: "refresh-token lifetime, a positive whole number of seconds as a duration, e.g. 24h (omit for the CLI default, 86400s/24h)"},
			{Name: "timeout", Type: "string", Desc: "wait `timeout` for the server to provision the client, e.g. 90s (default 60s)"},
		},
		Examples: []string{
			"rancher create oidc-client my-app --redirect-uri https://example.invalid/callback --apply --yes",
		},
		Destructive:  "true",
		Auth:         true,
		Capability:   capability(),
		OutputPolicy: runtime.OnceOnlyOutputPolicy(),
		Delivery:     runtime.DeliveryOnceOnly,
		Shape:        runtime.ShapeText,
	}
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func parseWaitTimeout(in runtime.Input) (time.Duration, error) {
	return runtime.DurationFlag(in, "timeout", defaultWaitTimeout, "90s")
}

func (c *createCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	name := in.Args()[0]

	tokenTTL, err := strictTTLSeconds("token-ttl", in.String("token-ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if tokenTTL == 0 {
		tokenTTL = defaultTokenTTLSeconds
	}
	refreshTTL, err := strictTTLSeconds("refresh-ttl", in.String("refresh-ttl"))
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if refreshTTL == 0 {
		refreshTTL = defaultRefreshTTLSeconds
	}
	timeout, err := parseWaitTimeout(in)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	spec := map[string]any{
		"tokenExpirationSeconds":        tokenTTL,
		"refreshTokenExpirationSeconds": refreshTTL,
	}
	if desc := in.String("description"); desc != "" {
		spec["description"] = desc
	}
	if uris := in.Strings("redirect-uri"); len(uris) > 0 {
		spec["redirectURIs"] = toAnySlice(uris)
	}
	if scopes := in.Strings("scope"); len(scopes) > 0 {
		spec["scopes"] = toAnySlice(scopes)
	}

	k := oidcClientKind()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "OIDCClient",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}

	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	// PlanCreate's own executor is discarded: create's executor needs the
	// wait/secret-read/discovery stages PlanCreate's generic shape does not
	// express. The Effect it builds (server-side dry-run preview) is the
	// part this command reuses.
	eff, _, err := engine.PlanCreate(ctx, client, k.GVR, "", obj, engine.LocalCluster, k.Name, "OIDCClient", k.Sensitive)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	exec := func(ctx context.Context) (runtime.Result, error) {
		return c.execute(ctx, client, k, obj, timeout)
	}
	return eff, exec, nil
}

// execute runs strictly in this order: mutation -> status wait ->
// backing-Secret GET (decode) -> discovery GET/degrade -> buffered
// render. This closure never writes stdout - "shown exactly once"
// describes finishResult's buffered render, not an intermediate step here.
func (c *createCmd) execute(ctx context.Context, client engine.ClusterClient, k kinds.Kind, obj *unstructured.Unstructured, timeout time.Duration) (runtime.Result, error) {
	created, err := client.Create(ctx, k.GVR, "", obj, false)
	if err != nil {
		return runtime.Result{}, err
	}
	name := created.GetName()
	recovery := recoveryHint(name)

	var clientID string
	check := func(_ context.Context, o *unstructured.Unstructured) waitOutcome {
		id, _, _ := unstructured.NestedString(o.Object, "status", "clientID")
		secrets, _, _ := unstructured.NestedMap(o.Object, "status", "clientSecrets")
		switch {
		case id == "" && len(secrets) == 0:
			return waitOutcome{} // not yet provisioned
		case id == "":
			return waitOutcome{terminal: impossibleCreateShape(name, secrets)}
		case len(secrets) == 1:
			clientID = id
			for key := range secrets {
				return waitOutcome{done: true, value: key}
			}
		}
		if len(secrets) > 1 {
			return waitOutcome{terminal: impossibleCreateShape(name, secrets)}
		}
		return waitOutcome{} // clientID set, controller has not generated the initial secret yet
	}
	secretName, err := waitForOIDCClient(ctx, client, k.GVR, name, timeout, check,
		func(last *unstructured.Unstructured) *clierr.Error { return createTimeoutError(name) })
	if err != nil {
		return runtime.Result{}, err
	}

	plaintext, err := readClientSecret(ctx, c.clients, clientID, secretName)
	if err != nil {
		return runtime.Result{}, committedPartialSuccess(err, "OIDCClient", name, secretName, recovery)
	}

	base, baseErr := c.serverBase.ServerBaseURL()
	var endpoints *oidcEndpoints
	var issuer string
	var warnings []string
	if baseErr != nil {
		return runtime.Result{}, committedPartialSuccess(baseErr, "OIDCClient", name, secretName, recovery)
	}
	endpoints, issuer, warnings = c.discover(ctx, base)

	data := map[string]any{
		"name":         name,
		"clientID":     clientID,
		"clientSecret": plaintext,
		"secretName":   secretName,
		"issuer":       issuer,
	}
	if endpoints != nil {
		data["endpoints"] = endpoints
	}

	secretLine, shownOnce := runtime.OnceOnlyLine("client secret", plaintext, "client secret")
	res := runtime.MutationText("oidcclient", name, "created",
		"client id: "+clientID,
		secretLine,
		"secret name: "+secretName,
		"issuer: "+issuer,
	)
	res.Kind = "OIDCClientCreated"
	res.Data = data
	res.OnceOnlyNotice = shownOnce
	res.Warnings = warnings
	res.CommittedContext = &runtime.CommittedContext{ResourceKind: "OIDCClient", ResourceName: name, SecretName: secretName, Recovery: recovery}
	return res, nil
}

// discover reads the live discovery document and returns its projected
// endpoints and issuer, or degrades: a static issuer derived from the SAME
// resolved base and no endpoints, with a warning naming what failed.
// Discovery failure (a blip, a proxy, the server's 500 when zero clients
// exist racing a concurrent delete) never fails a committed create - a
// failure here would read as retryable, and a retry creates a second client.
func (c *createCmd) discover(ctx context.Context, base string) (*oidcEndpoints, string, []string) {
	fallbackIssuer := base + "/oidc"
	degrade := func(reason string) (*oidcEndpoints, string, []string) {
		return nil, fallbackIssuer, []string{
			fmt.Sprintf("could not confirm the discovery document (%s); issuer shown is derived from the server, not read live", reason),
		}
	}
	root, err := c.roots.Root()
	if err != nil {
		return degrade(err.Error())
	}
	body, err := root.GetJSON(ctx, "/oidc/.well-known/openid-configuration")
	if err != nil {
		return degrade(err.Error())
	}
	endpoints, issuer, err := validateDiscovery(body, base)
	if err != nil {
		return degrade(err.Error())
	}
	return endpoints, issuer, nil
}

// oidcEndpoints is the fixed, flat projection of the discovery document -
// never the raw document - userinfo is omitted when the server omits it.
type oidcEndpoints struct {
	Authorization string `json:"authorization"`
	Token         string `json:"token"`
	JWKS          string `json:"jwks"`
	Userinfo      string `json:"userinfo,omitempty"`
}

// discoveryDocument is the subset of the well-known document this command
// reads; unknown fields are ignored.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// validateDiscovery decodes and validates body: issuer must be present and
// equal effectiveBase+"/oidc", and the authorization/token/jwks endpoints
// the advertised authorization-code/S256 flow needs must be present.
// Malformed JSON, a missing required endpoint, or an issuer mismatch are
// all reported as validation errors - the caller degrades on any of them,
// never distinguishing "malformed" from "wrong" for the purpose of the
// create Result.
func validateDiscovery(body []byte, effectiveBase string) (*oidcEndpoints, string, error) {
	var doc discoveryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", fmt.Errorf("malformed discovery document: %w", err)
	}
	want := effectiveBase + "/oidc"
	if doc.Issuer != want {
		return nil, "", fmt.Errorf("issuer %q does not match the expected %q", doc.Issuer, want)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, "", fmt.Errorf("discovery document is missing a required endpoint")
	}
	return &oidcEndpoints{
		Authorization: doc.AuthorizationEndpoint,
		Token:         doc.TokenEndpoint,
		JWKS:          doc.JWKSURI,
		Userinfo:      doc.UserinfoEndpoint,
	}, doc.Issuer, nil
}

// impossibleCreateShape builds create's terminal, non-retryable error for
// an observed shape the controller alone could never produce: a secret key
// present with no clientID, or more than one secret on the FIRST
// observation - a concurrent add after this create's own mutation. A
// completed controller plus a concurrent actor must never be misdiagnosed
// as a dead/disabled provider by waiting to timeout.
func impossibleCreateShape(name string, secrets map[string]any) *clierr.Error {
	keys := slices.Sorted(maps.Keys(secrets))
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("oidc-client %s: observed an impossible secret shape (keys: %v) - a concurrent actor changed this client while create was waiting", name, keys)).
		WithDetails(clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "OIDCClient", ResourceName: name,
			Recovery: recoveryHint(name),
		})
}

// createTimeoutError builds create's timeout error: NON-RETRYABLE (a
// clierr.Error defaults Retryable to false) - retrying a create whose
// mutation already committed would create a second client. Names
// oidc-provider as the likely cause (with the flag off, the server accepts
// the create but status never populates) and points at the object for
// re-checking; no automatic cleanup.
func createTimeoutError(name string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("timed out waiting for oidc-client %s to be provisioned; the oidc-provider feature may be disabled", name)).
		WithDetails(clierr.CommittedContextDetails{
			Committed: true, ResourceKind: "OIDCClient", ResourceName: name,
			Recovery: "check the oidc-provider feature, then " + recoveryHint(name),
		})
}

// committedPartialSuccess wraps a post-commit delivery failure (a read
// error, a missing/malformed secret) with clierr.CommittedContextDetails,
// preserving the original error's Code/Hint/Retryable when it is already a
// *clierr.Error (e.g. RBAC/NOT_FOUND from the backing-Secret read) rather
// than flattening it to CodeGeneric - a caller inspecting Retryable to
// decide whether to retry must see the classification the underlying read
// actually carried, not always false.
func committedPartialSuccess(err error, kind, name, secretName, recovery string) *clierr.Error {
	return engine.CommittedPartialSuccess(err, clierr.CommittedContextDetails{
		Committed: true, ResourceKind: kind, ResourceName: name, SecretName: secretName, Recovery: recovery,
	})
}
