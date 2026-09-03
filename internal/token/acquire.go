package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
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
	DerivedMintOutcome   CreateOutcome
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
	if st.sOwned && st.session.Create.Ref != nil && st.session.Create.Bearer != "" {
		ctx, cancel := cleanupContext(st.ctx)
		st.sClean = revokeToken(ctx, st.client, st.base, *st.session.Create.Ref, st.session.Create.Bearer)
		cancel()
		st.sOwned = false
	}
	marker := st.marker
	if !st.session.Create.Marked && st.derived.Outcome == CreateNotAttempted {
		// No marked create was attempted: a SAML S with no mint. The marker
		// identifies nothing on the server.
		marker = ""
	}
	return &AcquireError{
		Err: primary, Marker: marker,
		SessionTokenOutcome: st.session.Create.Outcome, SessionTokenKind: st.session.Create.Kind, SessionTokenRef: st.session.Create.Ref,
		SessionTokenUnmarked: !st.session.Create.Marked, SessionTokenSearch: st.session.Handoff,
		SessionTokenCleanup: st.sClean, SessionCreatedAt: st.session.Create.CreatedAt,
		DerivedMintOutcome: st.derived.Outcome, DerivedTokenRef: st.derived.Ref,
		DerivedTokenCleanup: st.dClean, DerivedCreatedAt: st.derived.CreatedAt,
	}
}

// AcquireLoginCredential runs the identity-provider dance and returns the
// credential auth login stores. Ownership begins at the first create: once
// the transient session token S exists, this function revokes it on every
// path that does not hand it outward.
func AcquireLoginCredential(ctx context.Context, client *http.Client, req LoginRequest, ttlMs int64, p runtime.Prompter) (LoginCredential, error) {
	marker, err := NewOperationMarker()
	if err != nil {
		return LoginCredential{}, err
	}
	host, herr := os.Hostname()
	if herr != nil {
		host = "unknown"
	}
	responseType := req.ResponseType
	if responseType == "" {
		responseType = "json"
	}
	input := &loginInput{server: req.Server, clusterID: req.ClusterID, authProvider: req.AuthProvider, authFlow: req.AuthFlow,
		idpClient: req.IdPClient, responseType: responseType, description: TokenDescription(host, marker)}
	st := &acquireState{marker: marker, client: client, base: kube.BaseURL(req.Server), ctx: ctx,
		derived: CreateResult{Outcome: CreateNotAttempted}, dClean: RevokeOutcome{Result: ResultNotAttempted}}

	st.session, err = loginAndGenerateCred(ctx, client, input, p)
	st.sClean = RevokeOutcome{Ref: st.session.Create.Ref, Result: ResultNotAttempted}
	if err != nil {
		switch st.session.Create.Outcome {
		case CreateNotAttempted, CreateNotCreated:
			return LoginCredential{}, err
		}
		// unknown, unidentified, or known without a usable bearer
		return LoginCredential{}, st.finishAcquireError(err)
	}
	s := st.session.Create
	if s.Bearer == "" || s.Ref == nil {
		return LoginCredential{}, st.finishAcquireError(clierr.New(clierr.CodeAuth, "authentication returned no usable token; check the auth provider and credentials"))
	}
	st.sOwned = true
	defer func() {
		// Panic safety net only: ordinary returns have already transferred
		// or revoked S through the explicit paths above and below.
		if st.sOwned {
			ctx, cancel := cleanupContext(ctx)
			_ = revokeToken(ctx, client, st.base, *s.Ref, s.Bearer)
			cancel()
		}
	}()

	dr, err := MintDerivedToken(ctx, client, st.base, s.Bearer, "", ttlMs, input.description)
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
				Ref: s.Ref, SessionTokenUnmarked: !s.Marked, SessionTokenSearch: st.session.Handoff,
				SessionCreatedAt: s.CreatedAt,
				parent:           ctx, client: client, base: st.base}, nil
		}
		if dr.Outcome == CreateKnown && dr.Ref != nil {
			// D exists but its bearer failed validation: delete it by name
			// with S as the bearer, before S itself is revoked.
			c, cancel := cleanupContext(ctx)
			st.dClean = revokeToken(c, client, st.base, *dr.Ref, s.Bearer)
			cancel()
		}
		return LoginCredential{}, st.finishAcquireError(err)
	}
	st.sOwned = false
	c, cancel := cleanupContext(ctx)
	sClean := revokeToken(c, client, st.base, *s.Ref, dr.Bearer)
	cancel()
	return LoginCredential{Token: dr.Bearer, ExpiresAt: dr.ExpiresAt, Provenance: ProvenanceDerived, Marker: marker,
		Ref: dr.Ref, SessionTokenCleanup: sClean, SessionTokenUnmarked: !s.Marked, SessionTokenSearch: st.session.Handoff,
		SessionCreatedAt: s.CreatedAt, DerivedCreatedAt: dr.CreatedAt,
		parent: ctx, client: client, base: st.base}, nil
}

// legacyFallbackNotice explains why the session token itself is being
// stored: this Rancher does not serve the ext token store the derived
// token would come from.
func legacyFallbackNotice(expiresAt string) string {
	note := fmt.Sprintf("this Rancher does not serve ext.cattle.io (requires Rancher >= %s, the supported minimum); storing the session token directly", config.MinRancherVersion)
	if expiresAt != "" {
		note += " (expires " + expiresAt + ")"
	}
	return note
}
