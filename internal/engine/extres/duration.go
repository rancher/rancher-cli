// Package extres holds the fixed mutating commands for Rancher's aggregated
// ext.cattle.io/v1 API: create token (server-derived tokens) and create
// kubeconfig (server-generated kubeconfigs).
package extres

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// leadingDaysRe matches an optional leading "<integer>d" days segment,
// capturing the digits and whatever plain Go duration text follows.
var leadingDaysRe = regexp.MustCompile(`^(\d+)d(.*)$`)

// maxDays bounds the day count before it is multiplied into a time.Duration
// (int64 nanoseconds): unbounded, a large-enough day count wraps the int64
// multiplication and silently produces a small positive duration instead of
// failing - e.g. 213504d wraps to roughly 25 minutes. maxDays is the largest
// day count whose *24h product cannot overflow.
const maxDays = int64(math.MaxInt64) / int64(24*time.Hour)

// ParseTTL parses the value of the --<flag> flag (pass flag without
// dashes; its errors name --<flag>): duration syntax such as 90m or 12h,
// optionally prefixed by a leading "<integer>d" days segment (1d = 24h)
// that may itself be followed by a plain Go duration ("1d12h"). At most
// one days segment is accepted, and only as the leading token - "12h1d"
// (not leading), "1d2d" (a second days segment), "1.5d" (fractional
// days), and a bare number with no unit ("90") are all VALIDATION, since
// none of them satisfy either branch below. Empty string means "no TTL" (server default). A negative
// result, from either branch, is VALIDATION - never-expires is not exposed
// by this flag.
func ParseTTL(flag, s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if m := leadingDaysRe.FindStringSubmatch(s); m != nil {
		days, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, invalidTTL(flag, s)
		}
		if days > maxDays {
			return 0, clierr.New(clierr.CodeValidation,
				fmt.Sprintf("invalid --%s %q: days must be at most %d (about 292 years)", flag, s, maxDays))
		}
		d = time.Duration(days) * 24 * time.Hour
		if rest := m[2]; rest != "" {
			extra, err := time.ParseDuration(rest)
			if err != nil {
				return 0, invalidTTL(flag, s)
			}
			d += extra
		}
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, invalidTTL(flag, s)
		}
	}
	if d < 0 {
		return 0, invalidTTL(flag, s)
	}
	return d, nil
}

func invalidTTL(flag, s string) error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("invalid --%s %q: use a duration such as 90m or 12h; it can start with a number of days, as in 90d or 1d12h", flag, s))
}

// TTLMilliseconds converts a parsed, positive TTL to the milliseconds the
// Token spec expects, rejecting a sub-millisecond, nonzero duration that
// would otherwise silently become spec.ttl: 0 - the server's own
// "use the default" sentinel, not what the caller asked for. ttl <= 0
// (absent --ttl) returns 0, nil so callers can keep their existing
// `if ttl > 0` idiom to omit the field entirely.
func TTLMilliseconds(flag string, ttl time.Duration, raw string) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	ms := ttl.Milliseconds()
	if ms == 0 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --%s %q: too small to represent in milliseconds; pass at least 1ms", flag, raw))
	}
	return ms, nil
}

// TTLSeconds is TTLMilliseconds' counterpart for the Kubeconfig spec, whose
// ttl field is SECONDS, not milliseconds.
func TTLSeconds(flag string, ttl time.Duration, raw string) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	secs := int64(ttl.Seconds())
	if secs == 0 {
		return 0, clierr.New(clierr.CodeValidation,
			fmt.Sprintf("invalid --%s %q: the duration is shorter than one second; pass at least 1s", flag, raw))
	}
	return secs, nil
}
