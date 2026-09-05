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
//
// The canonical escape policy: url.Parse hands back an already-unescaped
// host, and every byte of it is emitted as-is except the two byte values a
// host escape can legitimately introduce AND Go's zone-id grammar accepts
// back in escaped form - a literal '%' (RFC 6874's %25 prefixes an IPv6
// zone id) becomes %25 and a space becomes %20. Nothing else is
// re-escaped: net/url's own escaper rejects every other byte value when it
// arrives as an escape inside a zone id (it only accepts them raw), so
// escaping more would make some inputs unparseable again. Any decoded byte
// outside that set that still leaves the rebuilt URL unparseable fails
// closed: a canonical identity must round-trip through url.Parse, so an
// input that cannot is rejected rather than emitted.
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
	host := canonicalHostPort(scheme, u.Host)
	// '%' first: the space replacement introduces '%' bytes of its own,
	// which must not be re-escaped.
	host = strings.ReplaceAll(host, "%", "%25")
	host = strings.ReplaceAll(host, " ", "%20")
	canonical := scheme + "://" + host
	if _, err := url.Parse(canonical); err != nil {
		return "", fmt.Errorf("server URL %q does not canonicalize to a parseable form: %w", raw, err)
	}
	return canonical, nil
}

// canonicalHostPort lowercases host and drops an explicit port matching the
// scheme's default. net.SplitHostPort handles the IPv6 bracket form; a host
// with no port returns its "missing port" error and is lowercased as-is.
func canonicalHostPort(scheme, host string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return asciiLower(host)
	}
	if port == defaultSchemePort(scheme) {
		return asciiLower(bracketIfIPv6(h))
	}
	return asciiLower(bracketIfIPv6(h)) + ":" + port
}

// asciiLower lowercases only the ASCII letters in s, leaving every other
// byte untouched. An IPv6 zone id may carry raw bytes that are not valid
// UTF-8 (RFC 6874 imposes no encoding on it); strings.ToLower decodes runes
// and would replace such a byte with the UTF-8 encoding of U+FFFD, silently
// growing and corrupting it.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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
