package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// normanTokensPath is Rancher's v3 token collection at the server root, the
// create path a Rancher without ext.cattle.io still serves.
const normanTokensPath = "/v3/tokens"

// errClusterTokenUnscoped is a create whose response does not name the
// requested cluster: empty, missing, or another cluster. The result keeps
// the created token's ref so the caller revokes it; its bearer is never
// returned.
var errClusterTokenUnscoped = errors.New("the created token is not scoped to the requested cluster")

// errServerTooOldForReuse is a Rancher that serves neither the ext token
// store nor the v3 token create.
var errServerTooOldForReuse = errors.New("this Rancher serves neither ext.cattle.io nor a v3 token create; too old for token reuse")

// createV3ClusterToken creates a cluster-scoped v3 token through Rancher's
// Norman route with bearer as the credential. The response must carry a
// non-empty token and echo clusterId; an empty expiresAt is filled in from
// its createdTS and ttl; the status is classified as every
// create is (a deterministic 4xx proves absence, 5xx and transport failures
// may follow a committed create).
func createV3ClusterToken(ctx context.Context, client *http.Client, base, bearer, clusterID, description string) (CreateResult, error) {
	body := map[string]any{"type": "token", "description": description, "clusterId": clusterID}
	verb := "creating cluster token"
	if clusterID == "" {
		verb = "creating token"
	}
	r, resp, err := postTokenCreate(ctx, client, http.NewRequestWithContext, base+normanTokensPath, bearer, body, TokenKindV3, description, verb)
	if err != nil {
		return r, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return r, errServerTooOldForReuse
	}
	if r.ExpiresAt == "" {
		// Rancher answers a Norman create with an empty expiresAt and sets it
		// after answering: the creation time plus the ttl.
		r.ExpiresAt = computedExpiry(r.CreatedTS, r.TTL)
	}
	return validateScope(r, clusterID)
}

// validateScope is the security check the plugin lane needs and the
// routine per-cluster create never had: the response must name the
// requested cluster. An identified token that does not is a failure that
// keeps its ref (the caller revokes it) and loses its bearer; an
// unidentified 2xx fails with no ref.
func validateScope(r CreateResult, clusterID string) (CreateResult, error) {
	switch {
	case r.Outcome == CreateUnidentified:
		return r, clierr.New(clierr.CodeGeneric, "token create response identified no token")
	case r.Outcome != CreateKnown || r.Bearer == "":
		return r, clierr.New(clierr.CodeGeneric, "token create response contained no bearer token")
	case r.ClusterName != clusterID:
		r.Bearer = ""
		return r, fmt.Errorf("%w: requested %q, response names %q", errClusterTokenUnscoped, clusterID, r.ClusterName)
	}
	return r, nil
}

// createClusterToken is the plugin-created login's cluster-token chain: the
// ext create with spec.clusterName first, the v3 create on a Rancher
// without ext.cattle.io, and the scope check on both. Any failure fails
// closed: the unscoped login credential is never returned in its place.
func createClusterToken(ctx context.Context, client *http.Client, base, bearer, clusterID, description string) (CreateResult, error) {
	r, err := CreateDerivedToken(ctx, client, base, bearer, clusterID, 0, description)
	switch {
	case err == nil:
		return validateScope(r, clusterID)
	case errors.Is(err, ErrExtGroupAbsent):
		return createV3ClusterToken(ctx, client, base, bearer, clusterID, description)
	default:
		return r, err
	}
}
