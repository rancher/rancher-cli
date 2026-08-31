package oidcclient

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/extres"
)

// maxTTLSeconds is the largest whole-second value whose conversion back to
// a time.Duration (seconds * time.Second) cannot overflow int64 — the true
// server/time.Duration bound (math.MaxInt64/int64(time.Second)), not an
// arbitrary product limit. Both OIDCClientSpec TTL fields are wire int64
// seconds, so this is also the largest value the server itself could
// possibly accept without its own overflow.
const maxTTLSeconds = int64(math.MaxInt64) / int64(time.Second)

// strictTTLSeconds converts a --token-ttl/--refresh-ttl flag to the
// CRD-required whole-second wire value. raw == "" means the flag was
// omitted: the caller applies its OWN default (3600/86400) — the CLI's
// default, never the server's, since both OIDCClientSpec TTL fields are
// CRD-required with no server-side defaulting. A supplied value must be a
// positive whole number of seconds: any sub-second/fractional remainder,
// zero, or negative value is VALIDATION, never silently truncated or
// reinterpreted as omission — the existing TTLSeconds/TTLMilliseconds
// zero-is-omission sentinel precedent is deliberately not reused here for
// exactly that reason.
func strictTTLSeconds(flag, raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	ttl, err := extres.ParseTTL(raw)
	if err != nil {
		return 0, rewrapTTLFlagName(err, flag)
	}
	if ttl <= 0 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --%s %q: must be a positive duration; e.g. 1h or 3600s", flag, raw))
	}
	if ttl%time.Second != 0 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --%s %q: must be a whole number of seconds with no fractional remainder; e.g. 1h or 3600s", flag, raw))
	}
	secs := int64(ttl / time.Second)
	if secs > maxTTLSeconds {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --%s %q: must be at most %ds", flag, raw, maxTTLSeconds))
	}
	return secs, nil
}

// rewrapTTLFlagName corrects extres.ParseTTL's error, which hardcodes
// "--ttl" (create token/create kubeconfig's actual flag name) in its
// Message and Hint, into the flag this command really has (--token-ttl or
// --refresh-ttl) — ParseTTL cannot name a flag it does not know about, so a
// caller with a differently-named flag must correct the wording before it
// reaches the user. WithMessage preserves everything else (code, cause,
// retryable, candidates, details), so only Hint needs a second fix-up.
func rewrapTTLFlagName(err error, flag string) error {
	ce, ok := err.(*clierr.Error)
	if !ok {
		return err
	}
	fixed := "--" + flag
	return ce.WithMessage(strings.ReplaceAll(ce.Message, "--ttl", fixed)).
		WithHint(strings.ReplaceAll(ce.Hint, "--ttl", fixed))
}
