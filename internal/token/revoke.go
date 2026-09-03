package token

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
)

// V3TokensPath is the management.cattle.io/v3 Token path through the
// Rancher proxy on the local cluster; the group is an aggregated API, never
// served at the server root.
const V3TokensPath = "/k8s/clusters/local/apis/management.cattle.io/v3/tokens"

// cleanupTimeout bounds every revoke so cleanup can never hang a login.
const cleanupTimeout = 10 * time.Second

// cleanupContext derives the context every revoke runs on: detached from
// the operation's cancellation, so the cancellation that triggers cleanup
// cannot kill the revoke request, and bounded.
func cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), cleanupTimeout)
}

// revokeToken deletes one token by kind and name with bearer as the
// Authorization bearer. 404 is alreadyGone: the goal state holds.
func revokeToken(ctx context.Context, client *http.Client, base string, ref TokenRef, bearer string) RevokeOutcome {
	out := RevokeOutcome{Ref: &ref}
	path := base + V3TokensPath + "/" + ref.Name
	if ref.Kind == TokenKindExt {
		path = base + ExtTokensPath + "/" + ref.Name
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, path, nil)
	if err != nil {
		out.Result, out.Error = ResultFailed, err.Error()
		return out
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, _, err := httpx.Do(client, req)
	if err != nil {
		out.Result, out.Error = ResultFailed, err.Error()
		return out
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		out.Result = ResultRevoked
	case http.StatusNotFound:
		out.Result = ResultAlreadyGone
	default:
		out.Result, out.Error = ResultFailed, fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return out
}

// LoginCredential is a token ready to be stored, with everything the
// operation learned while creating it and the cleanup capability that
// ownership implies.
type LoginCredential struct {
	Token      string
	ExpiresAt  string
	Provenance Provenance
	// Marker is the operation marker embedded in every token this
	// operation created; empty for a userSupplied credential.
	Marker string
	// Ref identifies the transferred token for revocation; nil when the
	// token is user-supplied or unidentified.
	Ref *TokenRef
	// SessionTokenCleanup is S's fate on the derived path (revoked,
	// alreadyGone, or failed); zero-valued for legacySession (S is the
	// credential) and userSupplied.
	SessionTokenCleanup RevokeOutcome
	// SessionTokenUnmarked is true when S carries no marker (the SAML
	// authtokens flow cannot set a description).
	SessionTokenUnmarked bool
	// SessionTokenSearch carries the SAML handoff facts when S is unmarked.
	SessionTokenSearch *SAMLHandoff
	// SessionCreatedAt and DerivedCreatedAt are the client timestamps taken
	// before each create request, kept so a failure after acquisition can
	// still report when each token was created.
	SessionCreatedAt string
	DerivedCreatedAt string

	parent context.Context
	client *http.Client
	base   string
}

// UserSupplied wraps a pasted or piped token: no marker, no revoke.
func UserSupplied(token string) LoginCredential {
	return LoginCredential{Token: token, Provenance: ProvenanceUserSupplied}
}

// Revoke best-effort deletes the transferred token on the server using the
// token itself as the bearer. It derives its own detached, bounded context
// so a caller cannot hand it the cancelled operation context. userSupplied
// → notApplicable; no Ref → notAttempted.
func (c LoginCredential) Revoke() RevokeOutcome {
	if c.Provenance == ProvenanceUserSupplied {
		return RevokeOutcome{Result: ResultNotApplicable}
	}
	if c.Ref == nil {
		return RevokeOutcome{Result: ResultNotAttempted}
	}
	parent := c.parent
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := cleanupContext(parent)
	defer cancel()
	return revokeToken(ctx, c.client, c.base, *c.Ref, c.Token)
}

// SAMLHandoff is recorded immediately before the SAML browser URL is
// shown. PollEndedAt is when the CLI stopped waiting and bounds nothing:
// the browser sign-in can still complete afterwards. RequestExpiresAt
// is set only when the server declares an expiry for the deposited auth
// token and is the only value that bounds creation from above.
type SAMLHandoff struct {
	Provider         string
	HandoffAt        string
	PollEndedAt      string
	RequestID        string
	RequestExpiresAt string
}
