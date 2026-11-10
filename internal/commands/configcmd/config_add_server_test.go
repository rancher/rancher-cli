package configcmd

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
)

func newAddServerCmd(dir string) *configAddServerCmd {
	return &configAddServerCmd{dir: dir} // nil tlsPreflight: no network in tests
}

func TestAddServerCreatesEntryAndBootstrapsPointer(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)

	res, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, "qa1", data["name"])
	assert.Equal(t, "qa1", data["currentServer"])
	assert.Equal(t, false, data["unchanged"])
	assert.Equal(t, "server qa1 added\n  url: https://qa1.example.com\n  current server set to qa1\n", res.Text)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "qa1", cfg.CurrentServer)
	assert.Equal(t, "https://qa1.example.com", cfg.Servers["qa1"].URL)
}

// TestAddServerTextCAPinnedNoPointerLine asserts the two conditional text lines
// independently: a CA-pinned add gets "ca: pinned", and an add that finds
// the pointer already valid (a prior entry already current) omits the
// "current server set to" line entirely.
func TestAddServerTextCAPinnedNoPointerLine(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	// Seed an existing current server so this add's pointer bootstrap is a
	// no-op (the pointer is already valid).
	_, err := c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://prod.example.com"}, Pos: []string{"prod"}}, nil)
	require.NoError(t, err)

	res, err := c.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com", "cacert": cmdtest.CAPem1}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "server qa1 added\n  url: https://qa1.example.com\n  ca: pinned\n", res.Text)
}

func TestAddServerIdenticalReRunIsUnchanged(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	in := cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"qa1"}}

	_, err := c.Execute(context.Background(), in, nil)
	require.NoError(t, err)

	res, err := c.Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, true, res.Data.(map[string]any)["unchanged"])
	assert.Equal(t, "server qa1 unchanged\n", res.Text)
}

func TestAddServerDifferentURLConflicts(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://old.example.com"}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)

	_, err = c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://new.example.com"}, Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, clierr.Rancher+" config update-server 'qa1' --url 'https://new.example.com'", ce.Hint)
}

func TestAddServerRejectsLegacyPrefixedName(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{config.LegacyCachePrefix + "x"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestAddServerRejectsUserIDPrefixedName asserts the namespace of the
// exec plugin's cache slots of calls with --user-id is refused as a
// server name.
func TestAddServerRejectsUserIDPrefixedName(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{"user-id:x"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, `server names starting with "user-id:" are reserved`)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers)
}

// TestAddServerRejectsExecLoginPrefixedName asserts the other reserved
// namespace: the entries the exec plugin creates for its own logins.
func TestAddServerRejectsExecLoginPrefixedName(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com"}, Pos: []string{config.ExecLoginPrefix + "qa1.example.com"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, `"exec-login-" are reserved`)
}

func TestAddServerRequiresURL(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestAddServerRejectsUnsupportedScheme: an ftp:// (or any non-http(s))
// URL must never commit an entry - config.Effective's credential-read
// path would silently withhold the token for it later, reading as an
// unexplained NOT_LOGGED_IN after an apparently successful add.
func TestAddServerRejectsUnsupportedScheme(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), cmdtest.Input{Str: map[string]string{"url": "ftp://host"}, Pos: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry written for an unsupported scheme")
}

// TestAddServerUnchangedHeadlineBold asserts the idempotent re-add's
// one-line text carries the shared headline bold, matching update-server's
// unchanged branch.
func TestAddServerUnchangedHeadlineBold(t *testing.T) {
	t.Parallel()
	tr := runtime.TextLines("server prod unchanged")
	styled := tr.StyledText(style.Style{Enabled: true})
	assert.Equal(t, "\x1b[1mserver prod unchanged\x1b[0m\n", styled)
	assert.Equal(t, tr.Text, style.Strip(styled))
}

// TestAddServerSameCertificatesOnAnotherPathKeepsTheSavedCA asserts that a
// second add-server with a --cacert file that holds the same certificate as
// the entry's saved CA (a copy with Windows line endings on another path)
// answers unchanged with a warning, and leaves the config file as it was.
func TestAddServerSameCertificatesOnAnotherPathKeepsTheSavedCA(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	certificate := cmdtest.CertificatePEM(t)
	saved := cmdtest.WriteCAFile(t, certificate)
	_, err := c.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com", "cacert": saved}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)

	copyPath := cmdtest.WriteCAFile(t, strings.ReplaceAll(certificate, "\n", "\r\n"))
	res, err := c.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com", "cacert": copyPath}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "ServerAdded", res.Kind)
	data := res.Data.(map[string]any)
	assert.Equal(t, true, data["unchanged"])
	assert.Equal(t, "path", data["ca"])
	assert.Equal(t, "server qa1 unchanged\n", res.Text)
	assert.Contains(t, res.Warnings, `server entry "qa1" keeps its saved CA, which holds the same certificates`)

	after, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after, "the config file is not rewritten")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, saved, cfg.Servers["qa1"].CA)
}

// TestAddServerDifferentCertificateIsAConflictWithBothFingerprints asserts
// that another CA is still refused, with the fingerprints of both
// certificates and no path in the text before the unchanged tail.
func TestAddServerDifferentCertificateIsAConflictWithBothFingerprints(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	c := newAddServerCmd(dir)
	first, second := cmdtest.CertificatePEM(t), cmdtest.CertificatePEM(t)
	saved, other := cmdtest.WriteCAFile(t, first), cmdtest.WriteCAFile(t, second)
	_, err := c.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com", "cacert": saved}, Pos: []string{"qa1"}}, nil)
	require.NoError(t, err)

	_, err = c.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://qa1.example.com", "cacert": other}, Pos: []string{"qa1"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	head, _, _ := strings.Cut(ce.Message, "; to add a new server")
	assert.Equal(t, `server entry "qa1" already exists with a different CA (saved: `+certificateFingerprint(t, first)+
		`; --cacert: `+certificateFingerprint(t, second)+`)`, head)
	assert.NotContains(t, head, filepath.Dir(saved))
	assert.NotContains(t, head, filepath.Dir(other))
	assert.Contains(t, ce.Hint, "--cacert")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, saved, cfg.Servers["qa1"].CA)
}

func certificateFingerprint(t *testing.T, pemText string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(pemText))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return tlstrust.Fingerprint(cert)
}
