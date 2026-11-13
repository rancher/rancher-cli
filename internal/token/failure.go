package token

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// LoginStates is what the persistence step knows at a failure point.
type LoginStates struct {
	Config clierr.ConfigState
	// Commit is the backend's own account of the token write; nil before
	// the store was reached.
	Commit *config.CommitOutcome
	// Compensate is compensated or compensationFailed once compensation
	// ran, empty when it was not attempted.
	Compensate        clierr.CredentialState
	CompensationError string
	// Warnings are the advisories accumulated before the failure (a
	// keyring notice, the read-only-helper warnings); they belong to steps
	// that did happen and a later failure must not swallow them.
	Warnings []string
	// CredentialCleanup is set when the credential this login created was
	// already revoked before the failure (the read-only-helper finalizer
	// ran): the derived token on the derived path, the session token itself
	// on the legacy path. FailLogin reports that outcome instead of
	// revoking a second time.
	CredentialCleanup *clierr.RevokeOutcome
	// ConfigRecovery is set when an entry this operation wrote could not be
	// rolled back to its prior state.
	ConfigRecovery *clierr.ConfigRecovery
	// ClusterToken is the plugin-created login's third fate, already
	// carrying its cleanup outcome: the caller revokes the cluster token
	// (with the login credential as bearer) BEFORE calling FailLogin, which
	// then revokes the credential itself.
	ClusterToken *clierr.TokenFate
}

// credentialStateOf derives the reported state from the backend outcome
// and whether compensation ran.
func credentialStateOf(st LoginStates) clierr.CredentialState {
	if st.Commit == nil {
		return clierr.CredentialNotWritten
	}
	if st.Compensate != "" {
		return st.Compensate
	}
	return st.Commit.State
}

// credentialRecoveryFor builds the local half of recovery, only for states
// that can leave a local residue.
func credentialRecoveryFor(st LoginStates, server string) *clierr.CredentialRecovery {
	state := credentialStateOf(st)
	switch state {
	case clierr.CredentialNotWritten, clierr.CredentialCompensated:
		return nil
	}
	if st.Commit == nil {
		return nil
	}
	return &clierr.CredentialRecovery{
		Backend: st.Commit.Backend, State: state, PriorSource: st.Commit.PriorSource,
		HelperRecordRetained: st.Commit.HelperRecordRetained, CompensationError: st.CompensationError,
		Command: config.LogoutHintFor(server).Field(), DataLossRisk: clierr.DataLossRiskText(server),
	}
}

// FateOf projects one token's evidence onto the schema.
func FateOf(outcome clierr.CreateOutcome, kind clierr.TokenKind, ref *clierr.TokenRef, marked bool, createdAt, expiresAt string, cleanup clierr.RevokeOutcome, search *SAMLHandoff) *clierr.TokenFate {
	if outcome == "" || outcome == clierr.CreateNotAttempted {
		return nil
	}
	if ref != nil {
		kind = ref.Kind
	}
	f := &clierr.TokenFate{Outcome: outcome, Kind: kind, Ref: ref, Marked: marked, CreatedAt: createdAt, ExpiresAt: expiresAt}
	if cleanup.Result != "" {
		c := cleanup
		f.Cleanup = &c
	}
	if search != nil && !marked {
		f.Search = &clierr.UnmarkedTokenSearch{AuthProvider: search.Provider, HandoffAt: search.HandoffAt,
			PollEndedAt: search.PollEndedAt, RequestExpiresAt: search.RequestExpiresAt}
	}
	return f
}

// sessionRefOf names the session token of a derived credential: its
// revocation attempt is the only place its name survives.
func sessionRefOf(cred LoginCredential) *clierr.TokenRef { return cred.SessionTokenCleanup.Ref }

// primaryOf projects an error onto PrimaryFailure, classifying a plain
// error as GENERIC.
func primaryOf(err error) (*clierr.Error, clierr.PrimaryFailure) {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		ce = clierr.From(err)
	}
	return ce, clierr.PrimaryFailure{Code: ce.Code, Message: ce.Message, Hint: ce.Hint,
		Retryable: ce.Retryable, Candidates: ce.Candidates, Details: ce.Details}
}

// compose builds the outer error: the primary's code, message, hint,
// retryability, candidates, and warnings copied once; the details are the
// LoginFailureDetails around the primary.
func compose(primary *clierr.Error, d clierr.LoginFailureDetails) *clierr.Error {
	// The primary stays the cause so errors.Is/As through the composed
	// error still reach context.Canceled, store sentinels, and the
	// classified primary itself; the serialized projection is d.Primary.
	out := clierr.New(primary.Code, primary.Message).WithCause(primary).
		WithRetryable(primary.Retryable).WithCandidates(primary.Candidates).WithDetails(d)
	if primary.Hint != "" {
		out = out.WithHint(primary.Hint)
	}
	if w := primary.Warnings(); len(w) > 0 {
		out = out.WithWarnings(w...)
	}
	return out
}

// tokenFatesFrom projects a credential's tokens onto their fates given the
// derived token's cleanup outcome: on the derived path the session token's
// fate is the cleanup acquisition already performed and the derived token
// takes dClean; on the legacy path the session token is the credential and
// takes dClean itself; a user-supplied credential has no fates.
func tokenFatesFrom(cred LoginCredential, dClean clierr.RevokeOutcome) (session, derived *clierr.TokenFate) {
	switch cred.Provenance {
	case ProvenanceDerived:
		session = FateOf(clierr.CreateKnown, "", sessionRefOf(cred), !cred.SessionTokenUnmarked, cred.SessionCreatedAt, cred.SessionExpiresAt, cred.SessionTokenCleanup, cred.SessionTokenSearch)
		derived = FateOf(clierr.CreateKnown, clierr.TokenKindExt, cred.Ref, true, cred.DerivedCreatedAt, "", dClean, nil)
	case ProvenanceLegacySession:
		session = FateOf(clierr.CreateKnown, "", cred.Ref, !cred.SessionTokenUnmarked, cred.SessionCreatedAt, cred.ExpiresAt, dClean, cred.SessionTokenSearch)
	}
	return session, derived
}

// fatesOf lists every token a login may have created, in reporting order.
func fatesOf(d clierr.LoginFailureDetails) []*clierr.TokenFate {
	return []*clierr.TokenFate{d.SessionToken, d.DerivedToken, d.ClusterToken}
}

// FailLogin turns a post-credential failure into the one structured shape:
// it revokes what this operation created (never a user-supplied token),
// then composes LoginFailureDetails around the primary error.
func FailLogin(primary error, stage clierr.LoginStage, st LoginStates, cred LoginCredential, server string) error {
	ce, pf := primaryOf(primary)
	d := clierr.LoginFailureDetails{Primary: pf, Stage: stage, OperationMarker: cred.Marker}
	if stage == clierr.StageCommit {
		d.ConfigState = st.Config
		d.CredentialState = credentialStateOf(st)
		d.CredentialRecovery = credentialRecoveryFor(st, server)
	}
	d.ConfigRecovery = st.ConfigRecovery
	// The cluster token is revoked by the caller, with the login credential
	// as bearer, before this call: its fate arrives complete.
	d.ClusterToken = st.ClusterToken
	// The credential is revoked once: when the read-only-helper finalizer
	// already did it, its outcome is the one reported.
	var dClean clierr.RevokeOutcome
	if st.CredentialCleanup != nil {
		dClean = *st.CredentialCleanup
	} else {
		dClean = cred.Revoke()
	}
	d.SessionToken, d.DerivedToken = tokenFatesFrom(cred, dClean)
	persisted := d.CredentialState == clierr.CredentialCommitted ||
		d.CredentialState == clierr.CredentialHelperNonAtomic ||
		d.CredentialState == clierr.CredentialCommittedUncompensatable
	d.Recovery = recoveryText(d, server, persisted)
	out := compose(ce, d)
	if h, ok := recoveryHint(d, server, persisted); ok {
		out = out.WithHintCommand(h)
	}
	if len(st.Warnings) > 0 {
		out = out.WithWarnings(st.Warnings...)
	}
	return out
}

// FailAcquire reports a failure that happened while the credential was
// still being acquired: the token fates are the ones acquisition recorded,
// and no persistence system was reached.
func FailAcquire(ae *AcquireError, server string) error {
	ce, pf := primaryOf(ae.Err)
	d := clierr.LoginFailureDetails{Primary: pf, Stage: clierr.StageAuthentication, OperationMarker: ae.Marker,
		SessionToken: FateOf(ae.SessionTokenOutcome, ae.SessionTokenKind, ae.SessionTokenRef, !ae.SessionTokenUnmarked,
			ae.SessionCreatedAt, ae.SessionExpiresAt, ae.SessionTokenCleanup, ae.SessionTokenSearch),
		DerivedToken: FateOf(ae.DerivedCreateOutcome, clierr.TokenKindExt, ae.DerivedTokenRef, true,
			ae.DerivedCreatedAt, "", ae.DerivedTokenCleanup, nil),
	}
	d.Recovery = recoveryText(d, server, false)
	if errors.Is(ae.Err, errCredentialForbidden) {
		d.Recovery = withTokenPermission(ae.SessionUserID, d.Recovery, server)
	}
	out := compose(ce, d)
	if h, ok := recoveryHint(d, server, false); ok {
		out = out.WithHintCommand(h)
	}
	return out
}

// reLogin renders how to sign in again after this failure, and the command
// that does it. A plugin login entry has neither: auth login refuses the
// reserved namespace, so the kubectl command that drives the exec plugin is
// named in prose and no hint is offered.
func reLogin(server string) (text string, hint clierr.Command) {
	login, ok := config.LoginHintFor(server)
	if !ok {
		return "run the kubectl command again; the exec plugin logs in when token reuse is on", clierr.Command{}
	}
	return clierr.Text("log in again", clierr.Lead(" with: "), login), login
}

// ResidualRecovery renders the recovery for one token fate. A hint is
// returned only when the name is known and a credential the command can
// use was persisted; every other case is prose, and an unmarked token
// never mentions a marker.
func ResidualRecovery(f clierr.TokenFate, server, marker string, persisted bool) (text string, hint clierr.Command) {
	login, loginCommand := reLogin(server)
	switch {
	case f.Ref != nil && f.Ref.Kind == clierr.TokenKindV3:
		// The delete command is admin-only (Rancher grants a standard user no
		// rule on management.cattle.io tokens), so it is not valid verbatim
		// for every caller and no hint is offered.
		remedy := v3AdminRemedy(*f.Ref, f.ExpiresAt)
		if persisted {
			return remedy, clierr.Command{}
		}
		if !loginCommand.OK() {
			expiry := " expires"
			if f.ExpiresAt != "" {
				expiry += " at " + f.ExpiresAt
			}
			return clierr.NoteOnce(clierr.Text(fmt.Sprintf("no credential was stored; %s; %s%s; then an administrator can find it", login, f.Ref.String(), expiry),
				clierr.Lead(" with: "), clierr.NewCommand("get management-tokens"))), clierr.Command{}
		}
		return clierr.NoteOnce("no credential was stored; " + login + "; " + remedy), clierr.Command{}
	case f.Ref != nil:
		revoke := clierr.Revoke(*f.Ref)
		if persisted {
			return clierr.Text(fmt.Sprintf("revoke %s", f.Ref.String()), clierr.Lead(" with: "), revoke), revoke
		}
		if !loginCommand.OK() {
			return clierr.NoteOnce(clierr.Text(fmt.Sprintf("no credential was stored; %s; then find %s", login, f.Ref.String()),
				clierr.Lead(" with: "), clierr.NewCommand("get tokens"))), clierr.Command{}
		}
		return clierr.NoteOnce(clierr.Text(fmt.Sprintf("no credential was stored; %s; then revoke %s", login, f.Ref.String()), clierr.Lead(" with: "), revoke)), clierr.Command{}
	case f.Marked:
		// A nameless marked token of known kind is listed in its collection;
		// of unknown kind (a lost or unrecognizable login response) it may be
		// in either, and both are named so the marker search cannot miss it.
		// The management-token list and delete commands are an administrator's to run:
		// Rancher grants a standard user no rule on management.cattle.io tokens.
		extList, v3List := clierr.NewCommand("get tokens"), clierr.NewCommand("get management-tokens")
		carries := fmt.Sprintf("a leftover token created by this login carries %q in its description", "[op:"+marker+"]")
		var then, steps string
		switch f.Kind {
		case clierr.TokenKindV3:
			steps = clierr.Text("an administrator can list tokens", clierr.Lead(" with: "), v3List, "; "+carries+"; an administrator can revoke it with the found name")
		case "":
			then = "then "
			steps = clierr.Text("list tokens", clierr.Lead(" with: "), extList, "; an administrator can also list management tokens", clierr.Lead(" with: "), v3List,
				"; "+carries+"; revoke it with the found name; an administrator can revoke one from the management-token list")
		default:
			then = "then "
			steps = clierr.Text("list tokens", clierr.Lead(" with: "), extList, "; "+carries+"; revoke it with the found name")
		}
		if !persisted {
			return clierr.NoteOnce("no credential was stored; " + login + "; " + then + steps), clierr.Command{}
		}
		return steps, clierr.Command{}
	case f.Search != nil:
		// A SAML sign-in carries no description to mark, so only the provider
		// and the client-side window of the browser handoff are known. They
		// narrow the candidates; they cannot identify one token, and the text
		// says so rather than offering a command that would delete the wrong
		// one.
		return signInLeftover(f, "", marker), clierr.Command{}
	}
	return "", clierr.Command{}
}

// cleanFate is true when a token's create is settled and nothing it created
// outlives this operation.
func cleanFate(f *clierr.TokenFate) bool {
	if f == nil {
		return true
	}
	switch f.Outcome {
	case clierr.CreateNotAttempted, clierr.CreateNotCreated:
		return true
	case clierr.CreateKnown:
		return f.Cleanup != nil && (f.Cleanup.Result == clierr.ResultRevoked || f.Cleanup.Result == clierr.ResultAlreadyGone)
	}
	return false
}

// recoveryText joins the recovery for every token this operation may have
// left behind with the local credential's and the config entry's own
// recovery text.
func recoveryText(d clierr.LoginFailureDetails, server string, persisted bool) string {
	var parts []string
	for _, f := range fatesOf(d) {
		if cleanFate(f) {
			continue
		}
		if text, _ := ResidualRecovery(*f, server, d.OperationMarker, persisted); text != "" {
			parts = append(parts, text)
		}
	}
	if t := credentialRecoveryText(d.CredentialRecovery, server); t != "" {
		parts = append(parts, t)
	}
	if t := configRecoveryText(d.ConfigRecovery); t != "" {
		parts = append(parts, t)
	}
	if len(parts) == 0 {
		text, _ := reLogin(server)
		return text
	}
	return clierr.NoteOnce(strings.Join(parts, "; "))
}

// credentialRecoveryText states what the reached backend may hold; the
// command that clears it rides CredentialRecovery itself. An empty command
// means it was left out because a value in it cannot be printed, which the
// text says.
func credentialRecoveryText(cr *clierr.CredentialRecovery, server string) string {
	if cr == nil {
		return ""
	}
	text := credentialHoldText(cr, server)
	if cr.Command == "" {
		return clierr.WithLeftOutNote(text)
	}
	return text
}

// credentialHoldText is what the reached backend may hold.
func credentialHoldText(cr *clierr.CredentialRecovery, server string) string {
	switch {
	case cr.State == clierr.CredentialCompensationFailed:
		return fmt.Sprintf("the %s entry for %s may still hold this login's token; restoring the earlier value failed (%s)",
			cr.Backend, server, cr.CompensationError)
	case cr.Backend == clierr.BackendExec:
		return fmt.Sprintf("the credential helper may hold either this login's token or a credential from an earlier login for %s", server)
	case cr.Backend == clierr.BackendKeyring && cr.State == clierr.CredentialPossiblyCommitted:
		return fmt.Sprintf("the keychain item for %s may be this login's token, the earlier value, or another value; the CLI could not confirm which", server)
	case cr.Backend == clierr.BackendKeyring && cr.State == clierr.CredentialCommittedUncompensatable:
		return fmt.Sprintf("the keychain holds this login's token for %s and the earlier item could not be read; it was not restored", server)
	}
	return fmt.Sprintf("the %s entry for %s holds this login's token", cr.Backend, server)
}

// configRecoveryText names the entry that could not be restored; the
// command that removes it rides ConfigRecovery itself. An empty command means
// it was left out because the entry name cannot be printed, which the text
// says.
func configRecoveryText(cr *clierr.ConfigRecovery) string {
	if cr == nil {
		return ""
	}
	text := fmt.Sprintf("the config entry %s could not be restored to its prior state", cr.Entry)
	if cr.Command == "" {
		return clierr.WithLeftOutNote(text)
	}
	return text + "; remove it with: " + cr.Command
}

// recoveryHint offers a command only when running it is the right next
// step: the exact revoke when a name is known and a usable credential was
// persisted, a re-login when nothing was persisted and something may
// outlive this operation. Otherwise the primary keeps its own hint.
func recoveryHint(d clierr.LoginFailureDetails, server string, persisted bool) (clierr.Command, bool) {
	unclean := false
	for _, f := range fatesOf(d) {
		if cleanFate(f) {
			continue
		}
		unclean = true
		if _, hint := ResidualRecovery(*f, server, d.OperationMarker, persisted); hasCommand(hint) {
			return hint, true
		}
	}
	if !persisted && unclean {
		_, hint := reLogin(server)
		return hint, hasCommand(hint)
	}
	return clierr.Command{}, false
}

// FinishUnsupportedStore is the one finalizer for a read-only credential
// helper: it starts from the fates the credential already carries, revokes
// what this operation created, and derives warnings and residual tokens
// from the aggregate under one rule: nothing was persisted, so every
// residual gets the nothing-persisted recovery and no delete hint. It
// appends its warnings to the login state itself, in reading order: why
// nothing was persisted, then what to clean up.
func FinishUnsupportedStore(cred LoginCredential, server string, st *LoginStates) (residual []clierr.TokenFate) {
	st.Warnings = append(st.Warnings,
		"credential helper is read-only and the token was not persisted; future commands re-acquire it through the helper")
	dClean := cred.Revoke()
	st.CredentialCleanup = &dClean
	// A cleanup that failed always has a Ref: a credential with no Ref is not
	// attempted, so the warning builders never see a failed fate with no name.
	add := func(f *clierr.TokenFate, derived bool) {
		if f == nil || f.Cleanup == nil || f.Cleanup.Result != clierr.ResultFailed {
			return
		}
		residual = append(residual, *f)
		recovery := func(f clierr.TokenFate) string {
			text, _ := ResidualRecovery(f, server, cred.Marker, false)
			return text
		}
		if derived {
			st.Warnings = append(st.Warnings, fmt.Sprintf("the derived token %s created by this login could not be revoked (%s); %s",
				f.Ref.String(), f.Cleanup.Error, recovery(*f)))
			return
		}
		if text, ok := LoginTokenWarning(f, recovery); ok {
			st.Warnings = append(st.Warnings, text)
		}
	}
	session, derived := tokenFatesFrom(cred, dClean)
	add(session, false)
	add(derived, true)
	return residual
}
