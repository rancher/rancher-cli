package token

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// retirementBudget bounds every delete of one retirement pass together:
// the pass runs before the ExecCredential is written and kubectl supplies
// no deadline, so its cost must not scale with the record list. A var so
// tests shorten it.
var retirementBudget = 10 * time.Second

// RetirementReport is what one retirement pass did.
type RetirementReport struct {
	Retired  []TokenRef
	Failed   []TokenRef
	Kept     []config.SupersededToken
	Warnings []string
}

// expiryMargin is how long past its recorded expiry a token is treated as
// expired: an expired token authenticates nothing, and the margin absorbs
// a local clock that runs ahead of the server's.
const expiryMargin = time.Hour

// expiredPastMargin reports whether expiresAt passed more than the margin
// before now. An empty or unreadable expiry never expires: records written
// without one carry none.
func expiredPastMargin(expiresAt string, now time.Time) bool {
	if expiresAt == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, expiresAt)
	return err == nil && now.Sub(at) > expiryMargin
}

// RetireSuperseded retires the entry's superseded tokens the new credential
// owns: each same-owner record is revoked with cred as bearer, oldest
// first, under one total budget; a revoked or already-gone token drops its
// record, a failed delete keeps it stamped, and a record whose owner
// differs or is unknown is never attempted. A record expired past the
// margin is dropped without a request; one of another server than the
// entry's (serverURL) and a v3 record are kept without one. A pasted
// credential attempts and stamps nothing: its records wait for a login the
// CLI creates. The deletes run outside both locks; one config-lock hold
// then updates the records, and a failure there leaves them in place,
// redundant, never lost. Nothing here fails the caller: every leftover is
// a warning with its recovery sequence.
func RetireSuperseded(ctx context.Context, dir, entry, serverURL string, cred LoginCredential, records []config.SupersededToken) RetirementReport {
	var rep RetirementReport
	recs := slices.Clone(records)
	slices.SortStableFunc(recs, func(a, b config.SupersededToken) int { return strings.Compare(a.SupersededAt, b.SupersededAt) })
	if cred.Provenance == ProvenanceUserSupplied {
		for _, rec := range recs {
			rep.Kept = append(rep.Kept, rec)
			if advice := config.AdviseRecord(config.RecordSite{}, cmp.Or(rec.Server, serverURL), rec.UserID, rec.Ref); advice.Text != "" && rec.Ref.Kind != TokenKindV3 {
				// No login retires a token whose server cannot be used, so
				// the wait for a login is not said next to it.
				rep.Warnings = append(rep.Warnings, unusableServerKept(rec, advice.Text, ""))
				continue
			}
			rep.Warnings = append(rep.Warnings, keptWarning(rec, cmp.Or(rec.Server, serverURL), "it waits for a login the CLI creates, and this login's token was pasted"))
		}
		return rep
	}
	server := config.CanonicalServerIdentity(serverURL)
	budget, cancel := context.WithTimeout(context.WithoutCancel(ctx), retirementBudget)
	defer cancel()
	deadline, _ := budget.Deadline()

	stamps := map[TokenRef]clierr.RevokeOutcome{}
	var expired []TokenRef
	now := time.Now()
	for _, rec := range recs {
		switch {
		case expiredPastMargin(rec.ExpiresAt, now):
			expired = append(expired, rec.Ref)
			continue
		case rec.Server != "" && config.CanonicalServerIdentity(rec.Server) != server:
			rep.Kept = append(rep.Kept, rec)
			rep.Warnings = append(rep.Warnings, keptWarning(rec, rec.Server, "it is a token of "+config.DisplayServerURL(rec.Server)+", which this entry no longer names"))
			continue
		case rec.Ref.Kind == TokenKindV3:
			rep.Kept = append(rep.Kept, rec)
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("the superseded token %s was not retired: %s", rec.Ref.String(), v3Remedy(rec.Ref, rec.ExpiresAt)))
			continue
		}
		switch config.RecordOwnerState(rec, cred.UserID) {
		case config.OwnerDifferent:
			rep.Kept = append(rep.Kept, rec)
			who := "a different user"
			if rec.UserID != "" {
				who += " (" + clierr.IDText(rec.UserID) + ")"
			}
			rep.Warnings = append(rep.Warnings, keptWarning(rec, serverURL, who+" owns it"))
			continue
		case config.OwnerUnknown:
			rep.Kept = append(rep.Kept, rec)
			rep.Warnings = append(rep.Warnings, keptWarning(rec, serverURL, "its owner is not known"))
			continue
		}
		// The clock decides, not the budget's Done channel: a delete bound to
		// the same deadline can return before the budget's own timer fires,
		// and a passed deadline handed to RevokeCreated would be ignored
		// there and cost a full cleanup bound.
		if budget.Err() != nil || !time.Now().Before(deadline) {
			rep.Kept = append(rep.Kept, rec)
			rep.Warnings = append(rep.Warnings, keptWarning(rec, serverURL, "not attempted: the retirement budget was exhausted"))
			continue
		}
		out := cred.RevokeCreated(budget, rec.Ref)
		switch out.Result {
		case ResultRevoked, ResultAlreadyGone:
			rep.Retired = append(rep.Retired, rec.Ref)
		default:
			rep.Failed = append(rep.Failed, rec.Ref)
			stamps[rec.Ref] = clierr.RevokeOutcome{Result: out.Result, Error: out.Error}
			rep.Warnings = append(rep.Warnings, keptWarning(rec, serverURL, "it could not be revoked ("+out.Error+")"))
		}
	}
	if len(rep.Retired) == 0 && len(stamps) == 0 && len(expired) == 0 {
		return rep
	}
	if err := config.UpdateServerEntry(ctx, dir, entry, func(s *config.Server) bool {
		changed := false
		for i := range s.SupersededTokens {
			if st, ok := stamps[s.SupersededTokens[i].Ref]; ok {
				s.SupersededTokens[i].LastAttempt = &st
				changed = true
			}
		}
		kept := slices.DeleteFunc(s.SupersededTokens, func(r config.SupersededToken) bool {
			return slices.Contains(rep.Retired, r.Ref) || slices.Contains(expired, r.Ref)
		})
		changed = changed || len(kept) != len(s.SupersededTokens)
		s.SupersededTokens = kept
		if len(kept) == 0 {
			s.SupersededTokens = nil
		}
		return changed
	}); err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("could not update the superseded-token records of %s (%v); they are listed by: ", entry, err)+config.StatusCommand)
	}
	return rep
}

// v3Remedy says how a v3 token the CLI cannot delete ends: an
// administrator's delete, or its expiry.
func v3Remedy(ref TokenRef, expiresAt string) string {
	return "the CLI cannot delete this v3 token; " + v3AdminDelete("it", ref, expiresAt)
}

// v3AdminRemedy says how a v3 token ends when the CLI could not delete it:
// an administrator's delete, or its expiry.
func v3AdminRemedy(ref TokenRef, expiresAt string) string {
	return v3AdminDelete(ref.String(), ref, expiresAt)
}

// v3AdminDelete renders the administrator's delete of ref, naming it as
// subject, and the token's expiry when known.
func v3AdminDelete(subject string, ref TokenRef, expiresAt string) string {
	_, revoke := clierr.RecoveryCommands(ref)
	expiry := " expires"
	if expiresAt != "" {
		expiry += " at " + expiresAt
	}
	return subject + expiry + "; an administrator can delete it sooner with: " + revoke
}

// keptWarning names one unretired token, why, and what revokes it: the
// sequence that logs in as its owner (its owner or an administrator when the
// owner is not known), or for a v3 token the administrator's delete.
func keptWarning(rec config.SupersededToken, serverURL, why string) string {
	if rec.Ref.Kind == TokenKindV3 {
		// No login of the owner deletes a v3 token for a standard user.
		return fmt.Sprintf("the superseded token %s was not retired: %s; %s", rec.Ref.String(), why, v3AdminRemedy(rec.Ref, rec.ExpiresAt))
	}
	advice := config.AdviseRecord(config.RecordSite{}, serverURL, rec.UserID, rec.Ref)
	if advice.Text != "" {
		return unusableServerKept(rec, advice.Text, why)
	}
	revoke := "revoke it as " + config.RecordOwnerOrAdministrator + " with: "
	if advice.LoginAs != "" {
		revoke = "revoke it as user " + clierr.ShellQuote(clierr.IDText(advice.LoginAs)) + " with: "
	}
	return fmt.Sprintf("the superseded token %s was not retired: %s; the sequence %s; %s%s",
		rec.Ref.String(), why, clierr.RecoverySequenceAssumption, revoke, strings.Join(advice.Sequence, "; "))
}

// unusableServerKept is the warning for a superseded token whose server
// cannot be used: why it was kept (when given), then text, what becomes of
// the token.
func unusableServerKept(rec config.SupersededToken, text, why string) string {
	if why != "" {
		why += "; "
	}
	return fmt.Sprintf("the superseded token %s was not retired: %s%s", rec.Ref.String(), why, text)
}
