package token

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// noTokenPermissionText is the text every entry gives for a 403 on creating a
// token for Rancher itself: the user has no role that allows it, and what an
// administrator runs to grant one. A Rancher user id names the user in the
// command; any other id is shown through clierr.IDText and the command keeps
// <user-id>; without an id the text says how to find the id and writes
// <user-id> in the command. ext says the refused create was of an ext token,
// whose resource the text names; a Norman create names none. It carries no
// hint: an administrator, not the caller, runs the command.
func noTokenPermissionText(userID string, ext bool) string {
	return noTokenPermissionTextOf(userID, "", ext)
}

// noTokenPermissionTextOf is noTokenPermissionText for an id that may be
// recorded on a server entry. The grant command takes the id only when it is
// a Rancher user id; any other id (a recorded or server-named value the CLI
// did not check) is shown through clierr.IDText and the command keeps
// <user-id>, so a command never names a user other than the one meant. entry
// is the server entry whose recorded owner config status reports exactly as
// recorded, "" when it reports none; the text then points at that field.
func noTokenPermissionTextOf(userID, entry string, ext bool) string {
	lead := tokenPermissionLead(ext)
	if userID != "" && ValidUserID(userID) {
		grant := clierr.NewCommand("grant").Arg(userID).FlagText(" --global --role user --dry-run")
		return clierr.Text(lead+"an administrator can grant the global role user", clierr.Lead(": add --apply --yes after reading the preview of: "), grant)
	}
	users := clierr.NewCommand("get users")
	grant := clierr.NewCommand("grant").Placeholder("<user-id>").FlagText(" --global --role user --dry-run")
	if userID != "" {
		lead += "the user id " + clierr.ShellQuote(clierr.IDText(userID)) + " is not a Rancher user id, and the CLI prints no command for it; "
		if entry != "" {
			status := clierr.NewCommand("config status").FlagText(" -o json")
			return clierr.Text(lead+"the exact id is the tokenUserId of server entry "+clierr.ShellQuote(entry), clierr.Lead(" in: "), status,
				"; find the user's id", clierr.Lead(" with: "), users,
				"; an administrator can grant the global role user", clierr.Lead(": add --apply --yes after reading the preview of: "), grant)
		}
	}
	return clierr.Text(lead+"find the user's id", clierr.Lead(" with: "), users,
		"; an administrator can grant the global role user", clierr.Lead(": add --apply --yes after reading the preview of: "), grant)
}

// configStatusEntry is the name of the entry config status lists with its
// recorded owner, "" for a stored login that status leaves out (one the CLI
// holds no token of, such as a pasted token).
func configStatusEntry(sc storedCredential) string {
	if sc.entry.HoldsCLITokens() {
		return sc.name
	}
	return ""
}

// tokenPermissionLead opens the permission text: the missing role, and for an
// ext token create the resource the role must allow.
func tokenPermissionLead(ext bool) string {
	if ext {
		return "the user has no role that allows creating tokens (create on tokens.ext.cattle.io); "
	}
	return "the user has no role that allows creating tokens; "
}

// withTokenPermission puts noTokenPermissionText for an ext token create ahead of recovery, the
// recovery of the failure, and adds how to log in again unless the recovery
// already says it.
func withTokenPermission(userID, recovery, server string) string {
	login, _ := reLogin(server)
	text := noTokenPermissionText(userID, true) + "; " + recovery
	if !strings.Contains(recovery, login) {
		text += "; " + login
	}
	return text
}
