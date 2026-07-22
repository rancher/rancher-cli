package credhelper

import (
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalServerURL(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
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
