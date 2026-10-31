package configcmd

import (
	"cmp"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
)

// tokenRecovery is the commands that revoke a token: the administrator's
// delete for a v3 token, which no login of the entry's user can delete,
// else the sequence that logs in as the token's owner and deletes it. A
// server that cannot go into a command gives an empty list. Each command is
// a remedy command (clierr.MarkRemedy), so the json and yaml output name the
// invocation's config directory in it. entry is the stored entry the token
// belongs to and entryURL its current URL; serverURL is the server the token
// is recorded on.
func tokenRecovery(entry, entryURL, serverURL, userID string, ref clierr.TokenRef) []string {
	if ref.Kind == clierr.TokenKindV3 {
		_, revoke := clierr.RecoveryCommands(ref)
		return []string{clierr.MarkRemedy(revoke)}
	}
	if advice := config.AdviseRecord(config.RecordSite{Entry: entry, EntryURL: entryURL}, serverURL, userID, ref); advice.Text == "" {
		commands := make([]string, len(advice.Sequence))
		for i, command := range advice.Sequence {
			commands[i] = clierr.MarkRemedy(command)
		}
		return commands
	}
	return []string{}
}

// administratorLine is the text that replaces the recovery sequence and
// its assumption for a v3 token.
func administratorLine(serverURL string, ref clierr.TokenRef) string {
	_, revoke := clierr.RecoveryCommands(ref)
	return "an administrator logged in to " + config.DisplayServerURL(serverURL) + " can delete it with: " + revoke
}

// tokenReport projects one named token (the entry's current token or a
// superseded record) with its owner, expiry, and recovery commands.
func tokenReport(entry, entryURL, serverURL string, ref clierr.TokenRef, userID, expiresAt string) map[string]any {
	m := map[string]any{
		"ref":      map[string]any{"kind": string(ref.Kind), "name": ref.Name},
		"recovery": tokenRecovery(entry, entryURL, serverURL, userID, ref),
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
func recordReport(entry string, s config.Server, rec config.SupersededToken) map[string]any {
	m := tokenReport(entry, s.URL, cmp.Or(rec.Server, s.URL), rec.Ref, rec.UserID, rec.ExpiresAt)
	m["supersededAt"] = rec.SupersededAt
	m["state"] = recordState(s, rec)
	if rec.Server != "" {
		m["server"] = config.DisplayServerURL(rec.Server)
	}
	if rec.LastAttempt != nil {
		m["lastAttempt"] = map[string]any{"result": string(rec.LastAttempt.Result), "error": rec.LastAttempt.Error}
	}
	return m
}

// entryReport projects one server entry's CLI-owned tokens: provenance,
// the current token (or its logged-out state), and every record.
func entryReport(name string, s config.Server) map[string]any {
	m := map[string]any{"server": name, "url": config.DisplayServerURL(s.URL)}
	if s.Provenance != "" {
		m["provenance"] = string(s.Provenance)
	}
	if s.TokenUserID != "" {
		m["tokenUserId"] = s.TokenUserID
	}
	if s.TokenAuthConfig != "" {
		m["tokenAuthConfig"] = s.TokenAuthConfig
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
		m["currentToken"] = tokenReport(name, s.URL, s.URL, *s.TokenRef, s.TokenUserID, s.CredentialExpiresAt)
	case s.CLICreated() && s.LoggedOutAt == "":
		// The CLI created the token but never recorded its name: nothing
		// can be revoked by name, and the surface says so rather than
		// hiding the entry.
		m["currentToken"] = map[string]any{"note": "cannot revoke by name: no tokenRef recorded"}
	}
	recs := make([]map[string]any, 0, len(s.SupersededTokens))
	for _, r := range s.SupersededTokens {
		recs = append(recs, recordReport(name, s, r))
	}
	if len(recs) > 0 {
		m["supersededTokens"] = recs
	}
	return m
}

// recordLines renders an entry's records as indented text lines: one
// header per record, then state, last attempt, and the recovery sequence
// with its assumption, or for a v3 token the administrator's delete. A server
// URL that cannot be used gives the repair in words instead of the sequence.
func recordLines(entry string, s config.Server, indent string) []string {
	var lines []string
	for _, r := range s.SupersededTokens {
		owner := r.UserID
		if owner == "" {
			owner = "unknown owner"
		}
		lines = append(lines, fmt.Sprintf("%ssuperseded token %s (%s, superseded %s)", indent, output.Printable(r.Ref.String()), output.Printable(owner), r.SupersededAt))
		lines = append(lines, indent+"  state: "+recordState(s, r))
		if r.LastAttempt != nil {
			lines = append(lines, indent+"  last attempt: "+string(r.LastAttempt.Result)+" ("+output.Printable(r.LastAttempt.Error)+")")
		}
		ref := r.Ref
		ref.Name = output.Printable(ref.Name)
		userID := output.Printable(r.UserID)
		if r.Ref.Kind == clierr.TokenKindV3 {
			lines = append(lines, indent+"  "+administratorLine(cmp.Or(r.Server, s.URL), ref))
			continue
		}
		advice := config.AdviseRecord(config.RecordSite{Entry: entry, EntryURL: s.URL}, cmp.Or(r.Server, s.URL), userID, ref)
		if advice.Text != "" {
			lines = append(lines, indent+"  "+advice.Text)
			continue
		}
		lines = append(lines, sequenceLines(indent+"  ", advice.Sequence)...)
	}
	return lines
}

// recordState is a record's display-time state: otherServer when it names
// a server the entry no longer does (no login of the entry retires it),
// else how its owner compares with the entry's.
func recordState(s config.Server, rec config.SupersededToken) string {
	if rec.Server != "" && config.CanonicalServerIdentity(rec.Server) != config.CanonicalServerIdentity(s.URL) {
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
