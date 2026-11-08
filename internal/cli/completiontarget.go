package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// completionTargeter is the optional interface of a command whose live
// completion source needs no credential. It tells the surface where the
// command itself would connect for the line being completed: the server URL
// and the CA bundle as PEM (empty for the system roots). It reads only the
// config it is given, the environment and the CA file, never the credential
// store, and ok is false when the command would not connect or the line
// names no server. It is asserted on the command's completion.Completer.
type completionTargeter interface {
	CompletionTarget(cfg *config.Config, req completion.Request) (server, caPEM string, ok bool)
}

// noCredentialCandidates serves a site whose source needs no credential:
// the command's CompletionTarget names the server and CA, the surface
// checks the server URL as resolveCompletionToken does, and the source's
// requests carry no token. A command with no CompletionTarget, no config
// snapshot, a refused target or a failed client gives no suggestions.
func noCredentialCandidates(ctx context.Context, site CompletionSite, lc completion.Completer, req completion.Request, src completion.Source, limit int) []string {
	targeter, ok := lc.(completionTargeter)
	if !ok {
		completionDebugf("stop: the command has no completion target")
		return nil
	}
	cfg := storedConfigSnapshot()
	if cfg == nil {
		completionDebugf("stop: no config loaded")
		return nil
	}
	server, caPEM, ok := targeter.CompletionTarget(cfg, req)
	if !ok {
		completionDebugf("stop: the command names no server to connect to")
		return nil
	}
	canonical, ok := completionServerURL(server)
	if !ok {
		return nil
	}
	completionDebugf("connection: url=%s ca=%t, no credential", canonical, caPEM != "")
	hc, err := newCompletionClient(caPEM)
	if err != nil {
		completionDebugf("stop: HTTP client: %v", err)
		return nil
	}
	conn := tracedConnection(hc, TokenCapability{ServerURL: canonical}, limit)
	res := src.Complete(ctx, conn, req)
	if conn.Exceeded() {
		completionDebugf("stop: source exceeded its allowance of %d requests; result dropped", limit)
		return nil
	}
	completionDebugf("source: %d requests, %d names, %d descriptions", conn.Used(), len(res.Names), len(res.Descriptions))
	return emitResult(site, res)
}

// validateCompletionTargets rejects a command whose source needs no
// credential on one of the command's own sites and that has no
// CompletionTarget: the surface would offer nothing for it. leaves are the
// adapted commands' descriptors, completers the source of each command that
// implements completion.Completer, and targets the commands that implement
// completionTargeter.
func validateCompletionTargets(leaves map[string]runtime.Descriptor, completers map[string]completion.Source, targets map[string]bool) error {
	for _, name := range slices.Sorted(maps.Keys(completers)) {
		src := completers[name]
		if src.Claim == nil || targets[name] || !claimsWithoutCredential(leaves[name], src) {
			continue
		}
		return fmt.Errorf("command %s has a completion source that needs no credential, but the command has no completion target (the server to connect to)", name)
	}
	return nil
}

// claimsWithoutCredential reports whether src claims any flag value or
// positional of d with the no-credential need.
func claimsWithoutCredential(d runtime.Descriptor, src completion.Source) bool {
	sites := make([]completion.Request, 0, len(d.Flags)+len(d.Args))
	for _, f := range d.Flags {
		sites = append(sites, completion.Request{Command: d.Name, Flag: f.Name})
	}
	for i := range d.Args {
		sites = append(sites, completion.Request{Command: d.Name, Index: i, Positionals: slices.Repeat([]string{"x"}, i)})
	}
	return slices.ContainsFunc(sites, func(req completion.Request) bool {
		reqs, ok := src.Claim(req)
		return ok && reqs.Credential == completion.CredentialNone
	})
}
