package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthStatusClassifiesACertificateFailure asserts auth status, with
// the production probes, fails its connectivity preflight with TLS_TRUST
// (exit 10, not retryable, no retry advice) when the saved CA does not match
// the server's certificate, worded for the entry. Mutations: leave the
// preflight's client unwrapped; map the error to the retryable "cannot
// reach server" failure; leave the entry's TLSRemedy zero.
func TestAuthStatusClassifiesACertificateFailure(t *testing.T) {
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvCACert, "")
	server := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	other := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, server)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, other.pem, true)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, "tok:secret"))

	_, err := NewCheck(dir, store).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	assert.Equal(t, 10, ce.ExitCode())
	assert.False(t, ce.Retryable)
	d, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	want := tlstrust.UnknownAuthorityMessage(d.IsLeafCertificate(),
		"; <file> is a file that holds the server's CA certificate; save it for this entry with: "+clierr.Rancher+" config update-server 'prod' --cacert <file>")
	assert.Equal(t, want, ce.Message)
}
