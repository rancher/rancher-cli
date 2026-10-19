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
// recorded owner of the entry the typed --name identifies, or else the
// distinct recorded owners of the entries for the typed --server (the
// current server's URL without one), those that begin with the word. No
// store read, no request.
func userIDCandidates(site CompletionSite, tail []string) []string {
	var out []string
	for _, owner := range token.RecordedUserIDs(storedConfigSnapshot(), completion.TailFlag(tail, "server"),
		completion.TailFlag(tail, "name"), completion.TailFlag(tail, "cluster")) {
		if !strings.HasPrefix(owner, site.Partial) {
			continue
		}
		if site.EqualsForm {
			owner = site.FlagSpelling + "=" + owner
		}
		out = append(out, owner)
	}
	return out
}
