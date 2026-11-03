package token

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// noTokenPermissionText is the text every entry gives for a 403 on creating a
// token for Rancher itself: the user has no role that allows it, and what an
// administrator runs to grant one. A known user id names the user in the
// command; without one the text says how to find the id and writes
// <user-id> in the command. ext says the refused create was of an ext token,
// whose resource the text names; a Norman create names none. It carries no
// hint: an administrator, not the caller, runs the command.
func noTokenPermissionText(userID string, ext bool) string {
	if userID != "" {
		return tokenPermissionLead(ext) + "an administrator can grant the global role user: add --apply --yes after reading the preview of: " +
			clierr.Rancher + " grant" + clierr.ShellQuoteArgs(" --global --role user --dry-run", userID)
	}
	return tokenPermissionLead(ext) + "find the user's id with: " + clierr.Rancher +
		" get users; an administrator can grant the global role user: add --apply --yes after reading the preview of: " + clierr.Rancher +
		" grant <user-id> --global --role user --dry-run"
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
