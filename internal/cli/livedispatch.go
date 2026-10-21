package cli

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// liveSourceCandidates serves a live site through the completion source
// that claims it, in dispatch order: the command's own source, then the
// shared flag table for a flag site, then the positional tables for a
// first positional. It reports owned=false when no source claims the site,
// and the caller answers nothing. For a claimed site the policy is
// all here: the allowance class is validated and mapped to a request count,
// the credential and cluster needs are satisfied (a source that needs no
// credential gets its server and CA from its command, see
// noCredentialCandidates; a typed display name is
// resolved with one request counted apart), the Connection is built under
// the TAB deadline, and the Result is dropped whole when the source asked
// for more than its allowance. Every failure answers nothing.
func liveSourceCandidates(ctx context.Context, site CompletionSite, d runtime.Descriptor, lc completion.Completer, tail []string,
	flags map[string]completion.Source, kindTable map[string]completion.Source, positional map[string]string) ([]string, bool) {
	req := completion.Request{
		Flag:        site.FlagName,
		Index:       site.Index,
		Partial:     site.Partial,
		Cluster:     completion.TailFlag(tail, "cluster"),
		Project:     completion.TailFlag(tail, "project"),
		Tail:        tail,
		Positionals: typedPositionals(site, tail, d.Flags),
	}
	src, reqs, ok := claimLiveSource(req, d, lc, flags, kindTable, positional)
	if !ok {
		return nil, false
	}
	limit, ok := allowanceLimit(reqs.Allowance)
	if !ok {
		completionDebugf("stop: source claimed allowance %d, outside the class", reqs.Allowance)
		return nil, true
	}
	if !clusterNeedTyped(reqs, req) {
		completionDebugf("stop: source needs a cluster and --cluster is absent")
		return nil, true
	}

	ctx, cancel := completionDeadline(ctx)
	defer cancel()

	if reqs.Credential == completion.CredentialNone {
		return noCredentialCandidates(ctx, site, lc, req, src, limit), true
	}

	cap := resolveCompletionToken(tokenCluster(reqs, req))
	if cap.Token == "" {
		return nil, true
	}
	if !credentialNeedMet(reqs, cap) {
		completionDebugf("stop: source needs a management token and the token is exec-credential-only")
		return nil, true
	}
	if !clusterResolvable(reqs, req, cap) {
		completionDebugf("stop: --cluster %q is not an id and an exec-credential-only token cannot resolve a display name", req.Cluster)
		return nil, true
	}
	hc, err := newCompletionClient(cap.CACert)
	if err != nil {
		completionDebugf("stop: HTTP client: %v", err)
		return nil, true
	}
	if reqs.Cluster == completion.ClusterResolved {
		req.ClusterID = req.Cluster
		if !isClusterIDShaped(req.Cluster) {
			id := resolveDisplayNameToClusterID(ctx, hc, cap, req.Cluster)
			if id == "" {
				completionDebugf("stop: cluster display name %q did not resolve to one id", req.Cluster)
				return nil, true
			}
			req.ClusterID = id
		}
	}

	conn := tracedConnection(hc, cap, limit)
	res := src.Complete(ctx, conn, req)
	if conn.Exceeded() {
		completionDebugf("stop: source exceeded its allowance of %d requests; result dropped", limit)
		return nil, true
	}
	completionDebugf("source: %d requests, %d names, %d descriptions", conn.Used(), len(res.Names), len(res.Descriptions))
	return emitResult(site, res), true
}

// typedPositionals fills Request.Positionals: the tail's positionals with
// flag arity applied. The word being completed never reaches the tail.
func typedPositionals(site CompletionSite, tail []string, leafFlags []runtime.ArgSpec) []string {
	pos := positionals(tail, leafFlags)
	if site.DoubleDash {
		return append(pos, site.After...)
	}
	return pos
}

// tracedConnection builds the Connection a source (or the surface's own
// cluster resolution) reads through: the per-dispatch client, the
// resolved token, and limit requests, each one written to the
// completion trace with its server-relative path, status, size and
// latency.
func tracedConnection(hc *http.Client, cap TokenCapability, limit int) *completion.Connection {
	conn := completion.NewConnection(hc, cap.Token, cap.ServerURL, cap.IDOnly, limit)
	conn.SetProjectLister(listProjects)
	conn.SetClusterLister(listClusters)
	conn.SetTrace(func(path string, status, size int, elapsed time.Duration, err error) {
		if err != nil {
			completionDebugf("GET %s: %v (%s)", path, err, elapsed.Round(time.Millisecond))
			return
		}
		completionDebugf("GET %s: HTTP %d, %d bytes (%s)", path, status, size, elapsed.Round(time.Millisecond))
	})
	return conn
}

// claimLiveSource finds the source that owns a site: the command's own
// source first, then the shared flag table for a flag-value site (a
// Request with a Flag), then - for a first positional - the kind named by
// positionalKinds for a bespoke command or by the last word of a generated
// get, patch or delete, served by its kindSources entry or else by the
// generic kinds source. A source owns the site only when its Claim says
// so; the generic source declines a kind that does not complete. A
// --project value site is claimed only on a command that takes --project:
// every other command refuses the flag, and at the root or a group the
// command is not known yet.
func claimLiveSource(req completion.Request, d runtime.Descriptor, lc completion.Completer,
	flags map[string]completion.Source, kindTable map[string]completion.Source, positional map[string]string) (completion.Source, completion.Requirements, bool) {
	if req.Flag == runtime.FlagProject && !runtime.TakesProject(d) {
		completionDebugf("source: %s does not take --project", d.Name)
		return completion.Source{}, completion.Requirements{}, false
	}
	if lc != nil {
		if src := lc.LiveSource(); src.Claim != nil {
			if reqs, ok := src.Claim(req); ok {
				completionDebugf("source: claimed by command %s", d.Name)
				return src, reqs, true
			}
		}
	}
	switch {
	case req.Flag != "":
		if src, ok := flags[req.Flag]; ok {
			if reqs, ok := src.Claim(req); ok {
				completionDebugf("source: claimed by the shared --%s source", req.Flag)
				return src, reqs, true
			}
		}
	case req.Index == 0:
		key, ok := positional[d.Name]
		if !ok && isGeneratedVerb(d.Name) {
			if k, found := kindForDescriptor(d); found {
				key, ok = k.Key, true
			}
		}
		if !ok {
			return completion.Source{}, completion.Requirements{}, false
		}
		src, bespoke := kindTable[key]
		if !bespoke {
			src = kindscomplete.KindSource(key)
		}
		if reqs, ok := src.Claim(req); ok {
			completionDebugf("source: claimed by the %s kind source", key)
			return src, reqs, true
		}
	}
	return completion.Source{}, completion.Requirements{}, false
}

// tokenCluster names the cluster the TAB's token is resolved for. A
// source that needs a resolved cluster gets the typed one, so the
// exec-credential cache entry for that cluster can serve it. A source
// with no cluster need must not be suppressed or rescoped by an
// unrelated --cluster on the line: when it can work with any credential
// its listings go to the management cluster's own proxy, so the cache
// entry for that cluster is the one that can serve it; when it needs a
// management token no cache entry can, and no cluster is named.
func tokenCluster(reqs completion.Requirements, req completion.Request) string {
	switch {
	case reqs.Cluster == completion.ClusterResolved:
		return req.Cluster
	case reqs.Credential == completion.CredentialAny:
		return engine.LocalCluster
	}
	return ""
}

// clusterResolvable reports whether a resolved cluster need can be met
// with the resolved token: an exec-credential-only token cannot list the
// management clusters, so under it the typed --cluster must already be
// an id; a display name answers nothing rather than reaching a source as
// ClusterID.
func clusterResolvable(reqs completion.Requirements, req completion.Request, cap TokenCapability) bool {
	if reqs.Cluster != completion.ClusterResolved || !cap.IDOnly {
		return true
	}
	return isClusterIDShaped(req.Cluster)
}

// clusterNeedTyped reports whether the site's cluster need can be met from
// the line: a source that needs a resolved cluster must have --cluster
// typed, whatever its spelling - a display name may carry a space, and
// the shell-safe segment rule applies where the resolved id enters a path,
// in the seam's builders; a source with no cluster need is never held
// back by the flag.
func clusterNeedTyped(reqs completion.Requirements, req completion.Request) bool {
	return reqs.Cluster != completion.ClusterResolved || req.Cluster != ""
}

// credentialNeedMet reports whether the resolved token satisfies the
// source's credential need: a management listing is refused under an
// exec-credential-only token.
func credentialNeedMet(reqs completion.Requirements, cap TokenCapability) bool {
	return reqs.Credential != completion.CredentialManagement || !cap.IDOnly
}

// emitResult applies the surface's answer policy to a source's Result:
// the composite Prefix, when set, must be a compiled-in literal of
// shell-safe characters ending in exactly one "/" or the whole result is
// dropped; the names pass the shell-safe charset, are deduped and
// sorted; the prefix is put in front of every survivor, and an equals-form
// site gets full --flag=value tokens. A survivor's description is looked
// up by its bare name, sanitized, and written after the token as
// "token:description" (see formatCandidate); a name with no description
// left, or whose description is the name itself, is the bare token.
func emitResult(site CompletionSite, res completion.Result) []string {
	if res.Prefix != "" && !validCompositePrefix(res.Prefix) {
		completionDebugf("stop: source prefix %q is not a composite prefix; result dropped", res.Prefix)
		return nil
	}
	names := slices.Compact(sanitizeCandidates(res.Names))
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		tok := res.Prefix + n
		if site.EqualsForm {
			tok = site.FlagSpelling + "=" + tok
		}
		desc := completion.SanitizeDescription(res.Descriptions[n])
		if desc == n {
			desc = ""
		}
		out = append(out, formatCandidate(tok, desc))
	}
	return out
}

// validCompositePrefix reports whether p is a composite value prefix the
// surface recomposes: shell-safe characters followed by exactly one
// trailing "/".
func validCompositePrefix(p string) bool {
	body, ok := strings.CutSuffix(p, "/")
	return ok && body != "" && !strings.Contains(body, "/") && completionCharset.MatchString(body)
}

// completionTimeout returns the TAB budget from the stored config through
// config.EffectiveCompletionTimeout, which applies the default, the floor
// and the cap; a hand-edited value outside [1ms, 2s] is clamped here, at
// use, rather than at write time.
func completionTimeout() time.Duration {
	return config.EffectiveCompletionTimeout(storedConfigSnapshot())
}

// newCompletionClient builds the one http.Client of a live dispatch,
// straight from httpx and never through resolveDeps, bound
// by the TAB budget on its own so a request cannot outlive it whatever
// context it is given.
func newCompletionClient(caPEM string) (*http.Client, error) {
	return httpx.New(httpx.Options{CACert: caPEM, Timeout: completionTimeout()})
}
