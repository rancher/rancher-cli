package cli

import (
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/config"
)

// completionCharset is the allowlist for candidate values (AC-21).
// First character must not be a hyphen (would parse as a flag).
var completionCharset = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// sanitizeCandidates drops values that do not conform to the shell-safe
// charset and returns the survivors lexicographically sorted (AC-21, AC-26).
func sanitizeCandidates(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if completionCharset.MatchString(c) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// resolveStaticEnum returns enum candidates for a flag-value site, sorted.
// Called for both space-form (bare values) and equals-form (full tokens).
// Static enums are compiled-in literals, not server data, so the AC-21
// shell-safe charset does not gate them - "template={{.}}" must survive.
// Live candidates keep sanitizeCandidates at their own call sites.
func resolveStaticEnum(enum []string, site CompletionSite) []string {
	sorted := append([]string{}, enum...)
	sort.Strings(sorted)
	if site.EqualsForm {
		out := make([]string, 0, len(sorted))
		for _, v := range sorted {
			out = append(out, site.FlagSpelling+"="+v)
		}
		return out
	}
	return sorted
}

// completionTimeout returns the effective completion timeout from the stored
// config, delegating to config.EffectiveCompletionTimeout for default/floor/cap
// semantics (AC-2, finding 6). Hand-edited values outside [1ms, 2s] are clamped
// here at use rather than at write time.
func completionTimeout() time.Duration {
	return config.EffectiveCompletionTimeout(storedConfigSnapshot())
}

// completionNetworkEnabled reports whether live completion is configured
// (AC-1).
func completionNetworkEnabled() bool {
	cfg := storedConfigSnapshot()
	return cfg != nil && cfg.Completion.Network
}

// newCompletionClient builds an http.Client for completion requests. It
// is constructed directly from httpx, never through resolveDeps (AC-20/AC-22).
// Called exactly once per live dispatch (finding 9).
func newCompletionClient(caPEM string) (*http.Client, error) {
	return httpx.New(httpx.Options{CACert: caPEM, Timeout: completionTimeout()})
}

// dedupe removes duplicates from a sorted slice (input must be sorted).
func dedupe(s []string) []string {
	if len(s) == 0 {
		return s
	}
	// A fresh slice: appending into a shared backing array would silently
	// mutate the caller's input.
	out := []string{s[0]}
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
