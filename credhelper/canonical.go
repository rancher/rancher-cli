package credhelper

import (
	"errors"
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
	u, err := parseServerURL(raw)
	if err != nil {
		return "", err
	}
	return canonicalFromURL(u)
}

// CheckServerURL refuses a server URL that carries userinfo (a user name,
// with or without a password) or that does not parse into a scheme and a
// host, and returns the canonical form CanonicalServerURL returns for every
// URL it accepts. A scheme other than http or https is not refused here: the
// callers that need http or https check that themselves. The error text
// carries no part of the input, because the input may hold a password.
func CheckServerURL(raw string) (string, error) {
	u, err := parseServerURL(raw)
	if err != nil {
		return "", err
	}
	if u.User != nil {
		return "", errServerURLUserinfo
	}
	return canonicalFromURL(u)
}

// The error texts below carry no part of the URL they refuse. They are
// phrased as the reason after "the server URL in <source> cannot be used: ".
var (
	errServerURLSyntax   = errors.New("it is not a valid URL")
	errServerURLRelative = errors.New("it must be absolute (scheme://host)")
	errServerURLNoHost   = errors.New("it has no host name")
	errServerURLUserinfo = errors.New("it holds a user name or password")
	errServerURLRebuild  = errors.New("it does not parse as scheme://host")
)

func parseServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errServerURLSyntax
	}
	return u, nil
}

func canonicalFromURL(u *url.URL) (string, error) {
	if u.Scheme == "" || u.Host == "" {
		return "", errServerURLRelative
	}
	if u.Hostname() == "" {
		return "", errServerURLNoHost
	}
	scheme := strings.ToLower(u.Scheme)
	host := canonicalHostPort(scheme, u.Host)
	// '%' first: the space replacement introduces '%' bytes of its own,
	// which must not be re-escaped.
	host = strings.ReplaceAll(host, "%", "%25")
	host = strings.ReplaceAll(host, " ", "%20")
	canonical := scheme + "://" + host
	if _, err := url.Parse(canonical); err != nil {
		return "", errServerURLRebuild
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
