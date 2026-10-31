package telemetrysend

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// longestHost is a host of 253 characters, the longest the check accepts.
var longestHost = strings.Join([]string{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", 61)}, ".")

// TestOriginAccepts asserts the values that give an origin and the canonical
// form of each: a lower-case host, the default port dropped, an empty path or
// /, and http only for loopback hosts. The code makes no request.
func TestOriginAccepts(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"https://h.example":                               "https://h.example",
		"https://h.example/":                              "https://h.example",
		"https://H.EXAMPLE:443/":                          "https://h.example",
		"https://h.example:8443":                          "https://h.example:8443",
		"http://localhost:8080":                           "http://localhost:8080",
		"http://LOCALHOST":                                "http://localhost",
		"http://[::1]:8080":                               "http://[::1]:8080",
		"http://127.0.0.2":                                "http://127.0.0.2",
		"http://127.255.255.254/":                         "http://127.255.255.254",
		"https://telemetry.example":                       "https://telemetry.example",
		"https://h.example:65535":                         "https://h.example:65535",
		"https://h.example:1":                             "https://h.example:1",
		"https://192.0.2.1:8443":                          "https://192.0.2.1:8443",
		"https://[2001:db8::1]":                           "https://[2001:db8::1]",
		"https://h-1.example":                             "https://h-1.example",
		"https://h.example:0443":                          "https://h.example",
		"https://h.example:00080":                         "https://h.example:80",
		"http://localhost:80":                             "http://localhost",
		"http://localhost:0080":                           "http://localhost",
		"http://localhost:443":                            "http://localhost:443",
		"https://xn--bcher-kva.example":                   "https://xn--bcher-kva.example",
		"https://" + strings.Repeat("a", 63) + ".example": "https://" + strings.Repeat("a", 63) + ".example",
		"https://" + longestHost:                          "https://" + longestHost,
	} {
		got, err := Origin(value)
		require.NoError(t, err, value)
		assert.Equal(t, want, got, value)
	}
}

// TestOriginRefuses asserts each refused value gives one fixed reason that
// holds no part of the value.
func TestOriginRefuses(t *testing.T) {
	t.Parallel()
	const (
		schemeReason = "it must use https; http is accepted only for localhost, ::1 and 127.0.0.0/8"
		queryReason  = "it holds a query or fragment"
		pathReason   = "it holds a path; give only scheme://host[:port]"
		portReason   = "it holds a port that is not a number from 1 to 65535"
		hostReason   = "it holds a host that is not an IP address or a DNS name"
		ipReason     = "it holds a host that is not a valid IP address"
		asciiReason  = "it holds a host with non-ASCII characters; write the host in its ASCII (xn--) form"
	)
	for _, tc := range []struct{ name, value, reason string }{
		{"http to a named host", "http://h.example", schemeReason},
		{"http to a public address", "http://192.0.2.1", schemeReason},
		{"http to an IPv4 address in IPv6 form", "http://[::ffff:127.0.0.1]", schemeReason},
		{"http to localhost with a dot", "http://localhost.", schemeReason},
		{"http to a loopback address with a zone", "http://[::1%25eth0]", schemeReason},
		{"another scheme", "ftp://h.example", schemeReason},
		{"a query", "https://h.example/?a=1", queryReason},
		{"an empty query", "https://h.example/?", queryReason},
		{"an empty query with no path", "https://h.example?", queryReason},
		{"a fragment", "https://h.example/#f", queryReason},
		{"an empty fragment", "https://h.example#", queryReason},
		{"a path", "https://h.example/other", pathReason},
		{"the wire path", "https://h.example/v1/logs", pathReason},
		{"a path with a slash only after it", "https://h.example//", pathReason},
		{"an encoded slash", "https://h.example/%2F", pathReason},
		{"an empty port", "https://h.example:", portReason},
		{"port 0", "https://h.example:0", portReason},
		{"a port above 65535", "https://h.example:65536", portReason},
		{"a long port", "https://h.example:99999", portReason},
		{"an empty port on a loopback address", "http://[::1]:", portReason},
		{"a semicolon in the host", "https://h.example;x", hostReason},
		{"an underscore in the host", "https://h_x.example", hostReason},
		{"a trailing dot", "https://h.example.", hostReason},
		{"an empty label", "https://h..example", hostReason},
		{"a zone on an address", "https://[fe80::1%25eth0]", hostReason},
		{"a number that is not an IP address", "https://256.1.1.1", ipReason},
		{"three numbers", "https://1.2.3", ipReason},
		{"a label that starts with a hyphen", "https://-h.example", hostReason},
		{"a label that ends with a hyphen", "https://h-.example", hostReason},
		{"a non-ASCII name", "https://b\u00fccher.example", asciiReason},
		{"a label of 64 characters", "https://" + strings.Repeat("a", 64) + ".example", hostReason},
		{"a host of 254 characters", "https://" + strings.Join([]string{strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63), strings.Repeat("d", 62)}, "."), hostReason},
		{"only dots", "https://.", hostReason},
		{"two dots", "https://..", hostReason},
		{"numbers with an empty label", "https://1..2", hostReason},
		{"an IP address with a trailing dot", "https://1.2.3.4.", hostReason},
		{"a user name", "https://user@h.example", "it holds a user name or password"},
		{"a user name and password", "https://user:s3cr3t@h.example", "it holds a user name or password"},
		{"no scheme", "h.example", "it must be absolute (scheme://host)"},
		{"no host", "https://", "it must be absolute (scheme://host)"},
		{"not a URL", "https://h.example/%zz", "it is not a valid URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Origin(tc.value)
			require.ErrorContains(t, err, tc.reason)
			assert.Empty(t, got)
			assert.Equal(t, tc.reason, err.Error())
			assert.NotContains(t, err.Error(), "s3cr3t")
			assert.NotContains(t, err.Error(), "h.example")
		})
	}
}

// TestOriginFallbackErrorsHoldNoPartOfTheValue asserts that when a later
// parse fails, which the URL check rules out for any value it accepts, the
// error is the fixed sentence and holds none of the value, a password
// included. The origin the check returned is replaced to reach each path.
func TestOriginFallbackErrorsHoldNoPartOfTheValue(t *testing.T) {
	t.Parallel()
	const bad = "https://alice:s3cr3t@h.example/%zz"
	for _, tc := range []struct {
		name          string
		value, origin string
	}{
		{"the canonical origin does not parse", "https://h.example", bad},
		{"the value does not parse", bad, "https://h.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := originFrom(tc.value, tc.origin)
			require.Error(t, err)
			assert.Empty(t, got)
			assert.Equal(t, "it is not a valid URL", err.Error())
			assert.NotContains(t, err.Error(), "alice")
			assert.NotContains(t, err.Error(), "s3cr3t")
		})
	}
}
