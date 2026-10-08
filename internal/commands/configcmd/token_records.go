package configcmd

import (
	"cmp"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// tokenReport projects one named token (the entry's current token or a
// superseded record) with its owner, expiry, and recovery sequence.
func tokenReport(serverURL string, ref clierr.TokenRef, userID, expiresAt string) map[string]any {
	m := map[string]any{
		"ref":      map[string]any{"kind": string(ref.Kind), "name": ref.Name},
		"recovery": clierr.RecoverySequence(serverURL, userID, ref),
	}
	if userID != "" {
		m["userId"] = userID
	}
	if expiresAt != "" {
		m["expiresAt"] = expiresAt
	}
	return m
}

// recordReport projects one superseded record with its display-time state.
// A record that names its server is reported against that server, which
// may not be the entry's.
func recordReport(serverURL string, s config.Server, rec config.SupersededToken) map[string]any {
	m := tokenReport(cmp.Or(rec.Server, serverURL), rec.Ref, rec.UserID, rec.ExpiresAt)
	m["supersededAt"] = rec.SupersededAt
	m["state"] = recordState(s, rec)
	if rec.Server != "" {
		m["server"] = rec.Server
	}
	if rec.LastAttempt != nil {
		m["lastAttempt"] = map[string]any{"result": string(rec.LastAttempt.Result), "error": rec.LastAttempt.Error}
	}
	return m
}

// entryReport projects one server entry's CLI-owned tokens: provenance,
// the current token (or its logged-out state), and every record.
func entryReport(name string, s config.Server) map[string]any {
	m := map[string]any{"server": name, "url": s.URL}
	if s.Provenance != "" {
		m["provenance"] = string(s.Provenance)
	}
	if s.TokenUserID != "" {
		m["tokenUserId"] = s.TokenUserID
	}
	if s.CredentialExpiresAt != "" {
		m["credentialExpiresAt"] = s.CredentialExpiresAt
	}
	if s.LoggedOutAt != "" {
		m["loggedOutAt"] = s.LoggedOutAt
		m["credential"] = "no stored credential"
	}
	switch {
	case s.CLIOwned():
		m["currentToken"] = tokenReport(s.URL, *s.TokenRef, s.TokenUserID, s.CredentialExpiresAt)
	case s.CLICreated() && s.LoggedOutAt == "":
		// The CLI created the token but never recorded its name: nothing
		// can be revoked by name, and the surface says so rather than
		// hiding the entry.
		m["currentToken"] = map[string]any{"note": "cannot revoke by name: no tokenRef recorded"}
	}
	recs := make([]map[string]any, 0, len(s.SupersededTokens))
	for _, r := range s.SupersededTokens {
		recs = append(recs, recordReport(s.URL, s, r))
	}
	if len(recs) > 0 {
		m["supersededTokens"] = recs
	}
	return m
}

// recordLines renders an entry's records as indented text lines: one
// header per record, then state, last attempt, and the recovery sequence
// with its assumption.
func recordLines(s config.Server, indent string) []string {
	var lines []string
	for _, r := range s.SupersededTokens {
		owner := r.UserID
		if owner == "" {
			owner = "unknown owner"
		}
		lines = append(lines, fmt.Sprintf("%ssuperseded token %s (%s, superseded %s)", indent, r.Ref.String(), owner, r.SupersededAt))
		lines = append(lines, indent+"  state: "+recordState(s, r))
		if r.LastAttempt != nil {
			lines = append(lines, indent+"  last attempt: "+string(r.LastAttempt.Result)+" ("+r.LastAttempt.Error+")")
		}
		lines = append(lines, sequenceLines(indent+"  ", clierr.RecoverySequence(cmp.Or(r.Server, s.URL), r.UserID, r.Ref))...)
	}
	return lines
}

// recordState is a record's display-time state: otherServer when it names
// a server the entry no longer does (no login of the entry retires it),
// else how its owner compares with the entry's.
func recordState(s config.Server, rec config.SupersededToken) string {
	if rec.Server != "" && rec.Server != config.CanonicalServerIdentity(s.URL) {
		return "otherServer"
	}
	return string(config.RecordOwnerState(rec, s.TokenUserID))
}

// sequenceLines renders the recovery sequence, one command per line, then
// its assumption.
func sequenceLines(indent string, seq []string) []string {
	lines := []string{indent + "revoke it with:"}
	for _, c := range seq {
		lines = append(lines, indent+"  "+c)
	}
	return append(lines, indent+"  ("+clierr.RecoverySequenceAssumption+")")
}
