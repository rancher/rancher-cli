package credhelper

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalServerURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"lowercases scheme and host", "HTTPS://EXAMPLE.com", "https://example.com"},
		{"strips default https port", "https://example.com:443", "https://example.com"},
		{"strips default http port", "http://example.com:80", "http://example.com"},
		{"keeps non-default port", "https://example.com:8443", "https://example.com:8443"},
		{"drops path query fragment", "https://example.com/v3?x=1#f", "https://example.com"},
		{"ipv6 with default port", "https://[::1]:443", "https://[::1]"},
		{"ipv6 with custom port", "https://[::1]:8443", "https://[::1]:8443"},
		{"plain host control", "https://example.com", "https://example.com"},
		{"ipv6 zone id keeps its %25 escape", "https://[fe80::1%25eth0]:443", "https://[fe80::1%25eth0]"},
		{"minimal percent-in-host", "http://a%25b", "http://a%25b"},
		{"escaped space in zone id round-trips", "http://[::%25%200]", "http://[::%25%200]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalServerURL(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			_, perr := url.Parse(got)
			require.NoError(t, perr, "canonical output %q must re-parse", got)

			again, aerr := CanonicalServerURL(got)
			require.NoError(t, aerr)
			assert.Equal(t, got, again, "canonicalization must be idempotent")
		})
	}
}

func TestCanonicalServerURLErrors(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "example.com", "://nope", "https://"} {
		_, err := CanonicalServerURL(in)
		assert.Error(t, err, "input %q", in)
	}
}

func TestCanonicalServerURLEmptyHostname(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"https://:443", "https://:8443", "http://:80", "http://:8080"} {
		_, err := CanonicalServerURL(in)
		assert.Error(t, err, "input %q must be rejected: hostname is empty", in)
	}
}

func TestValidHelperName(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidHelperName("vault"))
	assert.True(t, ValidHelperName("my-helper2"))
	for _, bad := range []string{"", "Vault", "va ult", "va/ult", `va\ult`, "va.ult", "-vault"} {
		assert.False(t, ValidHelperName(bad), "name %q", bad)
	}
}
