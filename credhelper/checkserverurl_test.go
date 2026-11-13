package credhelper

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// distinctive parts of an input, each of which no error text may hold.
const (
	partHost = "hostzq7"
	partUser = "userzq7"
	partPass = "pwzq7"
	partPath = "/pathzq7"
)

// reason names the reason CheckServerURL refuses a row with, where a row
// asserts it.
type reason int

const (
	// reasonUnchecked asserts nothing about the reason.
	reasonUnchecked reason = iota
	// reasonRawAt asserts the refusal is errServerURLRawAt.
	reasonRawAt
	// reasonOther asserts the refusal is some other reason.
	reasonOther
)

type serverURLCase struct {
	name, in      string
	accepted      bool
	checkAccepted bool
	canonical     string
	reason        reason
}

// baseServerURLCases are the plain, userinfo and unparsable inputs of
// serverURLCases, and the one row of a path that holds an @.
var baseServerURLCases = []serverURLCase{
	{"plain https", "https://" + partHost, true, true, "https://" + partHost, reasonUnchecked},
	{"scheme case and host case", "HTTPS://HOSTZQ7", true, true, "https://" + partHost, reasonUnchecked},
	{"default https port", "https://" + partHost + ":443", true, true, "https://" + partHost, reasonUnchecked},
	{"default http port", "http://" + partHost + ":80", true, true, "http://" + partHost, reasonUnchecked},
	{"other port", "https://" + partHost + ":8443", true, true, "https://" + partHost + ":8443", reasonUnchecked},
	{"path query and fragment dropped", "https://" + partHost + partPath + "?q=1#f", true, true, "https://" + partHost, reasonUnchecked},
	{"ipv6", "https://[::1]:8443", true, true, "https://[::1]:8443", reasonUnchecked},
	{"ipv6 zone id", "https://[fe80::1%25eth0]:443", true, true, "https://[fe80::1%25eth0]", reasonUnchecked},
	{"an at sign in the path", "https://" + partHost + "/p@" + partUser, true, false, "", reasonRawAt},
	{"an encoded at sign in the path", "https://" + partHost + "/p%40" + partUser, true, true, "https://" + partHost, reasonUnchecked},
	{"ftp scheme accepted", "ftp://" + partHost, true, true, "ftp://" + partHost, reasonUnchecked},
	{"user and password", "https://" + partUser + ":" + partPass + "@" + partHost, true, false, "", reasonUnchecked},
	{"user name alone", "https://" + partUser + "@" + partHost, true, false, "", reasonUnchecked},
	{"empty user name", "https://@" + partHost, true, false, "", reasonUnchecked},
	{"password alone", "https://:" + partPass + "@" + partHost, true, false, "", reasonUnchecked},
	{"an encoded at sign in the user name", "https://" + partUser + "%40x:" + partPass + "@" + partHost, true, false, "", reasonUnchecked},
	{"userinfo and a path", "https://" + partUser + ":" + partPass + "@" + partHost + partPath, true, false, "", reasonUnchecked},
	{"userinfo and a bad port", "https://" + partUser + ":" + partPass + "@" + partHost + ":abc", false, false, "", reasonUnchecked},
	{"a bad port", "https://" + partHost + ":abc", false, false, "", reasonUnchecked},
	{"schemeless host", partHost + ":8443", false, false, "", reasonUnchecked},
	{"schemeless userinfo", partUser + ":" + partPass + "@" + partHost + ":1", false, false, "", reasonUnchecked},
	{"schemeless user name alone", partUser + "@" + partHost, false, false, "", reasonUnchecked},
	{"empty", "", false, false, "", reasonUnchecked},
	{"scheme only", "https://", false, false, "", reasonUnchecked},
	{"an empty hostname", "https://:443", false, false, "", reasonUnchecked},
	{"an empty hostname and userinfo", "https://" + partUser + ":" + partPass + "@:443", false, false, "", reasonUnchecked},
	{"a space in the host", "https://" + partHost + " x", false, false, "", reasonUnchecked},
}

// A user name and a first part of a password that read as a host and a port.
const rawAtHostPort = "admin:1234"

// rawAtCases are accepted by CanonicalServerURL and refused by CheckServerURL
// because the raw input holds an @ after the host.
var rawAtCases = []serverURLCase{
	{"a password with a slash", "https://" + rawAtHostPort + "/" + partPass + "@" + partHost, true, false, "", reasonRawAt},
	{"a password with a slash and a path after the at sign", "https://" + rawAtHostPort + "/" + partPass + "@" + partHost + "/y", true, false, "", reasonRawAt},
	{"a password with a question mark", "https://" + rawAtHostPort + "?" + partPass + "@" + partHost, true, false, "", reasonRawAt},
	{"a password with a hash", "https://" + rawAtHostPort + "#" + partPass + "@" + partHost, true, false, "", reasonRawAt},
	{"an at sign in the middle of a path", "https://" + partHost + "/p@q", true, false, "", reasonRawAt},
	{"a path that is an at sign", "https://" + partHost + "/@", true, false, "", reasonRawAt},
	{"a path that ends in an at sign", "https://" + partHost + "/x@", true, false, "", reasonRawAt},
	{"an at sign after an empty path segment", "https://" + partHost + "//@", true, false, "", reasonRawAt},
	{"an at sign after an ipv6 host", "https://[::1]/x@y", true, false, "", reasonRawAt},
	{"an at sign after an ipv6 host and a port", "https://[::1]:8443/x@y", true, false, "", reasonRawAt},
	{"an at sign after an ipv6 host with a zone id", "https://[fe80::1%25en0]/x@y", true, false, "", reasonRawAt},
	{"an at sign after a port and a longer path", "https://" + partHost + ":8443/a/b@c?x=1", true, false, "", reasonRawAt},
	{"an at sign in the query", "https://" + partHost + "?e=a@" + partUser, true, false, "", reasonRawAt},
	{"an at sign in the fragment", "https://" + partHost + "#a@" + partUser, true, false, "", reasonRawAt},
	{"an encoded at sign in the path and a raw one in the query", "https://" + partHost + "/p%40q?e=a@" + partUser, true, false, "", reasonRawAt},
	{"one encoded and one raw at sign", "https://" + rawAtHostPort + "/x%40" + partHost + "@y", true, false, "", reasonRawAt},
	{"an at sign and an ftp scheme", "ftp://" + partHost + "/x@" + partUser, true, false, "", reasonRawAt},
}

// otherReasonCases are refused for a reason of their own, which an @ after
// the host does not change.
var otherReasonCases = []serverURLCase{
	{"a bad port and an at sign in the path", "https://admin:" + partPass + "/x@" + partHost, false, false, "", reasonOther},
	{"a bad port and an at sign in the query", "https://admin:" + partPass + "?x@" + partHost, false, false, "", reasonOther},
	{"a bad port and an at sign in the fragment", "https://admin:" + partPass + "#x@" + partHost, false, false, "", reasonOther},
	{"userinfo and an at sign in the path", "https://" + partUser + ":" + partPass + "@" + partHost + "/x@y", true, false, "", reasonOther},
	{"a user name and a path", "https://" + partUser + "@" + partHost + "/x", true, false, "", reasonOther},
	{"an empty user name and a host", "https://@" + partHost, true, false, "", reasonOther},
	{"an empty hostname and an at sign in the path", "https://:443/x@y", false, false, "", reasonOther},
	{"a relative reference with an at sign", "h/x@y", false, false, "", reasonOther},
	{"a scheme relative reference with an at sign", "//h/x@y", false, false, "", reasonOther},
}

// rawAtAcceptedCases hold no raw @ in the input and stay accepted.
var rawAtAcceptedCases = []serverURLCase{
	{"a trailing slash", "https://" + partHost + "/", true, true, "https://" + partHost, reasonUnchecked},
	{"upper case host and a default port with a path", "https://" + "HOSTZQ7" + ":443/a", true, true, "https://" + partHost, reasonUnchecked},
	{"a port and a path with a trailing slash", "https://" + partHost + ":8443/dashboard/", true, true, "https://" + partHost + ":8443", reasonUnchecked},
	{"a long path, a query and a fragment", "https://" + partHost + "/a/b/c?x=1&y=2#f", true, true, "https://" + partHost, reasonUnchecked},
	{"an empty query", "https://" + partHost + "?", true, true, "https://" + partHost, reasonUnchecked},
	{"an empty fragment", "https://" + partHost + "#", true, true, "https://" + partHost, reasonUnchecked},
	{"a slash and an empty query", "https://" + partHost + "/?", true, true, "https://" + partHost, reasonUnchecked},
	{"an ipv6 host", "https://[::1]", true, true, "https://[::1]", reasonUnchecked},
	{"an ipv6 host, a port and a path", "https://[::1]:8443/x", true, true, "https://[::1]:8443", reasonUnchecked},
	{"an ipv6 host with a zone id and a path", "https://[fe80::1%25eth0]/a", true, true, "https://[fe80::1%25eth0]", reasonUnchecked},
	{"a path that is an encoded at sign", "https://" + partHost + "/%40", true, true, "https://" + partHost, reasonUnchecked},
	{"a double encoded at sign", "https://" + partHost + "/%2540", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded form of a password with a slash", "https://" + rawAtHostPort + "/x%40" + partHost, true, true, "https://" + rawAtHostPort, reasonUnchecked},
	{"an encoded at sign, a port and a trailing slash", "https://" + rawAtHostPort + "/x%40" + partHost + "/", true, true, "https://" + rawAtHostPort, reasonUnchecked},
	{"an encoded at sign, another port and a trailing slash", "https://" + partHost + ":8443/x%40/", true, true, "https://" + partHost + ":8443", reasonUnchecked},
	{"an encoded at sign, an ipv6 host, a port and a trailing slash", "https://[::1]:8443/a%40b/", true, true, "https://[::1]:8443", reasonUnchecked},
	{"an encoded at sign in the query", "https://" + partHost + "?e=a%40b", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign in the fragment", "https://" + partHost + "/#a%40b", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a brace in the path", "https://" + partHost + "/p%40q{", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a space in the path", "https://" + partHost + "/a b%40c", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a quote in the path", "https://" + partHost + "/p%40q\"", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a bar in the path", "https://" + partHost + "/%40|", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a brace in the fragment", "https://" + partHost + "#a%40b{", true, true, "https://" + partHost, reasonUnchecked},
	{"an encoded at sign and a space in the fragment", "https://" + partHost + "#a%40b c", true, true, "https://" + partHost, reasonUnchecked},
}

// serverURLCases are the inputs CheckServerURL and CanonicalServerURL are
// tested with: every accepted and refused form. accepted is the verdict of
// CanonicalServerURL; checkAccepted the verdict of CheckServerURL, which
// also refuses userinfo and an @ after the host. canonical is the result when
// accepted, and reason the reason a refusal is asserted to have.
var serverURLCases = slices.Concat(baseServerURLCases, rawAtCases, otherReasonCases, rawAtAcceptedCases)

func assertTextHoldsNoPart(t *testing.T, in string, err error) {
	t.Helper()
	require.Error(t, err)
	text := strings.ToLower(err.Error())
	for _, part := range []string{partHost, partUser, partPass, strings.TrimPrefix(partPath, "/")} {
		assert.NotContains(t, text, part, "input %q", in)
	}
}

// TestCheckServerURL asserts which inputs CheckServerURL accepts, that it
// returns the canonical form CanonicalServerURL returns for each accepted
// input, that every accepted input also canonicalizes, and that no error text
// holds a distinctive part of the input.
func TestCheckServerURL(t *testing.T) {
	t.Parallel()
	for _, tc := range serverURLCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CheckServerURL(tc.in)
			if !tc.checkAccepted {
				assertTextHoldsNoPart(t, tc.in, err)
				assert.Empty(t, got)
				switch tc.reason {
				case reasonRawAt:
					require.ErrorIs(t, err, errServerURLRawAt)
				case reasonOther:
					require.NotErrorIs(t, err, errServerURLRawAt)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.canonical, got)
			canonical, cerr := CanonicalServerURL(tc.in)
			require.NoError(t, cerr, "an input CheckServerURL accepts must canonicalize")
			assert.Equal(t, canonical, got)
		})
	}
}

// TestCanonicalServerURLTable asserts which inputs CanonicalServerURL
// accepts (userinfo is dropped, not refused) and that none of its error
// texts holds a distinctive part of the input.
func TestCanonicalServerURLTable(t *testing.T) {
	t.Parallel()
	for _, tc := range serverURLCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalServerURL(tc.in)
			if !tc.accepted {
				assertTextHoldsNoPart(t, tc.in, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.NotContains(t, strings.ToLower(got), partUser)
			assert.NotContains(t, strings.ToLower(got), partPass)
		})
	}
}

// TestRawAtReasonText asserts the exact reason of the refusal for an @ after
// the host, and that it holds no ; (the refusal form splits at the first one)
// and no part of an input.
func TestRawAtReasonText(t *testing.T) {
	t.Parallel()
	require.EqualError(t, errServerURLRawAt,
		"it holds an @ after the host, which usually means a password with a /, ? or # in it")
	assert.NotContains(t, errServerURLRawAt.Error(), ";")
	_, err := CheckServerURL("https://" + rawAtHostPort + "/" + partPass + "@" + partHost)
	require.Error(t, err)
	require.ErrorIs(t, err, errServerURLRawAt)
	assertTextHoldsNoPart(t, "", err)
	assert.NotContains(t, err.Error(), "admin")
	assert.NotContains(t, err.Error(), "1234")
}
