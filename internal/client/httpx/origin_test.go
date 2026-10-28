package httpx_test

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// TestOriginOfNormalizes asserts that scheme and hostname are lowercased, the
// port is defaulted and canonicalized, and IPv6 brackets are stripped for
// comparison and restored for display.
func TestOriginOfNormalizes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, a, b, display string
	}{
		{"host case", "https://IDP.Example/x", "https://idp.example/y", "https://idp.example"},
		{"implicit vs explicit https port", "https://h", "https://h:443/p", "https://h"},
		{"implicit vs explicit http port", "http://h", "http://h:80", "http://h"},
		{"leading zero port", "https://h:0443", "https://h:443", "https://h"},
		{"non-default port kept", "https://h:8443/a", "https://H:8443", "https://h:8443"},
		{"ipv6 default port", "https://[::1]", "https://[::1]:443", "https://[::1]"},
		{"ipv6 explicit port", "https://[::1]:8443", "https://[::1]:8443/p", "https://[::1]:8443"},
		{"ipv4", "https://127.0.0.1:9443", "https://127.0.0.1:9443/", "https://127.0.0.1:9443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := httpx.OriginOf(mustURL(t, tc.a))
			require.NoError(t, err)
			b, err := httpx.OriginOf(mustURL(t, tc.b))
			require.NoError(t, err)
			assert.True(t, a.Equal(b), "%s and %s must be one origin", tc.a, tc.b)
			assert.Equal(t, tc.display, a.String())
			_, wantHostPort, _ := strings.Cut(tc.display, "://")
			assert.Equal(t, wantHostPort, a.HostPort())
		})
	}
}

// TestOriginOfDistinguishes asserts the three axes that make two URLs different
// origins.
func TestOriginOfDistinguishes(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, a, b string }{
		{"scheme", "https://h", "http://h"},
		{"host", "https://a.example", "https://b.example"},
		{"port", "https://h:443", "https://h:8443"},
		{"ipv6 port", "https://[::1]:443", "https://[::1]:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := httpx.OriginOf(mustURL(t, tc.a))
			require.NoError(t, err)
			b, err := httpx.OriginOf(mustURL(t, tc.b))
			require.NoError(t, err)
			assert.False(t, a.Equal(b))
		})
	}
}

// TestOriginOfRejects asserts the constructor's errors: nil, unsupported scheme,
// no host, and a port outside 1..65535.
func TestOriginOfRejects(t *testing.T) {
	t.Parallel()
	_, err := httpx.OriginOf(nil)
	require.ErrorContains(t, err, "missing")
	for _, raw := range []string{"ftp://h", "https:///path", "https:opaque", "https://h:0", "https://h:70000"} {
		u, perr := url.Parse(raw)
		require.NoError(t, perr, raw)
		_, err := httpx.OriginOf(u)
		assert.Error(t, err, raw)
	}
}

// TestOriginZeroValueIsInvalid asserts that the zero value is unusable: Valid is
// false, Equal is false even against itself, String is empty.
func TestOriginZeroValueIsInvalid(t *testing.T) {
	t.Parallel()
	var zero httpx.Origin
	assert.False(t, zero.Valid())
	assert.False(t, zero.Equal(zero))
	assert.Empty(t, zero.String())
	assert.Empty(t, zero.HostPort())
	real, err := httpx.OriginOf(mustURL(t, "https://h"))
	require.NoError(t, err)
	assert.False(t, real.Equal(zero))
}

// TestOriginFieldsAreUnexported proves by reflection that OriginOf is the
// only way to build a non-zero Origin outside this package.
func TestOriginFieldsAreUnexported(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[httpx.Origin]()
	require.Positive(t, typ.NumField())
	for field := range typ.Fields() {
		assert.NotEmpty(t, field.PkgPath, "field %s is exported", field.Name)
	}
}
