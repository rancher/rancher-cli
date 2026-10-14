package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
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

// LogoutPath is Rancher's logout endpoint: POST deletes the token the
// request's bearer names, never any other.
const LogoutPath = "/v1/logout"

// revokeToken deletes one token by kind and name with bearer as the
// Authorization bearer. 404 is alreadyGone: the goal state holds.
//
// A v3 token whose own bearer this is goes through the logout endpoint
// first. Logout deletes the bearer's token and nothing else, so the local
// check must pass before any request: sending it with another token's
// bearer would delete that token instead. Any logout answer other than
// 200 or 204 (the cluster-scoped token is refused there, a provider set to
// forced single logout fails it) is followed by the proxy DELETE with the
// same ref and bearer, whose answer decides the outcome.
func revokeToken(ctx context.Context, client *http.Client, base string, ref TokenRef, bearer string) RevokeOutcome {
	out := RevokeOutcome{Ref: &ref}
	logout := ""
	if deletesWithOwnBearer(ref, bearer) {
		status, err := postLogout(ctx, client, base, bearer)
		out.LogoutStatus = status
		if status == http.StatusOK || status == http.StatusNoContent {
			out.Status, out.Result = status, ResultRevoked
			return out
		}
		logout = strconv.Itoa(status)
		if err != nil && status == 0 {
			logout = revokeErrorText(err)
		}
	}
	fail := func(answer string) RevokeOutcome {
		out.Result, out.Error = ResultFailed, answer
		if logout != "" {
			out.Error = "logout " + logout + "; delete " + answer
		}
		return out
	}
	path := base + V3TokensPath + "/" + ref.Name
	if ref.Kind == TokenKindExt {
		path = base + ExtTokensPath + "/" + ref.Name
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, _, err := httpx.Do(client, req)
	if err != nil {
		return fail(revokeErrorText(err))
	}
	out.Status = resp.StatusCode
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		out.Result = ResultRevoked
	case http.StatusNotFound:
		out.Result = ResultAlreadyGone
	default:
		return fail(fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	return out
}

// revokeErrorText is the text a failed revoke request records, in the
// outcome's error and in the stamp a retired token's entry keeps. A
// certificate failure reaches here as the classified error inside the
// request's url.Error; the text is rebuilt as that url.Error around the
// transport error, which is what an unclassified client produces, so the
// recorded text does not depend on which client sent the request.
func revokeErrorText(err error) string {
	ce := tlstrust.TypedError(err)
	ue, ok := errors.AsType[*url.Error](err)
	if ce == nil || !ok || ce.Unwrap() == nil {
		return err.Error()
	}
	return (&url.Error{Op: ue.Op, URL: ue.URL, Err: ce.Unwrap()}).Error()
}

// deletesWithOwnBearer reports whether bearer is the v3 token ref's own
// bearer, the only case the logout endpoint may be used for.
func deletesWithOwnBearer(ref TokenRef, bearer string) bool {
	return ref.Kind == TokenKindV3 && v3NameRE.MatchString(ref.Name) && usableBearer(TokenKindV3, ref.Name, bearer)
}

// postLogout sends one logout request with bearer, no query string and no
// body: a query asking for logout of every session would end the account's
// sessions at the identity provider. It returns the status the server
// answered, 0 when none arrived. A response whose body cannot be read still
// carries its status.
func postLogout(ctx context.Context, client *http.Client, base, bearer string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+LogoutPath, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, _, err := httpx.Do(client, req)
	if resp != nil {
		return resp.StatusCode, err
	}
	return 0, err
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
	// UserID is the token's owner as the create response reported it; empty
	// when no response carried one (a SAML legacy credential, or a 2.14+
	// login body).
	UserID string
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
	// SessionExpiresAt is when S expires, as the create response reported
	// it; empty when it reported none.
	SessionExpiresAt string

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

// RevokeCreated deletes a token this credential created, with the
// credential as bearer: the cluster token the plugin login proved itself
// with, or a superseded token of the same owner. Like Revoke it detaches
// from the caller's cancellation and runs under the cleanup bound. A
// caller deadline that has NOT yet passed shortens the bound (the
// retirement pass hands in what remains of its budget); a deadline that
// has already passed is the very cancellation that triggered cleanup and
// must not cancel the cleanup too, so it is ignored and the full cleanup
// bound applies. The retirement pass checks its deadline against the
// clock before each call; a deadline that passes between that check and
// this read costs one cleanup bound, a window of microseconds.
func (c LoginCredential) RevokeCreated(ctx context.Context, ref TokenRef) RevokeOutcome {
	if c.Provenance == ProvenanceUserSupplied {
		return RevokeOutcome{Ref: &ref, Result: ResultNotApplicable}
	}
	bound := cleanupTimeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 && remaining < bound {
			bound = remaining
		}
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
	defer cancel()
	return revokeToken(rctx, c.client, c.base, ref, c.Token)
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
