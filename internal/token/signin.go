package token

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// signInAgain is the way to sign in again after a failed sign-in of the exec
// plugin with token reuse off: the kubectl command that runs the plugin,
// named in prose because the plugin's entry has no command of its own.
const signInAgain = "run the kubectl command again"

// signInRefusal is a failed cluster lookup or cluster token create with how
// the person should go on: retry is the recovery text, "" for signInAgain.
type signInRefusal struct {
	err   error
	retry string
}

const (
	// correctCluster is the way on after Rancher refused the cluster the
	// exec entry names: signing in again would name the same cluster.
	correctCluster = "the user can correct --cluster in the kubeconfig exec entry"
	// grantCluster is the way on after Rancher refused the user the cluster.
	grantCluster = "an administrator can grant the user access to cluster %s, or " + correctCluster
)

// signInTokenError words a rejection of the token this sign-in just created
// (the lookup, or the Norman create). The sign-in keeps no login and token
// reuse plays no part in it, so the text names neither.
func signInTokenError(err error) error {
	switch {
	case errors.Is(err, errCredentialUnauthorized):
		return clierr.New(clierr.CodeAuth, "the server rejected the token this sign-in just created: "+clierr.From(err).Message).WithCause(err)
	case errors.Is(err, errCredentialForbidden):
		return clierr.New(clierr.CodeRBAC, "the token this sign-in just created cannot create tokens: "+clierr.From(err).Message).WithCause(err)
	case errors.Is(err, errServerTooOldForReuse):
		return clierr.New(clierr.CodeGeneric, "this Rancher serves neither ext.cattle.io nor a v3 token create; the sign-in cannot create a cluster token").WithCause(err)
	}
	return err
}

// lookupRefusal words a failure of the cluster lookup. A cluster that is not
// among the user's keeps its list and tells the user to correct --cluster.
func lookupRefusal(err error) signInRefusal {
	if clierr.AsNotFound(err) != nil {
		return signInRefusal{err: err, retry: correctCluster}
	}
	return signInRefusal{err: signInTokenError(err)}
}

// createRefusal words a failure of the cluster token create for cluster. A
// create answers 400 for a cluster that does not exist and 403 for one the
// user may not access: Rancher's own refusal, which signing in again
// repeats, so the recovery says who can change it. The status decides; the
// Norman create keeps the token wording in its message.
func createRefusal(err error, cluster string, ext bool) signInRefusal {
	quoted := clierr.ShellQuote(cluster)
	status, _ := clierr.HTTPStatus(err)
	if ext {
		text := "Rancher refused to create a token for cluster " + quoted + " for this user: " + rancherMessage(err)
		switch status {
		case http.StatusForbidden:
			return signInRefusal{err: clierr.New(clierr.CodeRBAC, text).WithCause(err), retry: fmt.Sprintf(grantCluster, quoted)}
		case http.StatusBadRequest:
			return signInRefusal{err: clierr.New(clierr.From(err).Code, text).WithCause(err), retry: correctCluster}
		}
	}
	switch {
	case errors.Is(err, errServerTooOldForReuse):
		return signInRefusal{err: signInTokenError(err), retry: "this command needs Rancher " + config.MinRancherVersion + " or later"}
	case status == http.StatusForbidden:
		return signInRefusal{err: signInTokenError(err), retry: fmt.Sprintf(grantCluster, quoted)}
	case status == http.StatusBadRequest:
		return signInRefusal{err: signInTokenError(err), retry: correctCluster}
	}
	return signInRefusal{err: signInTokenError(err)}
}

// extCreateVerb leads the text of a failed ext create (CreateDerivedToken).
const extCreateVerb = "creating derived token"

// rancherMessage is the message Rancher answered a refused create with: the
// error's text without the words the CLI put before it (the create's verb, or
// the forbidden-credential sentence).
func rancherMessage(err error) string {
	msg := clierr.From(err).Message
	for _, lead := range []string{extCreateVerb + ": ", errCredentialForbidden.Error() + ": "} {
		msg = strings.TrimPrefix(msg, lead)
	}
	return msg
}

// failSignInAcquire reports a login that failed with a token the CLI may
// have created: the login token's fate is the one acquisition recorded. A
// login that failed before it created or attempted one returns its own error.
func failSignInAcquire(ae *AcquireError) error {
	login := FateOf(ae.SessionTokenOutcome, ae.SessionTokenKind, ae.SessionTokenRef, !ae.SessionTokenUnmarked,
		ae.SessionCreatedAt, ae.SessionExpiresAt, ae.SessionTokenCleanup, ae.SessionTokenSearch)
	if login == nil {
		// The login failed before it reached the server (a provider that
		// cannot be chosen, a sign-in refused for want of a terminal): no
		// token exists, so the error is returned as it is.
		return ae.Err
	}
	return failSignIn(ae.Err, clierr.StageAuthentication, ae.Marker, login, nil, "", "")
}

// failSignIn is the error of a failed sign-in for a cluster token: the
// primary failure, the stage, and the fate of both tokens, the one shape
// the plugin's sign-in with token reuse on returns. Either fate may be nil.
// userID is the user the cluster token belongs to, "" when unknown. retry
// is the way on after the leftover tokens, "" for signInAgain; the recovery
// is never empty.
func failSignIn(primary error, stage clierr.LoginStage, marker string, login, cluster *clierr.TokenFate, userID, retry string) *clierr.Error {
	ce, pf := primaryOf(primary)
	d := clierr.LoginFailureDetails{Primary: pf, Stage: stage, OperationMarker: marker,
		SessionToken: login, ClusterToken: cluster, Recovery: signInRecovery(login, cluster, marker, userID, retry)}
	return compose(ce, d)
}

// withSignInFate adds the login token's fate to an error raised after the
// login token was deleted (an owner mismatch, a cancelled wait, an expiry
// that cannot be read). The cluster token's fate is the one the error
// already carries as directWaitDetails. The warnings collected so far ride
// the returned error.
func (si clusterSignIn) withSignInFate(err error, warnings []string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		ce = clierr.From(err)
	}
	var cluster *clierr.TokenFate
	if d, ok := ce.Details.(directWaitDetails); ok && d.Fate != nil {
		// A copy: the details of the other producers keep their fate as it
		// is. The wait's fate omits the expiry the create reported, which
		// the recovery text states.
		c := *d.Fate
		c.ExpiresAt = si.Cluster.ExpiresAt
		cluster = &c
	}
	// The fate travels as clusterToken beside the login token's, so the
	// copy of the primary error carries no details of its own, except the
	// certificate facts of a certificate failure the error was raised for.
	primary := ce.WithMessage(ce.Message)
	primary.Details = nil
	if tls := tlstrust.TypedError(ce.Unwrap()); tls != nil {
		if details, ok := tls.Details.(tlstrust.Details); ok {
			primary.Details = details
		}
	}
	return failSignIn(primary, clierr.StageClusterToken, si.Marker, si.Login, cluster, si.Cluster.UserID, "").WithWarnings(warnings...)
}

// clusterFate is the cluster token's fate at creation, with no cleanup
// outcome yet.
func (si clusterSignIn) clusterFate() *clierr.TokenFate {
	ct := si.Cluster
	return FateOf(ct.Outcome, ct.Kind, ct.Ref, ct.Marked, ct.CreatedAt, ct.ExpiresAt, RevokeOutcome{}, nil)
}

// leftover says how the cluster token ends if its delete fails.
func (si clusterSignIn) leftover() string {
	f := si.clusterFate()
	if f == nil {
		return ""
	}
	return signInLeftover(*f, si.Cluster.UserID, si.Marker)
}

// warning is the warning for a login token that could not be deleted, empty
// when it was deleted.
func (si clusterSignIn) warning() string {
	text, _ := LoginTokenWarning(si.Login, loginTokenNoun(si.Login), func(f clierr.TokenFate) string {
		return signInLeftover(f, si.Cluster.UserID, si.Marker)
	})
	return text
}

// signInRecovery is the recovery text of a failed sign-in: how each token it
// could not delete ends, then retry, or signInAgain when retry is empty.
func signInRecovery(login, cluster *clierr.TokenFate, marker, userID, retry string) string {
	if retry == "" {
		retry = signInAgain
	}
	var parts []string
	for _, t := range []struct {
		noun string
		f    *clierr.TokenFate
	}{{loginTokenNoun(login), login}, {"cluster token", cluster}} {
		if cleanFate(t.f) {
			continue
		}
		text := signInLeftover(*t.f, userID, marker)
		if text == "" {
			continue
		}
		subject := "the " + t.noun
		if t.f.Ref != nil {
			subject += " " + t.f.Ref.String()
		}
		parts = append(parts, subject+": "+text)
	}
	if len(parts) == 0 {
		return retry
	}
	return strings.Join(parts, "; ") + "; " + retry
}

// loginTokenNoun is Rancher's name for the token a sign-in creates: a
// session token for the password and OAuth flows, a kubeconfig token for
// SAML, which the browser handoff identifies.
func loginTokenNoun(f *clierr.TokenFate) string {
	if f != nil && f.Search != nil {
		return "kubeconfig token"
	}
	return "session token"
}

// signInWho is who acts on a token left behind, and how that person's
// directory is named: the cluster token's owner when known.
func signInWho(userID string) (who, as string) {
	if userID == "" {
		return "the user who signed in", "that user"
	}
	return "user " + clierr.ShellQuote(userID), "user " + clierr.ShellQuote(userID)
}

// signInLeftover says how a token the exec plugin's sign-in could not delete
// ends, for the person who can act on it. A command is named only with who
// runs it, so it is valid as written for that person: a v3 token is an
// administrator's to delete, an ext token its owner's, from a directory
// signed in as that owner. userID is the owner as the cluster token's create
// named it; "" when unknown. marker finds a token whose name is unknown.
func signInLeftover(f clierr.TokenFate, userID, marker string) string {
	who, as := signInWho(userID)
	switch {
	case f.Ref != nil && f.Ref.Kind == clierr.TokenKindV3:
		return v3AdminRemedy(*f.Ref, f.ExpiresAt)
	case f.Ref != nil:
		_, revoke := clierr.RecoveryCommands(*f.Ref)
		text := who + " can delete it with: " + clierr.MarkRemedy(revoke) + ", from a directory signed in as " + as +
			" (" + clierr.Rancher + " auth login signs one in)"
		if f.ExpiresAt != "" {
			text += "; or it expires at " + f.ExpiresAt
		}
		return text
	case f.Marked:
		find := fmt.Sprintf("delete the one whose description carries %q", "[op:"+marker+"]")
		var lists []string
		if f.Kind != clierr.TokenKindV3 {
			list, _ := clierr.RecoveryCommands(clierr.TokenRef{Kind: clierr.TokenKindExt})
			lists = append(lists, who+" can list tokens with: "+clierr.MarkRemedy(list)+", from a directory signed in as "+as+", and "+find)
		}
		if f.Kind != clierr.TokenKindExt {
			list, _ := clierr.RecoveryCommands(clierr.TokenRef{Kind: clierr.TokenKindV3})
			lists = append(lists, "an administrator can list tokens with: "+clierr.MarkRemedy(list)+" and "+find)
		}
		return strings.Join(lists, "; ")
	case f.Search != nil:
		window := "at or after " + f.Search.HandoffAt
		if f.Search.RequestExpiresAt != "" {
			window += " and not after " + f.Search.RequestExpiresAt
		}
		return fmt.Sprintf("a kubeconfig token may have been created through %s %s; the CLI stopped waiting at %s, "+
			"but the browser sign-in can still complete after that, and the token may be newer; "+
			"these facts narrow the candidates but cannot identify one token; "+
			"an administrator can inspect them with: %s; the user who signed in can find them in the UI under Account & API Keys",
			f.Search.AuthProvider, window, f.Search.PollEndedAt, clierr.Rancher+" get management-tokens -o json")
	}
	return ""
}

// signInRevokedClause says what became of the cluster token the sign-in
// created and then refused to hand kubectl: revoked, or how it ends.
// leftover is signInLeftover for that token.
func signInRevokedClause(subject string, ref *TokenRef, out RevokeOutcome, leftover string) string {
	switch {
	case out.Result == ResultRevoked || out.Result == ResultAlreadyGone:
		return subject + " was revoked"
	case ref == nil:
		return subject + " could not be identified; it was not revoked"
	}
	cause := ""
	if out.Error != "" {
		cause = " (" + out.Error + ")"
	}
	return subject + " could not be revoked" + cause + "; " + leftover
}

// SessionTokenFate is the fate of the transient session token a login
// created and deleted: what became of it and, when the delete failed, what
// is left. nil when the login created none.
func SessionTokenFate(cred LoginCredential) *clierr.TokenFate {
	return FateOf(clierr.CreateKnown, "", cred.SessionTokenCleanup.Ref, !cred.SessionTokenUnmarked,
		cred.SessionCreatedAt, cred.SessionExpiresAt, cred.SessionTokenCleanup, cred.SessionTokenSearch)
}

// LoginTokenWarning is the warning for the token a login created and could
// not delete, which outlives the login that succeeded: the token, why the
// delete failed, and recovery(f), how it ends. noun is Rancher's name for the
// token. It reports false when f names no token whose delete failed.
func LoginTokenWarning(f *clierr.TokenFate, noun string, recovery func(clierr.TokenFate) string) (string, bool) {
	if f == nil || f.Ref == nil || f.Cleanup == nil || f.Cleanup.Result != clierr.ResultFailed {
		return "", false
	}
	return fmt.Sprintf("the transient %s %s created by this login could not be revoked (%s); %s",
		noun, f.Ref.String(), f.Cleanup.Error, recovery(*f)), true
}
