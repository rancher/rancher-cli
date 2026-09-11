package token

import (
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

// RetireSuperseded retires the entry's superseded tokens the new credential
// owns: each same-owner record is revoked with cred as bearer, oldest
// first, under one total budget; a revoked or already-gone token drops its
// record, a failed delete keeps it stamped, and a record whose owner
// differs or is unknown is never attempted. The deletes run outside both
// locks; one config-lock hold then updates the records, and a failure
// there leaves them in place, redundant, never lost. Nothing here fails
// the caller: every leftover is a warning with its recovery sequence.
func RetireSuperseded(ctx context.Context, dir, entry, serverURL string, cred LoginCredential, records []config.SupersededToken) RetirementReport {
	var rep RetirementReport
	recs := slices.Clone(records)
	slices.SortStableFunc(recs, func(a, b config.SupersededToken) int { return strings.Compare(a.SupersededAt, b.SupersededAt) })
	budget, cancel := context.WithTimeout(context.WithoutCancel(ctx), retirementBudget)
	defer cancel()
	deadline, _ := budget.Deadline()

	stamps := map[TokenRef]clierr.RevokeOutcome{}
	for _, rec := range recs {
		switch config.RecordOwnerState(rec, cred.UserID) {
		case config.OwnerDifferent:
			rep.Kept = append(rep.Kept, rec)
			rep.Warnings = append(rep.Warnings, keptWarning(rec, serverURL, "a different user ("+rec.UserID+") owns it"))
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
	if len(rep.Retired) == 0 && len(stamps) == 0 {
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
			return slices.Contains(rep.Retired, r.Ref)
		})
		changed = changed || len(kept) != len(s.SupersededTokens)
		s.SupersededTokens = kept
		if len(kept) == 0 {
			s.SupersededTokens = nil
		}
		return changed
	}); err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("could not update the superseded-token records of %s (%v); they are listed by: rancher config status", entry, err))
	}
	return rep
}

// keptWarning names one unretired token, why, and the sequence that
// revokes it.
func keptWarning(rec config.SupersededToken, serverURL, why string) string {
	seq := clierr.RecoverySequence(serverURL, rec.UserID, rec.Ref)
	return fmt.Sprintf("the superseded token %s was not retired: %s; revoke it with: %s (%s)",
		rec.Ref.String(), why, strings.Join(seq, "; "), clierr.RecoverySequenceAssumption)
}
