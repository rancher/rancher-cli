package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// userCheck is what a call with --user-id checks its tokens against, and
// what its remedies are filled in with: the user id ("" for a call
// without --user-id, which checks nothing), the call's auth config (its
// --auth-provider with --user-id, "" for none), the call's cluster, its
// canonical server URL, and the CA it resolved (a path or an inline PEM).
type userCheck struct {
	userID, authConfig, clusterID, serverID, caSource string
	// caFrom is where caSource came from: --cacert, RANCHER_CLI_CACERT or a
	// server entry, whose name the remedy gives for an inline CA.
	caFrom config.CASource
}

// otherOwner reports whether a recorded owner is known and is not the
// call's user.
func (u userCheck) otherOwner(recorded string) bool {
	return u.userID != "" && recorded != "" && recorded != u.userID
}

// otherAuthConfig reports whether a recorded auth config is known and is
// not the call's. A call without an auth config has none to differ from.
func (u userCheck) otherAuthConfig(recorded string) bool {
	return u.authConfig != "" && recorded != "" && recorded != u.authConfig
}

// relogin is the way to a login of the call's user, through the call's
// auth config when it names one, for entry, as a sentence names it, and the
// command alone, the hint. For a user entry it is a login of that entry
// with a piped token, which needs no terminal. For a plugin login entry,
// which auth login refuses by name, it is the entry's removal, after which
// the next call signs in as the user in a terminal or uses a login of an
// entry of the user's own choice; there is no hint.
func (u userCheck) relogin(cfg *config.Config, entry string, plugin bool) (text, hint string) {
	quoted := clierr.ShellQuote(entry)
	through := ""
	if u.authConfig != "" {
		through = " through auth config " + clierr.ShellQuote(u.authConfig)
	}
	if plugin || config.IsExecLoginEntryName(entry) {
		login := NoTerminalLogin(cfg, "", u.clusterID, u.serverID, u.caSource, u.caFrom, false)
		piped := "a login piped to: "
		if u.authConfig != "" {
			piped = "a login made" + through + " and piped to: "
		}
		next := "the next call signs in as " + clierr.ShellQuote(u.userID) + through + " in a terminal; or it uses " + piped + login.Text()
		if facts := login.Facts(); facts != "" {
			next = facts + "; " + next
		}
		return "remove " + quoted + " with: " + clierr.Rancher + " auth logout" + clierr.ShellQuoteArgs("", entry) + "; then " +
			clierr.Rancher + " config delete-server" + clierr.ShellQuoteArgs("", entry) + "; " + next, ""
	}
	of := ""
	if u.authConfig != "" {
		of = " made" + through
	}
	hint = clierr.Rancher + " auth login" + clierr.ShellQuoteArgs(" --token-stdin", entry)
	return "pipe an API token of " + clierr.ShellQuote(u.userID) + of + " to: " + hint, hint
}

// authConfigRemedy is the way to a login of entry made through the call's
// auth config: for a user entry, a sign-in through it, the command being
// the hint, or the piped token relogin gives; for a plugin login entry
// relogin's removal, with no hint.
func (u userCheck) authConfigRemedy(cfg *config.Config, entry string, plugin bool) (text, hint string) {
	text, _ = u.relogin(cfg, entry, plugin)
	if plugin || config.IsExecLoginEntryName(entry) {
		return text, ""
	}
	signIn := clierr.Rancher + " auth login" + clierr.ShellQuoteArgs(" --auth-provider "+clierr.ShellQuote(u.authConfig), entry)
	return "sign in through auth config " + clierr.ShellQuote(u.authConfig) + " with: " + signIn + "; or " + text, signIn
}

// entryAuthConfigRefused is the refusal of a call with an auth config whose
// entry holds a login made through another one, or, for a plugin login entry
// that holds none, was last logged in through another: no request is made
// with it. The way on is a login of that entry through the call's auth
// config.
func (u userCheck) entryAuthConfigRefused(cfg *config.Config, entry, recorded string, plugin, holds bool) *clierr.Error {
	subject := "server entry " + clierr.ShellQuote(entry)
	state := " holds a login made through auth config " + clierr.ShellQuote(recorded)
	if plugin {
		subject += ", the exec plugin's own login entry,"
		if !holds {
			state = " was last logged in through auth config " + clierr.ShellQuote(recorded)
		}
	}
	text, hint := u.authConfigRemedy(cfg, entry, plugin)
	return clierr.New(clierr.CodeAuth, subject+state+", and this exec entry names --auth-provider "+clierr.ShellQuote(u.authConfig)+"; "+text).WithHint(hint)
}

// loginOfNewEntry is the way to a login when no user entry is the call's to
// log in: a new entry for the server, with the CA the call resolved, made
// current, then the kubeconfig regenerated from it. It is three commands,
// so it has no hint. The caller adds the note about the entry it selects.
func (u userCheck) loginOfNewEntry(cfg *config.Config) string {
	login := NoTerminalLogin(cfg, "", u.clusterID, u.serverID, u.caSource, u.caFrom, false)
	return login.Instruction("pipe an API token to: ") + "; then " + clierr.Rancher + " config use-server <name>; then " +
		kubeconfigRemedy(u.clusterID)
}

// entryOwnerRefused is the refusal of a call whose entry holds a login
// another user's token was recorded for, or for a plugin login entry that
// holds none was last logged in as another user: no request is made with
// it. The call's user logs in again, or, with a cluster, the kubeconfig is
// regenerated from an entry that holds that user's login.
func (u userCheck) entryOwnerRefused(cfg *config.Config, entry, owner string, plugin, holds bool) *clierr.Error {
	subject := "server entry " + clierr.ShellQuote(entry)
	state := " holds the login of Rancher user " + clierr.ShellQuote(owner)
	if plugin {
		subject += ", the exec plugin's own login entry,"
		if !holds {
			state = " was last logged in as Rancher user " + clierr.ShellQuote(owner)
		}
	}
	text, hint := u.relogin(cfg, entry, plugin)
	msg := subject + state + ", and this exec entry names user " + clierr.ShellQuote(u.userID) + " with --user-id; " + text
	if u.clusterID != "" {
		current := ""
		if cfg != nil {
			current = cfg.CurrentServer
		}
		msg += "; or regenerate the kubeconfig from an entry that holds the login of " + clierr.ShellQuote(u.userID) + " with: " +
			useServerCommand("") + "; then " + kubeconfigRemedy(u.clusterID) + useServerNote(current)
	}
	return clierr.New(clierr.CodeAuth, msg).WithHint(hint)
}

// entryNameTooLong is the refusal of a plugin-created login whose entry
// would have a name the credential store cannot hold: a login of a user
// entry under a shorter name is the way to a stored login, a sign-in through
// the call's auth config when it names one, or a piped token.
func (u userCheck) entryNameTooLong(cfg *config.Config, length int) *clierr.Error {
	login := NoTerminalLogin(cfg, "", u.clusterID, u.serverID, u.caSource, u.caFrom, false)
	piped := "pipe an API token"
	if u.userID != "" {
		piped += " of " + clierr.ShellQuote(u.userID)
	}
	subject := "the exec plugin's login entry for this server"
	remedy := login.Instruction("to log in under a shorter name, " + piped + " to: ")
	if u.authConfig != "" {
		subject += ", for Rancher user " + clierr.ShellQuote(u.userID) + " through auth config " + clierr.ShellQuote(u.authConfig) + ","
		piped += " made through auth config " + clierr.ShellQuote(u.authConfig)
		remedy = login.Instruction("to log in under a shorter name, sign in with: " + login.signIn(u.authConfig) + "; or " + piped + " to: ")
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"%s would have a name of %d characters, and the credential store accepts at most %d; %s",
		subject, length, config.MaxEntryNameLength, remedy))
}

// revokedClause says what became of a token this call created and then
// refused to hand kubectl: revoked, or how to revoke it.
func revokedClause(subject string, ref *TokenRef, out RevokeOutcome) string {
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
	if ref.Kind == TokenKindV3 {
		return subject + " could not be revoked" + cause + "; " + v3AdminRemedy(*ref, "")
	}
	_, revoke := clierr.RecoveryCommands(*ref)
	return subject + " could not be revoked" + cause + "; revoke it with: " + clierr.MarkRemedy(revoke)
}

// revokeRefused revokes a token the call created and refused to hand
// kubectl, with the bearer that created it, detached from the call's
// cancellation, and returns what became of it.
func revokeRefused(ctx context.Context, fresh *freshToken) RevokeOutcome {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	return fresh.cleanup(cctx)
}

// fateDetails is the refused token's fate as an error's details, the shape
// a cancelled wait for the direct endpoint reports it in.
func fateDetails(fresh *freshToken, out RevokeOutcome) directWaitDetails {
	c := fresh.create
	return directWaitDetails{Fate: &clierr.TokenFate{Outcome: c.Outcome, Kind: c.Kind, Ref: c.Ref, Marked: c.Marked, CreatedAt: c.CreatedAt, Cleanup: &out},
		unscoped: c.ClusterName == ""}
}

// checkCreatedOwner is the check of a token created from a stored login for
// a call with --user-id: the create response must name the call's user as
// the owner. Any other answer revokes the token, with the stored login as
// bearer, before anything is cached. sc is the entry whose login made it.
func (u userCheck) checkCreatedOwner(ctx context.Context, cfg *config.Config, created CreateResult, fresh *freshToken, sc storedCredential) error {
	if created.UserID == u.userID {
		return nil
	}
	out := revokeRefused(ctx, fresh)
	if created.UserID == "" {
		return clierr.New(clierr.CodeGeneric, "the token create response for server entry "+clierr.ShellQuote(sc.name)+
			" named no owner; it cannot be checked against --user-id "+clierr.ShellQuote(u.userID)+"; "+
			revokedClause("the token", created.Ref, out)).WithDetails(fateDetails(fresh, out))
	}
	text, hint := u.relogin(cfg, sc.name, sc.entry.Provenance == clierr.ProvenanceExecLogin)
	return clierr.New(clierr.CodeAuth, "the stored login of server entry "+clierr.ShellQuote(sc.name)+" belongs to Rancher user "+clierr.ShellQuote(created.UserID)+
		", not to "+clierr.ShellQuote(u.userID)+", which this exec entry names with --user-id; "+
		revokedClause("the token created from it", created.Ref, out)+"; "+text).WithHint(hint).WithDetails(fateDetails(fresh, out))
}

// refuseNoAuthConfig is the refusal of a token created from a stored login
// whose create response named no auth config, for a call with an auth
// config: the token is revoked, with the stored login as bearer.
func (u userCheck) refuseNoAuthConfig(ctx context.Context, created CreateResult, fresh *freshToken, sc storedCredential) error {
	out := revokeRefused(ctx, fresh)
	return clierr.New(clierr.CodeAuth, "the token create response for server entry "+clierr.ShellQuote(sc.name)+
		" named no auth config; it cannot be checked against --auth-provider "+clierr.ShellQuote(u.authConfig)+"; "+
		revokedClause("the token", created.Ref, out)).WithDetails(fateDetails(fresh, out))
}

// refuseOtherAuthConfig is the refusal of a token created from a stored
// login that was made through another auth config than the call's: the token
// is revoked, with the stored login as bearer, and the way on is a login of
// that entry through the call's auth config.
func (u userCheck) refuseOtherAuthConfig(ctx context.Context, cfg *config.Config, created CreateResult, fresh *freshToken, sc storedCredential) error {
	out := revokeRefused(ctx, fresh)
	text, hint := u.authConfigRemedy(cfg, sc.name, sc.entry.Provenance == clierr.ProvenanceExecLogin)
	return clierr.New(clierr.CodeAuth, "the token created from the stored login of server entry "+clierr.ShellQuote(sc.name)+
		" was made through auth config "+clierr.ShellQuote(created.AuthConfig)+", not "+clierr.ShellQuote(u.authConfig)+
		", which this exec entry names with --auth-provider; "+revokedClause("the token", created.Ref, out)+"; "+text).
		WithHint(hint).WithDetails(fateDetails(fresh, out))
}

// noOwnerWithoutExtTokens is the refusal of a call with --user-id on a
// Rancher that does not serve ext.cattle.io: the stored login itself would
// be handed kubectl, and no response names whose login it is.
func (u userCheck) noOwnerWithoutExtTokens(entry string) *clierr.Error {
	return clierr.New(clierr.CodeCapabilityUnsupported, "this Rancher does not serve ext.cattle.io; no token create reports whose login server entry "+
		clierr.ShellQuote(entry)+" holds, and --user-id "+clierr.ShellQuote(u.userID)+" cannot be checked; Rancher "+config.MinRancherVersion+" or later is required").
		WithDetails(clierr.CapabilityUnsupportedDetails{Feature: "ext.cattle.io/v1 tokens", Server: u.serverID})
}

// selfSubjectReviewPath is the SelfSubjectReview collection under a
// cluster's path through the Rancher proxy.
const selfSubjectReviewPath = "/apis/authentication.k8s.io/v1/selfsubjectreviews"

// maxOwnerBody bounds what the owner request reads of an answer; a review
// is a few hundred bytes.
const maxOwnerBody = 1 << 20

// requestOwner asks whose token bearer is: one SelfSubjectReview through
// the cluster's own path, since a token scoped to the cluster is refused on
// any other, and the user it reports, which Rancher sets to the token's
// owner. The answer carries no uid; the name is compared.
func requestOwner(ctx context.Context, client *http.Client, base, clusterID, bearer string) (string, error) {
	body := []byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`)
	req, err := kube.NewWriteRequest(ctx, http.MethodPost, base+"/k8s/clusters/"+url.PathEscape(clusterID)+selfSubjectReviewPath, bytes.NewReader(body))
	if err != nil {
		return "", clierr.New(clierr.CodeGeneric, "building the SelfSubjectReview request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	var raw []byte
	if err == nil {
		// One byte past the bound tells an answer that is too large from
		// one that fits; nothing past it is read.
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxOwnerBody+1))
		_ = resp.Body.Close()
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
		if ce := tlstrust.TypedError(err); ce != nil {
			return "", ce
		}
		return "", clierr.Retryable(clierr.CodeGeneric, "posting a SelfSubjectReview: "+err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", clierr.FromResponse("posting a SelfSubjectReview", http.MethodPost, resp.StatusCode, raw,
			&clierr.Request{Cluster: clusterID, Endpoint: true, Unresolved: true})
	}
	var review struct {
		Status struct {
			UserInfo struct {
				Username string `json:"username"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	if len(raw) > maxOwnerBody {
		return "", clierr.New(clierr.CodeGeneric, fmt.Sprintf("the SelfSubjectReview response is larger than %d bytes", maxOwnerBody))
	}
	if json.Unmarshal(raw, &review) != nil {
		return "", clierr.New(clierr.CodeGeneric, "the SelfSubjectReview response is not readable")
	}
	if review.Status.UserInfo.Username == "" {
		return "", clierr.New(clierr.CodeGeneric, "the SelfSubjectReview response named no user")
	}
	return review.Status.UserInfo.Username, nil
}

// checkSignInOwner is the check of the token an interactive sign-in made
// for a call with --user-id, the token created with the sign-in's login
// token, scoped to the cluster or, with none, for Rancher itself: the owner
// its create response named (an ext token's spec.userID), when it named one,
// else the one the owner request reports (through the local cluster when the
// call has no cluster), must be the call's user, and, for a call with an auth
// config, the auth config the response names must be that one
// (checkSignInAuthConfig). Any other answer, or a failed request, revokes
// the token with its own bearer before anything is cached; a token that
// cannot be revoked ends as fresh.leftover says.
func (u userCheck) checkSignInOwner(ctx context.Context, client *http.Client, base string, created CreateResult, fresh *freshToken) error {
	owner := created.UserID
	if owner == "" {
		var err error
		cluster := u.clusterID
		if cluster == "" {
			cluster = "local"
		}
		owner, err = requestOwner(ctx, client, base, cluster, created.Bearer)
		if err != nil {
			out := revokeRefused(ctx, fresh)
			code, retryable := clierr.CodeGeneric, false
			if ce, ok := errors.AsType[*clierr.Error](err); ok {
				code, retryable = ce.Code, ce.Retryable
			}
			return clierr.New(code, "checking the signed-in user against --user-id "+clierr.ShellQuote(u.userID)+": "+err.Error()+"; "+
				signInRevokedClause("the token the sign-in created", created.Ref, out, fresh.leftover)).WithRetryable(retryable).WithCause(err).WithDetails(fateDetails(fresh, out))
		}
	}
	if owner == u.userID {
		return u.checkSignInAuthConfig(ctx, created, fresh)
	}
	out := revokeRefused(ctx, fresh)
	return clierr.New(clierr.CodeAuth, "the sign-in was made as Rancher user "+clierr.ShellQuote(owner)+", and this exec entry names user "+
		clierr.ShellQuote(u.userID)+" with --user-id; "+signInRevokedClause("the token it created", created.Ref, out, fresh.leftover)+"; sign in as "+clierr.ShellQuote(u.userID)).
		WithDetails(fateDetails(fresh, out))
}

// checkSignInAuthConfig is the second half of checkSignInOwner, for a call
// with an auth config: the token's create response must name that auth
// config. A browser sign-in chooses its auth config on Rancher's page, so
// only it can return another; a response that names none cannot be checked.
// Either revokes the token with its own bearer before anything is cached.
func (u userCheck) checkSignInAuthConfig(ctx context.Context, created CreateResult, fresh *freshToken) error {
	if u.authConfig == "" || created.AuthConfig == u.authConfig {
		return nil
	}
	out := revokeRefused(ctx, fresh)
	revoked := signInRevokedClause("the token it created", created.Ref, out, fresh.leftover)
	if created.AuthConfig == "" {
		return clierr.New(clierr.CodeAuth, "the token create response of the sign-in named no auth config; it cannot be checked against --auth-provider "+
			clierr.ShellQuote(u.authConfig)+"; "+revoked).WithDetails(fateDetails(fresh, out))
	}
	return clierr.New(clierr.CodeAuth, "the sign-in was made through auth config "+clierr.ShellQuote(created.AuthConfig)+", not "+clierr.ShellQuote(u.authConfig)+
		", which this exec entry names with --auth-provider; "+revoked+"; on the sign-in page, choose auth config "+clierr.ShellQuote(u.authConfig)).
		WithDetails(fateDetails(fresh, out))
}

// pluginSignInRefused is the refusal of a plugin-created login for a call
// with --user-id whose sign-in was made as another user, or whose creates
// named no owner at all: nothing is stored, and the caller revokes what
// the login created.
func (u userCheck) pluginSignInRefused(owner string) *clierr.Error {
	if owner == "" {
		return clierr.New(clierr.CodeAuth, "the tokens the sign-in created named no owner; the sign-in cannot be checked against --user-id "+
			clierr.ShellQuote(u.userID)+"; nothing was stored")
	}
	return clierr.New(clierr.CodeAuth, "the sign-in was made as Rancher user "+clierr.ShellQuote(owner)+", and this exec entry names user "+
		clierr.ShellQuote(u.userID)+" with --user-id; nothing was stored")
}

// pluginAuthConfigRefused is the refusal of a plugin-created login for a call
// with an auth config whose sign-in was made through another one, or whose
// creates named none at all: nothing is stored, and the caller revokes what
// the login created. named is the auth config the creates named, "" for
// none.
func (u userCheck) pluginAuthConfigRefused(named string) *clierr.Error {
	if named == "" {
		return clierr.New(clierr.CodeAuth, "the tokens the sign-in created named no auth config; they cannot be checked against --auth-provider "+
			clierr.ShellQuote(u.authConfig)+"; nothing was stored")
	}
	return clierr.New(clierr.CodeAuth, "the sign-in was made through auth config "+clierr.ShellQuote(named)+", not "+clierr.ShellQuote(u.authConfig)+
		", which this exec entry names with --auth-provider; nothing was stored; on the sign-in page, choose auth config "+
		clierr.ShellQuote(u.authConfig))
}
