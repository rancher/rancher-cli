package credhelper

import (
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

// serverURLCases are the inputs CheckServerURL and CanonicalServerURL are
// tested with: every accepted and refused form. accepted is the verdict of
// CanonicalServerURL; checkAccepted the verdict of CheckServerURL, which
// also refuses userinfo. canonical is the result when accepted.
var serverURLCases = []struct {
	name, in      string
	accepted      bool
	checkAccepted bool
	canonical     string
}{
	{"plain https", "https://" + partHost, true, true, "https://" + partHost},
	{"scheme case and host case", "HTTPS://HOSTZQ7", true, true, "https://" + partHost},
	{"default https port", "https://" + partHost + ":443", true, true, "https://" + partHost},
	{"default http port", "http://" + partHost + ":80", true, true, "http://" + partHost},
	{"other port", "https://" + partHost + ":8443", true, true, "https://" + partHost + ":8443"},
	{"path query and fragment dropped", "https://" + partHost + partPath + "?q=1#f", true, true, "https://" + partHost},
	{"ipv6", "https://[::1]:8443", true, true, "https://[::1]:8443"},
	{"ipv6 zone id", "https://[fe80::1%25eth0]:443", true, true, "https://[fe80::1%25eth0]"},
	{"an at sign in the path", "https://" + partHost + "/p@" + partUser, true, true, "https://" + partHost},
	{"an encoded at sign in the path", "https://" + partHost + "/p%40" + partUser, true, true, "https://" + partHost},
	{"ftp scheme accepted", "ftp://" + partHost, true, true, "ftp://" + partHost},
	{"user and password", "https://" + partUser + ":" + partPass + "@" + partHost, true, false, ""},
	{"user name alone", "https://" + partUser + "@" + partHost, true, false, ""},
	{"empty user name", "https://@" + partHost, true, false, ""},
	{"password alone", "https://:" + partPass + "@" + partHost, true, false, ""},
	{"an encoded at sign in the user name", "https://" + partUser + "%40x:" + partPass + "@" + partHost, true, false, ""},
	{"userinfo and a path", "https://" + partUser + ":" + partPass + "@" + partHost + partPath, true, false, ""},
	{"userinfo and a bad port", "https://" + partUser + ":" + partPass + "@" + partHost + ":abc", false, false, ""},
	{"a bad port", "https://" + partHost + ":abc", false, false, ""},
	{"schemeless host", partHost + ":8443", false, false, ""},
	{"schemeless userinfo", partUser + ":" + partPass + "@" + partHost + ":1", false, false, ""},
	{"schemeless user name alone", partUser + "@" + partHost, false, false, ""},
	{"empty", "", false, false, ""},
	{"scheme only", "https://", false, false, ""},
	{"an empty hostname", "https://:443", false, false, ""},
	{"an empty hostname and userinfo", "https://" + partUser + ":" + partPass + "@:443", false, false, ""},
	{"a space in the host", "https://" + partHost + " x", false, false, ""},
}

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
