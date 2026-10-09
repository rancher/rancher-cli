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

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// userCheck is what a call with --user-id checks its tokens against, and
// what its remedies are filled in with: the user id ("" for a call
// without --user-id, which checks nothing), the call's cluster, its
// canonical server URL, and the CA it resolved (a path or an inline PEM).
type userCheck struct {
	userID, clusterID, serverID, caSource string
	// caEntry is the server entry whose config.yaml record holds caSource,
	// when that is not the exec entry: a login of a plugin login entry's
	// URL that is not the call's.
	caEntry string
}

// pluginEntrySkipped is the lookup's answer for a call with --user-id
// whose plugin login entry records another owner: the call neither reads
// nor writes that entry, which may be another user's login used by their
// kubeconfigs, and signs in instead.
type pluginEntrySkipped struct {
	entry, owner string
}

func (e *pluginEntrySkipped) Error() string {
	return "the exec plugin's own login entry " + e.entry + " records Rancher user " + e.owner + " as its owner"
}

// otherOwner reports whether a recorded owner is known and is not the
// call's user.
func (u userCheck) otherOwner(recorded string) bool {
	return u.userID != "" && recorded != "" && recorded != u.userID
}

// loginOfEntry is the login of a user entry for the call's user without a
// terminal, as a sentence names it, and the command alone, the hint. An
// entry with a plugin login entry's name, which auth login refuses, gets
// the login of a new entry instead, with no hint.
func (u userCheck) loginOfEntry(cfg *config.Config, entry string) (text, hint string) {
	if config.IsExecLoginEntryName(entry) {
		return u.loginOfNewEntry(cfg), ""
	}
	hint = clierr.Rancher + " auth login " + clierr.ShellQuote(entry) + " --token-stdin"
	return "pipe an API token of " + clierr.ShellQuote(u.userID) + " to " + hint, hint
}

// loginOfNewEntry is the way to the call's user's login when no user entry
// is the call's to log in: a new entry for the server, with the CA the
// call resolved, made current, then the kubeconfig regenerated from it.
// It is three commands, so it has no hint. An empty user id names no
// user.
func (u userCheck) loginOfNewEntry(cfg *config.Config) string {
	login := NoTerminalLogin(cfg, "", u.clusterID, u.serverID, u.caSource, false)
	login.InlineCAEntry = u.caEntry
	current := ""
	if cfg != nil {
		current = cfg.CurrentServer
	}
	of := ""
	if u.userID != "" {
		of = " of " + clierr.ShellQuote(u.userID)
	}
	return "pipe an API token" + of + " to " + login.Text() + ", then " + useServerRemedy("<name>", current) +
		", then " + clierr.Rancher + " kubeconfig --cluster " + clierr.ShellQuote(u.clusterID)
}

// NewEntryLoginRemedy is the way to a login whose owner is recorded when
// no user entry is a call's to log in: a new entry for serverURL, with
// the CA the call resolved (a path, or an inline PEM it cannot name),
// made current, then the kubeconfig for clusterID regenerated from it.
// userID names the user the token must be of; empty names none.
func NewEntryLoginRemedy(cfg *config.Config, userID, clusterID, serverURL, ca string) string {
	return userCheck{userID: userID, clusterID: clusterID, serverID: serverURL, caSource: ca}.loginOfNewEntry(cfg)
}

// entryOwnerRefused is the refusal of a call whose entry holds a login
// another user's token was recorded for: no request is made with it. A
// user entry is logged in again for the call's user, or the kubeconfig is
// regenerated from an entry that holds that user's login; a plugin login
// entry is never the login target, so a new entry is.
func (u userCheck) entryOwnerRefused(cfg *config.Config, entry, owner string, plugin, holds bool) *clierr.Error {
	names := " and this exec entry names user " + clierr.ShellQuote(u.userID) + " with --user-id; "
	if plugin {
		state := "holds the login of Rancher user " + clierr.ShellQuote(owner)
		if !holds {
			state = "records Rancher user " + clierr.ShellQuote(owner) + " as its owner"
		}
		return clierr.New(clierr.CodeAuth, "server entry "+clierr.ShellQuote(entry)+", the exec plugin's own login entry, "+state+","+
			names+u.loginOfNewEntry(cfg))
	}
	current := ""
	if cfg != nil {
		current = cfg.CurrentServer
	}
	text, hint := u.loginOfEntry(cfg, entry)
	return clierr.New(clierr.CodeAuth, "server entry "+clierr.ShellQuote(entry)+" holds the login of Rancher user "+clierr.ShellQuote(owner)+","+
		names+text+", or regenerate the kubeconfig from an entry that holds the login of "+clierr.ShellQuote(u.userID)+" with "+
		useServerRemedy("<entry>", current)+", then "+clierr.Rancher+" kubeconfig --cluster "+clierr.ShellQuote(u.clusterID)).WithHint(hint)
}

// revokedClause says what became of a token this call created and then
// refused to hand kubectl: revoked, or how to revoke it.
func revokedClause(subject string, ref *TokenRef, out RevokeOutcome) string {
	switch {
	case out.Result == ResultRevoked || out.Result == ResultAlreadyGone:
		return subject + " was revoked"
	case ref == nil:
		return subject + " could not be identified, so it was not revoked"
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
	return directWaitDetails{Fate: &clierr.TokenFate{Outcome: c.Outcome, Kind: c.Kind, Ref: c.Ref, Marked: c.Marked, CreatedAt: c.CreatedAt, Cleanup: &out}}
}

// checkCreatedOwner is the check of a token created from a stored login for
// a call with --user-id: the create response must name the call's user as
// the owner. Any other answer revokes the token, with the stored login as
// bearer, before anything is cached. sc is the entry whose login made it
// and identified the entry the call's --name identifies.
func (u userCheck) checkCreatedOwner(ctx context.Context, cfg *config.Config, created CreateResult, fresh *freshToken, sc storedCredential, identified string) error {
	if created.UserID == u.userID {
		return nil
	}
	out := revokeRefused(ctx, fresh)
	if created.UserID == "" {
		return clierr.New(clierr.CodeGeneric, "the token create response for server entry "+clierr.ShellQuote(sc.name)+
			" named no owner, so it cannot be checked against --user-id "+clierr.ShellQuote(u.userID)+"; "+
			revokedClause("the token", created.Ref, out)).WithDetails(fateDetails(fresh, out))
	}
	owner := clierr.ShellQuote(created.UserID)
	notU := " belongs to Rancher user " + owner + ", not to " + clierr.ShellQuote(u.userID) + ", which this exec entry names with --user-id; " +
		revokedClause("the token created from it", created.Ref, out) + "; "
	plugin := sc.entry.Provenance == clierr.ProvenanceExecLogin
	var e *clierr.Error
	switch {
	case !plugin:
		text, hint := u.loginOfEntry(cfg, sc.name)
		e = clierr.New(clierr.CodeAuth, "the stored login of server entry "+clierr.ShellQuote(sc.name)+notU+text).WithHint(hint)
	case identified != "" && identified != sc.name:
		text, hint := u.loginOfEntry(cfg, identified)
		e = clierr.New(clierr.CodeAuth, "server entry "+clierr.ShellQuote(identified)+" has no login, and the exec plugin's own login entry "+
			clierr.ShellQuote(sc.name)+" it fell back to"+notU+text).WithHint(hint)
	default:
		e = clierr.New(clierr.CodeAuth, "the exec plugin's own login entry "+clierr.ShellQuote(sc.name)+notU+u.loginOfNewEntry(cfg))
	}
	return e.WithDetails(fateDetails(fresh, out))
}

// noOwnerWithoutExtTokens is the refusal of a call with --user-id on a
// Rancher that does not serve ext.cattle.io: the stored login itself would
// be handed kubectl, and no response names whose login it is.
func (u userCheck) noOwnerWithoutExtTokens(entry string) *clierr.Error {
	return clierr.New(clierr.CodeCapabilityUnsupported, "this Rancher does not serve ext.cattle.io, so no token create reports whose login server entry "+
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/k8s/clusters/"+url.PathEscape(clusterID)+selfSubjectReviewPath, bytes.NewReader(body))
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
// for a call with --user-id: the owner the login response named, when it
// named one, else the one the owner request reports, must be the call's
// user. Any other answer, or a failed request, revokes the token, which
// revokes itself, before anything is cached.
func (u userCheck) checkSignInOwner(ctx context.Context, client *http.Client, base string, created CreateResult, fresh *freshToken) error {
	owner := created.UserID
	if owner == "" {
		var err error
		owner, err = requestOwner(ctx, client, base, u.clusterID, created.Bearer)
		if err != nil {
			out := revokeRefused(ctx, fresh)
			code, retryable := clierr.CodeGeneric, false
			if ce, ok := errors.AsType[*clierr.Error](err); ok {
				code, retryable = ce.Code, ce.Retryable
			}
			return clierr.New(code, "checking the signed-in user against --user-id "+clierr.ShellQuote(u.userID)+": "+err.Error()+"; "+
				revokedClause("the token the sign-in created", created.Ref, out)).WithRetryable(retryable).WithCause(err).WithDetails(fateDetails(fresh, out))
		}
	}
	if owner == u.userID {
		return nil
	}
	out := revokeRefused(ctx, fresh)
	return clierr.New(clierr.CodeAuth, "the sign-in was made as Rancher user "+clierr.ShellQuote(owner)+", and this exec entry names user "+
		clierr.ShellQuote(u.userID)+" with --user-id; "+revokedClause("the token it created", created.Ref, out)+"; sign in as "+clierr.ShellQuote(u.userID)).
		WithDetails(fateDetails(fresh, out))
}

// pluginSignInRefused is the refusal of a plugin-created login for a call
// with --user-id whose sign-in was made as another user, or whose creates
// named no owner at all: nothing is stored, and the caller revokes what
// the login created.
func (u userCheck) pluginSignInRefused(owner string) *clierr.Error {
	if owner == "" {
		return clierr.New(clierr.CodeAuth, "the tokens the sign-in created named no owner, so the sign-in cannot be checked against --user-id "+
			clierr.ShellQuote(u.userID)+"; nothing was stored")
	}
	return clierr.New(clierr.CodeAuth, "the sign-in was made as Rancher user "+clierr.ShellQuote(owner)+", and this exec entry names user "+
		clierr.ShellQuote(u.userID)+" with --user-id; nothing was stored")
}
