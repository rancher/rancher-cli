package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoginVerifyWordsACertificateFailureForItsPath asserts the verify step
// of a login with a piped token, when the server's certificate fails the
// check the preflight passed, returns TLS_TRUST (exit 10, not retryable)
// worded for the path - --url for a new entry, or the plain login of an
// entry - with and without RANCHER_CLI_CACERT. No path offers --no-verify;
// the --url login keeps the sentence about undoing its registration that
// follows every verify failure of a registration. Mutations: leave the verify
// connection's TLSRemedy zero (each path, each variant); add the --no-verify
// option to either path's TLS_TRUST error.
func TestLoginVerifyWordsACertificateFailureForItsPath(t *testing.T) {
	const (
		noVerify         = "--no-verify"
		registrationUndo = "; undo the registration with: rancher config delete-server 'prod'"
	)
	for _, tc := range []struct {
		name     string
		urlPath  bool
		variable bool
		remedy   string
	}{
		{name: "--url with a CA from the flag", urlPath: true, remedy: " and pass it with --cacert"},
		{name: "--url with the variable", urlPath: true, variable: true,
			remedy: "; the CA came from RANCHER_CLI_CACERT - pass --cacert <file> to this login, or unset RANCHER_CLI_CACERT, or set it to the server's CA"},
		{name: "plain login of an entry",
			remedy: "; save it for this entry with: rancher config update-server 'prod' --cacert <file>"},
		{name: "plain login of an entry with the variable", variable: true,
			remedy: "; the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; to save a CA for this entry, run: rancher config update-server 'prod' --cacert <file>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvURL, "")
			t.Setenv(config.EnvCACert, "")
			server := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			other := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			srv := newTLSFake(t, server)

			dir, store := cmdtest.FilePolicyDir(t)
			cmd := &loginCmd{
				dir:          dir,
				newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
				tlsPreflight: func(context.Context, string, string) (string, error) { return "", nil },
				verify:       provider.Owner,
			}
			in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}, Str: map[string]string{}}
			if tc.variable {
				t.Setenv(config.EnvCACert, other.pem)
			}
			if tc.urlPath {
				in.Str["url"] = srv.URL
				if !tc.variable {
					in.Str["cacert"] = other.pem
				}
			} else {
				ca := other.pem
				if tc.variable {
					ca = ""
				}
				seedServer(t, dir, "prod", srv.URL, ca, false)
			}

			_, err := cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
			assert.Equal(t, 10, ce.ExitCode())
			assert.False(t, ce.Retryable)
			d, ok := ce.Details.(clierr.LoginFailureDetails)
			require.True(t, ok, "got %T", ce.Details)
			primary, ok := d.Primary.Details.(tlstrust.Details)
			require.True(t, ok, "the primary keeps the certificate details")
			assert.Equal(t, tlstrust.KindUnknownAuthority, primary.Kind)
			want := tlstrust.UnknownAuthorityMessage(primary.IsLeafCertificate(), tc.remedy)
			assert.NotContains(t, ce.Message, noVerify)
			if tc.urlPath {
				assert.Equal(t, want+registrationUndo, ce.Message)
				return
			}
			assert.Equal(t, want, ce.Message)
		})
	}
}

// TestVerifyTLSRemedyWordsEachLoginRow asserts the wording of a certificate
// failure of the verify step, row by row, in every slot. Mutations: change
// any string, or give a path another path's row.
func TestVerifyTLSRemedyWordsEachLoginRow(t *testing.T) {
	plain := func(ca string) tlstrust.Remedy {
		return tlstrust.Remedy{CA: ca, URL: "the server URL saved for entry 'prod'", Fix: "a different CA"}
	}
	for _, tc := range []struct {
		name                   string
		delegated, viaVariable bool
		want                   tlstrust.Remedy
	}{
		{"--url with a CA from the flag or a prompt", true, false, tlstrust.Remedy{}},
		{"--url with the variable", true, true,
			tlstrust.Remedy{CA: "; the CA came from RANCHER_CLI_CACERT - pass --cacert <file> to this login, or unset RANCHER_CLI_CACERT, or set it to the server's CA"}},
		{"plain login of an entry", false, false,
			plain("; save it for this entry with: rancher config update-server 'prod' --cacert <file>")},
		{"plain login of an entry with the variable", false, true,
			plain("; the CA came from RANCHER_CLI_CACERT - unset it, or set it to the server's CA; to save a CA for this entry, run: rancher config update-server 'prod' --cacert <file>")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, verifyTLSRemedy(tc.delegated, tc.viaVariable, "prod"))
		})
	}
}
