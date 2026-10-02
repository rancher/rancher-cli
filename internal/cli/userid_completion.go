package cli

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/token"
)

// userIDFlag is auth get-token's flag naming the Rancher user the token
// must belong to.
const userIDFlag = "user-id"

// userIDSite reports a --user-id flag-value site on a command whose
// descriptor carries the flag. The site is owned here, so neither the live
// layer nor the shell's file completion is consulted for it.
func userIDSite(site CompletionSite, d runtime.Descriptor) bool {
	return site.Kind == SiteFlagValue && site.FlagName == userIDFlag &&
		slices.ContainsFunc(d.Flags, func(f runtime.ArgSpec) bool { return f.Name == userIDFlag })
}

// userIDCandidates completes --user-id from config.yaml alone: the
// recorded owner of the entry the typed --name identifies, when it begins
// with the word; else nothing. No store read, no request.
func userIDCandidates(site CompletionSite, tail []string) []string {
	owner := token.RecordedUserID(storedConfigSnapshot(), completion.TailFlag(tail, "server"),
		completion.TailFlag(tail, "name"), completion.TailFlag(tail, "cluster"))
	if owner == "" || !strings.HasPrefix(owner, site.Partial) {
		return nil
	}
	if site.EqualsForm {
		return []string{site.FlagSpelling + "=" + owner}
	}
	return []string{owner}
}
