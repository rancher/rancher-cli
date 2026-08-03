package credhelper

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// helperNameRE is the portable helper-name grammar: the value is a PATH-lookup
// command suffix, never a path.
var helperNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidHelperName reports whether name satisfies the helper-name grammar.
func ValidHelperName(name string) bool { return helperNameRE.MatchString(name) }

// CanonicalServerURL reduces a server URL to its canonical scheme://host form:
// scheme and host lowercased, an explicit default port (443 for https, 80 for
// http) dropped, path/query/fragment/userinfo discarded. This single function
// defines server identity for the helper wire protocol, helper records,
// and the CLI's own same-server comparison.
func CanonicalServerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("server URL %q must be absolute (scheme://host)", raw)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("server URL %q has no hostname", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme + "://" + canonicalHostPort(scheme, u.Host), nil
}

// canonicalHostPort lowercases host and drops an explicit port matching the
// scheme's default. net.SplitHostPort handles the IPv6 bracket form; a host
// with no port returns its "missing port" error and is lowercased as-is.
func canonicalHostPort(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return strings.ToLower(host)
	}
	if port == defaultSchemePort(scheme) {
		return strings.ToLower(bracketIfIPv6(h))
	}
	return strings.ToLower(bracketIfIPv6(h)) + ":" + port
}

func defaultSchemePort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// bracketIfIPv6 restores the brackets SplitHostPort strips from an IPv6
// literal, so "https://[::1]:443" canonicalizes to "https://[::1]".
func bracketIfIPv6(h string) string {
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}
