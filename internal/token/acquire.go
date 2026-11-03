package token

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// SessionAcquireResult is what loginAndGenerateCred returns for every
// provider: the synchronous create result, plus the SAML handoff when a
// browser handoff was published. Authoritative when error != nil.
type SessionAcquireResult struct {
	Create  CreateResult
	Handoff *SAMLHandoff
}

// AcquireError is the failure shape of AcquireLoginCredential once the
// first create was attempted: the primary error plus everything cleanup
// learned. It unwraps to the primary.
type AcquireError struct {
	Err                 error
	Marker              string
	SessionTokenOutcome CreateOutcome
	// SessionTokenKind is S's kind when known without a name ("" when the
	// login response was lost or unrecognizable: S may be of either kind).
	SessionTokenKind     TokenKind
	SessionTokenRef      *TokenRef
	SessionTokenUnmarked bool
	SessionTokenSearch   *SAMLHandoff
	SessionTokenCleanup  RevokeOutcome
	SessionCreatedAt     string
	SessionExpiresAt     string
	// SessionUserID is the user the session token belongs to, "" when the
	// login response named none.
	SessionUserID        string
	DerivedCreateOutcome CreateOutcome
	DerivedTokenRef      *TokenRef
	DerivedTokenCleanup  RevokeOutcome
	DerivedCreatedAt     string
}

func (e *AcquireError) Error() string { return e.Err.Error() }
func (e *AcquireError) Unwrap() error { return e.Err }

// WithPrimary returns a copy whose primary error is replaced and whose
// cleanup evidence is untouched. Callers that reclassify the primary use
// this and never return the classified error alone.
func (e *AcquireError) WithPrimary(err error) *AcquireError {
	c := *e
	c.Err = err
	return &c
}

// acquireState is the evidence gathered while acquiring, built into an
// AcquireError only after every cleanup that must be reported has run.
type acquireState struct {
	marker  string
	session SessionAcquireResult
	sOwned  bool
	sClean  RevokeOutcome
	derived CreateResult
	dClean  RevokeOutcome
	client  *http.Client
	base    string
	ctx     context.Context
}

// finishAcquireError revokes S while ownership remains, records the
// outcome, and only then builds the error: a deferred function could not
// change a return value already evaluated. sClean and dClean are already
// notAttempted with the token's ref when no DELETE ran.
func (st *acquireState) finishAcquireError(primary error) *AcquireError {
	if st.sOwned {
		ctx, cancel := cleanupContext(st.ctx)
		st.sClean = revokeToken(ctx, st.client, st.base, *st.session.Create.Ref, st.session.Create.Bearer)
		cancel()
		st.sOwned = false
	}
	marker := st.marker
	if !st.session.Create.Marked && st.derived.Outcome == CreateNotAttempted {
		// No marked create was attempted: a SAML S with no derived token create. The marker
		// identifies nothing on the server.
		marker = ""
	}
	return &AcquireError{
		Err: primary, Marker: marker,
		SessionTokenOutcome: st.session.Create.Outcome, SessionTokenKind: st.session.Create.Kind, SessionTokenRef: st.session.Create.Ref,
		SessionTokenUnmarked: !st.session.Create.Marked, SessionTokenSearch: st.session.Handoff,
		SessionTokenCleanup: st.sClean, SessionCreatedAt: st.session.Create.CreatedAt, SessionExpiresAt: st.session.Create.ExpiresAt,
		SessionUserID:        st.session.Create.UserID,
		DerivedCreateOutcome: st.derived.Outcome, DerivedTokenRef: st.derived.Ref,
		DerivedTokenCleanup: st.dClean, DerivedCreatedAt: st.derived.CreatedAt,
	}
}

// acquireLogin is the login prefix both sign-ins share: it sets the
// operation marker, the json response type (unless the caller named one) and
// the description on a prepared input, runs the login, and decides who owns
// the session token S it created. S is owned once it has a name and a usable
// bearer, whatever status or error came with them; an owned S is revoked by
// finishAcquireError on every failure. The returned error is the login's own
// when it left nothing the CLI can find (nothing was created, or the request
// never went out), and an *AcquireError carrying S's fate otherwise. The
// state is returned with a nil error only while S is still owned.
func acquireLogin(ctx context.Context, client *http.Client, input *loginInput, p runtime.Prompter) (*acquireState, error) {
	marker, err := NewOperationMarker()
	if err != nil {
		return nil, err
	}
	if input.responseType == "" {
		input.responseType = "json"
	}
	input.description = TokenDescription(hostname(), marker)
	st := &acquireState{marker: marker, client: client, base: kube.BaseURL(input.server), ctx: ctx,
		derived: CreateResult{Outcome: CreateNotAttempted}, dClean: RevokeOutcome{Result: ResultNotAttempted}}

	st.session, err = loginAndGenerateCred(ctx, client, input, p)
	st.sClean = RevokeOutcome{Ref: st.session.Create.Ref, Result: ResultNotAttempted}
	// Ownership begins the moment a session token the CLI can address
	// exists: a name and a usable bearer, whatever status or error came
	// with them. Every failure from here on revokes it.
	st.sOwned = st.session.Create.Ref != nil && st.session.Create.Bearer != ""
	if err != nil {
		switch st.session.Create.Outcome {
		case CreateNotAttempted, CreateNotCreated:
			return st, err
		}
		// unknown, unidentified, known without a usable bearer, or known
		// with one that arrived alongside an error (a status the CLI
		// refuses): the last is owned and is revoked here.
		return st, st.finishAcquireError(err)
	}
	if !st.sOwned {
		return st, st.finishAcquireError(clierr.New(clierr.CodeAuth, "authentication returned no usable token; check the auth config and credentials"))
	}
	return st, nil
}

// AcquireLoginCredential runs the identity-provider dance and returns the
// credential auth login stores. Ownership begins at the first create: once
// the transient session token S exists, this function revokes it on every
// path that does not hand it outward.
func AcquireLoginCredential(ctx context.Context, client *http.Client, req LoginRequest, ttlMs int64, p runtime.Prompter) (LoginCredential, error) {
	if req.CAFlag == "" {
		return LoginCredential{}, callerCAFlagMissing("LoginRequest.CAFlag")
	}
	input := &loginInput{server: req.Server, authProvider: req.AuthProvider, authFlow: req.AuthFlow, exactAuthConfig: req.ExactAuthConfig,
		caFlag: req.CAFlag, idpClient: req.IdPClient, responseType: req.ResponseType, beforeSignIn: req.BeforeSignIn}
	st, err := acquireLogin(ctx, client, input, p)
	if err != nil {
		return LoginCredential{}, err
	}
	marker := st.marker
	s := st.session.Create
	defer func() {
		// Panic safety net only: ordinary returns have already transferred
		// or revoked S through the explicit paths above and below.
		if st.sOwned {
			ctx, cancel := cleanupContext(ctx)
			_ = revokeToken(ctx, client, st.base, *s.Ref, s.Bearer)
			cancel()
		}
	}()

	dr, err := CreateDerivedToken(ctx, client, st.base, s.Bearer, "", ttlMs, input.description)
	st.derived = dr
	// D's cleanup is an explicit outcome from here on: notAttempted until a
	// DELETE runs, so no report ever carries an empty result for a token
	// whose create was attempted.
	st.dClean = RevokeOutcome{Ref: dr.Ref, Result: ResultNotAttempted}
	if err != nil {
		if errors.Is(err, ErrExtGroupAbsent) {
			st.sOwned = false
			p.Notice(legacyFallbackNotice(s.ExpiresAt))
			return LoginCredential{Token: s.Bearer, ExpiresAt: s.ExpiresAt, Provenance: ProvenanceLegacySession, Marker: marker,
				UserID: s.UserID,
				Ref:    s.Ref, SessionTokenUnmarked: !s.Marked, SessionTokenSearch: st.session.Handoff,
				SessionCreatedAt: s.CreatedAt,
				parent:           ctx, client: client, base: st.base}, nil
		}
		// One bounded context covers both cleanups, so a slow server costs
		// at most one cleanup timeout before the failure is reported. D
		// goes first, by name with S as the bearer, while S is still valid;
		// then S itself. The two are sequential by necessity, and a v3 S
		// takes up to two requests (logout, then the DELETE), so the bound
		// covers up to three requests. The shared bound is a deliberate
		// trade: a D delete that consumes most of it leaves S little time
		// and S may go unrevoked, which the failure reports; two full
		// bounds would double the worst-case wait of an interactive login
		// against a server that is already failing.
		c, cancel := cleanupContext(ctx)
		if dr.Outcome == CreateKnown && dr.Ref != nil {
			st.dClean = revokeToken(c, client, st.base, *dr.Ref, s.Bearer)
		}
		st.sClean = revokeToken(c, client, st.base, *s.Ref, s.Bearer)
		st.sOwned = false
		cancel()
		return LoginCredential{}, st.finishAcquireError(err)
	}
	st.sOwned = false
	// A v3 S is deleted with its own bearer, which logout requires and which
	// is still valid here; an ext S is deleted with the derived token's.
	sBearer := dr.Bearer
	if s.Ref.Kind == TokenKindV3 {
		sBearer = s.Bearer
	}
	c, cancel := cleanupContext(ctx)
	sClean := revokeToken(c, client, st.base, *s.Ref, sBearer)
	cancel()
	return LoginCredential{Token: dr.Bearer, ExpiresAt: dr.ExpiresAt, Provenance: ProvenanceDerived, Marker: marker,
		UserID: dr.UserID, AuthConfig: dr.AuthConfig,
		Ref: dr.Ref, SessionTokenCleanup: sClean, SessionTokenUnmarked: !s.Marked, SessionTokenSearch: st.session.Handoff,
		SessionCreatedAt: s.CreatedAt, SessionExpiresAt: s.ExpiresAt, DerivedCreatedAt: dr.CreatedAt,
		parent: ctx, client: client, base: st.base}, nil
}

// clusterSignIn is what the sign-in for a cluster token leaves: the cluster
// token, the login token's fate after its delete, and the operation marker.
type clusterSignIn struct {
	Cluster CreateResult
	Login   *clierr.TokenFate
	Marker  string
}

// signInForClusterToken signs in and creates the cluster token the sign-in
// returns. The login token (a session token, or a kubeconfig token for SAML)
// is the bearer of the cluster lookup and of the ext create, so Rancher binds
// the cluster token to the login token's user; the login token is then
// deleted with its own bearer. The cluster token stays valid after that.
//
// Every failure after the login token exists deletes what the CLI can
// address: the cluster token when its name is known (with the login token's
// bearer, while it is valid), then the login token, both on one cleanup bound
// detached from the caller's cancellation. A failure with no addressable
// login token returns the login's own error; every other failure is a
// LoginFailureDetails error carrying the fate of both tokens. A failed delete
// of the login token alone is not a failure: its fate rides the result.
func signInForClusterToken(ctx context.Context, client *http.Client, input *loginInput, clusterID string, p runtime.Prompter) (clusterSignIn, error) {
	st, err := acquireLogin(ctx, client, input, p)
	if err != nil {
		if ae, ok := errors.AsType[*AcquireError](err); ok {
			return clusterSignIn{}, failSignInAcquire(ae)
		}
		return clusterSignIn{}, err
	}
	s := st.session.Create
	defer func() {
		// Panic safety net only: ordinary returns have already revoked the login token.
		if st.sOwned {
			c, cancel := cleanupContext(ctx)
			_ = revokeToken(c, client, st.base, *s.Ref, s.Bearer)
			cancel()
		}
	}()
	loginFate := func(out RevokeOutcome) *clierr.TokenFate {
		return FateOf(s.Outcome, s.Kind, s.Ref, s.Marked, s.CreatedAt, s.ExpiresAt, out, st.session.Handoff)
	}
	// fail deletes the cluster token when its name is known, then the login
	// token, on one bound, and reports both.
	fail := func(refusal signInRefusal, stage clierr.LoginStage, ct CreateResult) error {
		c, cancel := cleanupContext(ctx)
		defer cancel()
		clusterOut := RevokeOutcome{Ref: ct.Ref, Result: ResultNotAttempted}
		if ct.Outcome == CreateKnown && ct.Ref != nil {
			clusterOut = revokeToken(c, client, st.base, *ct.Ref, s.Bearer)
		}
		loginOut := revokeToken(c, client, st.base, *s.Ref, s.Bearer)
		st.sOwned = false
		marker := st.marker
		if !s.Marked && ct.Outcome == CreateNotAttempted {
			marker = ""
		}
		return failSignIn(refusal.err, stage, marker, loginFate(loginOut), FateOf(ct.Outcome, ct.Kind, ct.Ref, ct.Marked, ct.CreatedAt, ct.ExpiresAt, clusterOut, nil), ct.UserID, refusal.recovery(cmp.Or(ct.UserID, s.UserID)), clusterID)
	}

	resolved, warning, err := resolveClusterIDForCreate(ctx, client, st.base, s.Bearer, clusterID)
	if err != nil {
		return clusterSignIn{}, fail(lookupRefusal(err), clierr.StageClusterToken, CreateResult{Outcome: CreateNotAttempted})
	}
	if warning != "" {
		p.Notice(warning)
	}
	ct, err := createClusterToken(ctx, client, st.base, s.Bearer, resolved, input.description)
	if err != nil {
		return clusterSignIn{}, fail(createRefusal(err, resolved, ct.Kind == TokenKindExt), clierr.StageClusterToken, ct)
	}

	// The server's facts requests go out with the login token while it is
	// still valid, never with the cluster token handed to kubectl.
	httpx.SendSignInRequest(ctx, st.base, provider.InstallIDFetch(client, st.base, s.Bearer))

	c, cancel := cleanupContext(ctx)
	loginOut := revokeToken(c, client, st.base, *s.Ref, s.Bearer)
	cancel()
	st.sOwned = false
	return clusterSignIn{Cluster: ct, Login: loginFate(loginOut), Marker: st.marker}, nil
}

// legacyFallbackNotice explains why the session token itself is being
// stored: this Rancher does not serve the ext token store the derived
// token would come from.
func legacyFallbackNotice(expiresAt string) string {
	note := fmt.Sprintf("this Rancher does not serve ext.cattle.io (requires Rancher >= %s, the supported minimum); storing the session token directly", config.MinRancherVersion)
	if expiresAt != "" {
		note += " (expires " + output.Printable(expiresAt) + ")"
	}
	return note
}
