// Package extres holds the fixed mutating commands for Rancher's aggregated
// ext.cattle.io/v1 API: create token (server-derived tokens) and create
// kubeconfig (server-minted kubeconfigs).
package extres

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// leadingDaysRe matches an optional leading "<integer>d" days segment,
// capturing the digits and whatever plain Go duration text follows.
var leadingDaysRe = regexp.MustCompile(`^(\d+)d(.*)$`)

// ParseTTL parses a --ttl flag: Go duration syntax (90m, 12h), optionally
// prefixed by a leading "<integer>d" days segment (1d = 24h) that may itself
// be followed by a plain Go duration ("1d12h"). At most one days segment is
// accepted, and only as the leading token — "12h1d" (not leading), "1d2d"
// (a second days segment), "1.5d" (fractional days), and a bare number with
// no unit ("90") are all VALIDATION, since none of them satisfy either
// branch below. Empty string means "no TTL" (server default). A negative
// result, from either branch, is VALIDATION — never-expires is not exposed
// by this flag.
func ParseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if m := leadingDaysRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, invalidTTL(s)
		}
		d = time.Duration(days) * 24 * time.Hour
		if rest := m[2]; rest != "" {
			extra, err := time.ParseDuration(rest)
			if err != nil {
				return 0, invalidTTL(s)
			}
			d += extra
		}
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, invalidTTL(s)
		}
	}
	if d < 0 {
		return 0, invalidTTL(s)
	}
	return d, nil
}

func invalidTTL(s string) error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf("invalid --ttl %q", s)).
		WithHint("use a Go duration (90m, 12h), optionally prefixed by <n>d (90d, 1d12h)")
}
