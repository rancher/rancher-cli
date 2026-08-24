package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

type args struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (x args) String(k string) string  { return x.s[k] }
func (x args) Bool(k string) bool      { return x.b[k] }
func (x args) Strings(string) []string { return nil }
func (x args) IsSet(string) bool       { return false }
func (x args) Args() []string          { return x.a }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// lineReader yields one line at a time without buffering, so multiple
// sequential Prompt/PromptSecret calls (each creating a fresh bufio.Scanner
// per ui.Prompter.readLine) don't lose data to a Scanner's internal buffer —
// a shared strings.Reader over multi-line input loses everything past the
// first line once one Scanner over-reads it. Copied from
// internal/token/token_test.go (unexported there too) rather than imported,
// since it's test-only plumbing with no production use.
type lineReader struct {
	lines []string
	pos   int
}

func newLineReader(lines ...string) *lineReader {
	return &lineReader{lines: lines}
}

func (r *lineReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.lines) {
		return 0, io.EOF
	}
	line := r.lines[r.pos] + "\n"
	r.pos++
	n := copy(p, line)
	return n, nil
}

// stdinPrompter builds a Prompter whose input is a canned reader (for --token-stdin).
func stdinPrompter(tok string) *ui.Prompter {
	return ui.New(&bytes.Buffer{}, strings.NewReader(tok), true, false)
}

// filePolicyDir returns a config dir pinned to the file backend, so login's
// RepinForLogin is a no-op and never probes or writes the real OS keychain.
func filePolicyDir(t *testing.T) (string, config.CredStore) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, config.NewFileStore(dir)
}

// stubRancherVersion stands in for loginCmd.rancherVersion in tests that
// aren't exercising the PS-8 version probe itself: it never dials out, so
// login's success path can be tested against unresolvable/example hostnames
// without a real network call.
func stubRancherVersion(context.Context, config.Effective) (string, error) {
	return "v2.20.0", nil
}

// newLoginCmd builds a loginCmd with the version probe stubbed and the
// credential store fixed via a factory, mirroring NewLogin's wiring but
// for direct struct construction in tests. The factory ignores the policy
// and always returns store, which is sufficient for tests that control
// the store directly (the store-under-lock race test uses its own factory).
func newLoginCmd(dir string, store config.CredStore) runtime.ReadCommand {
	return &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
	}
}

// setErrStore wraps a real store, failing SetToken with a fixed error.
type setErrStore struct {
	config.CredStore
	setErr error
}

func (s setErrStore) SetToken(config.ServerRef, string) error { return s.setErr }

func TestLoginReadOnlyHelperWarnsAndWritesConfig(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return setErrStore{CredStore: store, setErr: fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported)}, "", nil
	}}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "https://r.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	require.NoError(t, err, "read-only helper must not fail login")
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "read-only")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["prod"]
	assert.True(t, ok, "config written exactly as on success")
}

// TestLoginRejectsLegacyPrefixedName is the round-7 finding-3 repro: a
// "legacy:"-prefixed --name must be rejected before any auth or config write —
// such a server entry would win the token shim's exact-name stored-login tier
// and defeat its documented server-ranking guarantee.
func TestLoginRejectsLegacyPrefixedName(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	_, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "https://r.ex", "name": config.LegacyCachePrefix + "alice"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, "reserved")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers[config.LegacyCachePrefix+"alice"]
	assert.False(t, ok, "the reserved name must never reach the config")
}

func TestLoginOtherStoreErrorAborts(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return setErrStore{CredStore: store, setErr: errors.New("keychain exploded")}, "", nil
	}}
	_, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "https://r.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	require.Error(t, err)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["prod"]
	assert.False(t, ok, "abort-before-config-write preserved")
}

func TestLoginTokenViaStdin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	res, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id-1:secret-1\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer)
	assert.Equal(t, "https://rancher.example.com", cfg.Servers["acme"].URL)

	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id-1:secret-1", tok)
}

func TestLoginExtTokenViaStdin(t *testing.T) {
	// ext-issued bearers look like "ext/token-abc:secret"; the id:secret split
	// cuts at the FIRST colon so the ext/ prefix stays inside the id (EXT-3).
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	res, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("ext/token-abc:secretxyz\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "ext/token-abc:secretxyz", tok)
}

// TestLoginTokenStdinRejectsProviderFlags pins the fix for combining
// --token-stdin with any provider flag: the two paths are mutually
// exclusive, and silently ignoring the provider flags would surprise a
// caller who set --ttl expecting it to reach the mint request.
func TestLoginTokenStdinRejectsProviderFlags(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{"auth-provider", "auth-flow", "ttl"} {
		in := args{
			s: map[string]string{"server": "https://r.ex", extra: "x"},
			b: map[string]bool{"token-stdin": true},
		}
		_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).
			Execute(t.Context(), in, stdinPrompter("id:sec\n"))
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, extra)
		assert.Equal(t, clierr.CodeValidation, ce.Code, extra)
	}
}

// TestLoginProviderFlowNonTTYAllowedWithExplicitProvider mirrors get-token's
// own TestOAuthDeviceCodeFlowNonTTYAllowed: naming --auth-provider explicitly
// must reach the provider flow without a TTY — the finer-grained gates
// inside it (OAuth needs none for device-code) are the real gate, not
// login's own outer CanPrompt check.
func TestLoginProviderFlowNonTTYAllowedWithExplicitProvider(t *testing.T) {
	t.Parallel()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer fake.Close()

	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{
		"server": fake.URL, "name": "prod", "auth-provider": "azureADProvider", "auth-flow": "devicecode",
	}}
	p := ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false) // no TTY, no --no-input
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.Error(t, err, "device-code will fail on OAuth config/network, but not on login's own gate")
	assert.NotContains(t, err.Error(), "token required", "non-TTY must bypass login's outer gate when --auth-provider is explicit")
}

func TestLoginTokenViaPrompt(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	// isTTY=true, noInput=false -> CanPrompt. Two prompts now fire (EXT-R-8's
	// paste-or-provider menu, then the secret itself), so the input needs a
	// reader that doesn't lose the second line to Scanner over-read (see
	// lineReader's doc comment) — a plain strings.Reader would drop "id:sec".
	p := ui.New(&bytes.Buffer{}, newLineReader("1", "id:sec"), false, true)
	in := args{s: map[string]string{"server": "https://r.ex", "name": "acme"}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginServerRequired(t *testing.T) {
	t.Parallel()
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"token-stdin": true}}, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginUnaffectedWithoutGlobalOverride(t *testing.T) {
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	_, err := newLoginCmd(dir, store).
		Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
}

func TestLoginTokenRequiredNonInteractive(t *testing.T) {
	t.Parallel()
	// No --token-stdin and !CanPrompt (Discard) -> VALIDATION "token required".
	in := args{s: map[string]string{"server": "https://r.ex"}}
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), in, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyStdinToken(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStdinNonEOFErrorIsGeneric(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	p := ui.New(&bytes.Buffer{}, errReader{}, true, false)
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestLoginBadTokenShape(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("notacolon\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStripsUserinfoAndQuery(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://admin:pw@rancher.ex/v3?debug=1#frag", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://rancher.ex", cfg.Servers["acme"].URL)
}

// TestLoginDefaultNameIsHost makes sure a nameless login names its entry
// after the canonical host, so two nameless logins to different servers
// never share (and silently replace) one entry.
func TestLoginDefaultNameIsHost(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://r.ex", cfg.Servers["r.ex"].URL)
	assert.Equal(t, "r.ex", cfg.CurrentServer)
}

// TestLoginNamelessLoginsToDistinctServersKeepBothEntries pins the clobber
// fix: the second nameless login must not replace the first server's entry
// (the old literal-"default" behavior lost it).
func TestLoginNamelessLoginsToDistinctServersKeepBothEntries(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	first := args{s: map[string]string{"server": "https://one.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	second := args{s: map[string]string{"server": "https://two.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), second, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://one.ex", cfg.Servers["one.ex"].URL)
	assert.Equal(t, "https://two.ex", cfg.Servers["two.ex"].URL)
	assert.Equal(t, "two.ex", cfg.CurrentServer)
}

// TestLoginNamelessReloginReusesExistingNamedEntry: a nameless re-login to a
// URL that already has a user-named entry refreshes that entry in place —
// never a host-named duplicate of the same server.
func TestLoginNamelessReloginReusesExistingNamedEntry(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	named := args{s: map[string]string{"server": "https://r.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), named, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	nameless := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://r.ex", cfg.Servers["prod"].URL)
	assert.NotContains(t, cfg.Servers, "r.ex", "no host-named duplicate of an already-named server")
	assert.Equal(t, "prod", cfg.CurrentServer)
}

// TestLoginNamelessReuseIsSpellingInsensitive: a stored URL that is not
// byte-identical to a fresh canonicalization (hand-edited config, an entry
// from before canonicalization) still matches — the reuse comparison is
// config.SameServerURL, never ==.
func TestLoginNamelessReuseIsSpellingInsensitive(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	named := args{s: map[string]string{"server": "https://Rancher.EX", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), named, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	// Force a non-canonical stored spelling, as a hand edit would.
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["prod"] = config.Server{URL: "https://Rancher.EX"}
	require.NoError(t, cfg.Write())

	nameless := args{s: map[string]string{"server": "https://rancher.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Servers, "rancher.ex", "no duplicate entry for a differently spelled same server")
	assert.Equal(t, "prod", cfg.CurrentServer)
}

// TestLoginSameHostDifferentSchemeGetsDistinctEntries: http://X and
// https://X are different server identities — the derived-name ladder gives
// the second one a scheme-prefixed entry instead of refusing or clobbering.
func TestLoginSameHostDifferentSchemeGetsDistinctEntries(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	first := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	second := args{s: map[string]string{"server": "http://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), second, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://r.ex", cfg.Servers["r.ex"].URL)
	assert.Equal(t, "http://r.ex", cfg.Servers["http-r.ex"].URL)
}

// TestLoginDerivedNameSkipsLegacyReservedPrefix: a host literally named
// legacy with a port would derive an entry name inside the legacy shim's
// reserved "legacy:" cache-slot namespace; the ladder skips to the
// scheme-prefixed candidate instead.
func TestLoginDerivedNameSkipsLegacyReservedPrefix(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": "https://legacy:8443"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Servers, "legacy:8443")
	assert.Equal(t, "https://legacy:8443", cfg.Servers["https-legacy:8443"].URL)
}

// TestLoginNamelessReuseSkipsLegacyReservedEntry: a hand-edited entry inside
// the legacy shim's reserved namespace is never reused even when its URL
// matches — the login lands on a safe derived name and leaves the reserved
// key untouched.
func TestLoginNamelessReuseSkipsLegacyReservedEntry(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[config.LegacyCachePrefix+"8443"] = config.Server{URL: "https://legacy:8443"}
	require.NoError(t, cfg.Write())

	in := args{s: map[string]string{"server": "https://legacy:8443"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://legacy:8443", cfg.Servers["https-legacy:8443"].URL, "login lands on the safe derived name")
	assert.Equal(t, "https://legacy:8443", cfg.Servers[config.LegacyCachePrefix+"8443"].URL, "the reserved hand-edited entry is untouched")
	assert.Equal(t, "https-legacy:8443", cfg.CurrentServer)
}

// TestLoginReloginPreservesStoredCA: a re-login that reuses an entry (token
// expiry is the everyday case) must never erase the CA the entry was
// verified with; only an explicit --cacert replaces it.
func TestLoginReloginPreservesStoredCA(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	withCA := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": "-----BEGIN CERTIFICATE-----fake"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), withCA, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	nameless := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----fake", cfg.Servers["prod"].CA, "a nameless re-login must not erase the stored CA")

	replace := args{s: map[string]string{"server": "https://r.ex", "cacert": "-----BEGIN CERTIFICATE-----new"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), replace, stdinPrompter("id3:sec3\n"))
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----new", cfg.Servers["prod"].CA, "an explicit --cacert replaces the stored CA")
}

// TestLoginVersionProbeUsesPersistedCA: the post-login version probe must
// dial with the CA the entry was persisted with — on a nameless re-login
// that preserves a stored CA, the raw --cacert flag is empty and would build
// a system-roots client that fails against the custom-CA server.
func TestLoginVersionProbeUsesPersistedCA(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	var probeCA string
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: func(_ context.Context, eff config.Effective) (string, error) {
			probeCA = eff.CA
			return "v2.20.0", nil
		},
	}
	withCA := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": "-----BEGIN CERTIFICATE-----fake"}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), withCA, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	require.Equal(t, "-----BEGIN CERTIFICATE-----fake", probeCA)

	nameless := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----fake", probeCA,
		"the probe must use the preserved stored CA, never the empty --cacert flag")
}

// TestLoginNameCollisionHintNamesTheEntry: the refusal hint must be
// copy-paste valid — auth logout takes the entry NAME positionally and has
// no --server flag.
func TestLoginNameCollisionHintNamesTheEntry(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	first := args{s: map[string]string{"server": "https://one.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	second := args{s: map[string]string{"server": "https://two.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), second, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Hint, "rancher auth logout prod")
	assert.NotContains(t, ce.Hint, "--server", "auth logout has no --server flag; the hint must be valid verbatim")
}

// TestLoginNameCollisionRefusesBeforeAuthentication: the refusal fires
// before any token is read or auth flow launched — with an empty stdin the
// collision message (not a missing-token error) must come back, proving the
// pre-auth check ordered first.
func TestLoginNameCollisionRefusesBeforeAuthentication(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	first := args{s: map[string]string{"server": "https://one.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	second := args{s: map[string]string{"server": "https://two.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), second, stdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, `"prod" already points at`, "the collision refusal must precede token reading")
}

// TestLoginNameCollisionWithDifferentURLRefuses: an explicit --name that
// matches an entry pointing at a DIFFERENT server is a structured refusal,
// never an overwrite.
func TestLoginNameCollisionWithDifferentURLRefuses(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	first := args{s: map[string]string{"server": "https://one.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	second := args{s: map[string]string{"server": "https://two.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), second, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `"prod" already points at https://one.ex`)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://one.ex", cfg.Servers["prod"].URL, "the existing entry is untouched")
	assert.Equal(t, "prod", cfg.CurrentServer)
}

func TestLoginEmptyPromptToken(t *testing.T) {
	t.Parallel()
	// TTY prompt returns a blank line -> "no token", not an id:secret shape error.
	p := ui.New(&bytes.Buffer{}, strings.NewReader("\n"), false, true)
	in := args{s: map[string]string{"server": "https://r.ex"}}
	_, err := NewLogin(t.TempDir()).(runtime.ReadCommand).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginUpgradesAutoPinToKeychain(t *testing.T) {
	// Non-parallel: keyring.MockInit swaps a process-global in-memory backend.
	// Isolate from the operator's shell: RANCHER_CLI_CREDENTIAL_STORE=file (this
	// project's own recommended hermetic-manual-run setting) would otherwise
	// outrank both the "auto" policy under test and the explicit "keychain"
	// argument to OpenStore below (config.EffectivePolicy: env > argument).
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	dir := t.TempDir() // no config on disk → auto policy; mock keychain is usable
	in := args{
		s: map[string]string{"server": "https://r.ex", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	// Inject a file store; under auto with a usable (mock) keychain, login must
	// re-pin to keychain and write the token there, not to the file store.
	_, err := newLoginCmd(dir, config.NewFileStore(dir)).
		Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	if got := config.ReadPin(dir); got != "keychain" {
		t.Fatalf("pin after auto login = %q, want keychain", got)
	}
	ks, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	tok, err := ks.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
	if _, ferr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme"}); !errors.Is(ferr, config.ErrNoToken) {
		t.Errorf("token must land in the keychain, not the file store; got %v", ferr)
	}
}

// TestLoginFirstPinNoticeReachesWarnings exercises the production wiring
// (newStore: config.OpenStoreWithNotice) on a fresh auto+usable-keychain
// machine: the first-pin notice must appear in res.Warnings so it reaches
// the JSON/YAML envelope, not just a raw stderr line. This covers the
// regression where openStoreResolved probed and pinned the keychain before
// RepinForLogin ran, making RepinForLogin return early with no notice.
func TestLoginFirstPinNoticeReachesWarnings(t *testing.T) {
	// Non-parallel: keyring.MockInit and keyringUsable are process-globals.
	// Isolate from the operator's shell so RANCHER_CLI_CREDENTIAL_STORE=file
	// doesn't outrank the "auto" policy under test.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)

	dir := t.TempDir() // fresh dir, no config → auto policy, no prior pin

	cmd := &loginCmd{
		dir:            dir,
		newStore:       config.OpenStoreWithNotice,
		rancherVersion: stubRancherVersion,
	}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "https://r.ex", "name": "acme"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "pinned credential store: keychain",
		"first-pin notice must reach res.Warnings on the production login path")
	assert.Equal(t, "keychain", config.ReadPin(dir), "auto + usable keychain must pin to keychain")
}

// TestLoginFileToKeychainRepinNoticeReachesWarnings exercises the production
// wiring (newStore: config.OpenStoreWithNotice) when the machine was previously
// pinned to "file" (e.g. first login over SSH) and now has a usable keychain:
// RepinForLogin must upgrade the pin and emit the re-pin notice into res.Warnings
// so it appears in the JSON/YAML envelope, not only as a raw stderr line.
func TestLoginFileToKeychainRepinNoticeReachesWarnings(t *testing.T) {
	// Non-parallel: keyring.MockInit and keyringUsable are process-globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)
	dir := t.TempDir()
	require.NoError(t, config.WritePin(dir, "file"))

	cmd := &loginCmd{
		dir:            dir,
		newStore:       config.OpenStoreWithNotice,
		rancherVersion: stubRancherVersion,
	}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "https://r.ex", "name": "acme"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "credential store re-pinned: file → keychain",
		"file→keychain re-pin notice must reach res.Warnings on the production login path")
	assert.Equal(t, "keychain", config.ReadPin(dir), "pin must be upgraded to keychain after re-probe")
}

// extLoginFakeOpts configures the httptest fake server standing in for
// Rancher across the EXT-R-8 provider-flow tests: the IdP dance
// (/v1-public/*) and the ext Token store mint/revoke, both reached ONLY
// through the /k8s/clusters/local proxy prefix — a bare /apis/... path is
// deliberately unregistered, so a proxy-path regression 404s here exactly as
// it would against a real Rancher.
type extLoginFakeOpts struct {
	sessionToken  string // the /v1-public/login response's "token" (S)
	mintNotFound  bool   // ext group absent: mint POST 404s
	revokeStatus  int    // revoke DELETE response status (0 -> 200)
	mintBodyCh    chan []byte
	revokedPathCh chan string
}

func newExtLoginFake(t *testing.T, opts extLoginFakeOpts) *httptest.Server {
	t.Helper()
	if opts.revokeStatus == 0 {
		opts.revokeStatus = http.StatusOK
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))

		case strings.HasSuffix(r.URL.Path, "/v1-public/login") && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": opts.sessionToken})

		case r.URL.Path == token.MintTokensPath && r.Method == http.MethodPost:
			if opts.mintBodyCh != nil {
				b, _ := io.ReadAll(r.Body)
				opts.mintBodyCh <- b
			}
			if opts.mintNotFound {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"ext/derived-1:secretvalue"}}`))

		case (strings.HasPrefix(r.URL.Path, v3TokensPath+"/") || strings.HasPrefix(r.URL.Path, token.MintTokensPath+"/")) && r.Method == http.MethodDelete:
			if opts.revokedPathCh != nil {
				opts.revokedPathCh <- r.URL.Path
			}
			w.WriteHeader(opts.revokeStatus)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginProviderFlowMintsAndStoresDerivedTokenRevokesSession(t *testing.T) {
	t.Parallel()
	revokedPathCh := make(chan string, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess1:sessionsecret", revokedPathCh: revokedPathCh})

	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "ext/derived-1:secretvalue", tok, "the derived token D is stored, not the session token S")

	select {
	case revokedPath := <-revokedPathCh:
		assert.Equal(t, v3TokensPath+"/token-sess1", revokedPath, "S is v3-shaped, revoked via the v3 Token CRD path")
	default:
		t.Fatal("the transient session token was never revoked")
	}
}

func TestLoginProviderFlowExtAbsentFallsBackToSessionToken(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess2:sessionsecret", mintNotFound: true})

	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider"}}
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "token-sess2:sessionsecret", tok, "no ext group: the session token itself is stored (legacy semantics)")
	assert.Contains(t, errBuf.String(), "does not serve ext.cattle.io")
}

func TestLoginProviderFlowRevokeFailureToleratedLoginStillSucceeds(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess3:sessionsecret", revokeStatus: http.StatusInternalServerError})

	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider"}}
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "ext/derived-1:secretvalue", tok, "a revoke failure must not block login")
	assert.Contains(t, errBuf.String(), "could not revoke the transient session token")
}

func TestLoginProviderFlowTTLPlumbedToMintBodyInMilliseconds(t *testing.T) {
	t.Parallel()
	bodyCh := make(chan []byte, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess4:sessionsecret", mintBodyCh: bodyCh})

	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider", "ttl": "12h"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.NoError(t, err)

	select {
	case b := <-bodyCh:
		var payload struct {
			Spec struct {
				TTL int64 `json:"ttl"`
			} `json:"spec"`
		}
		require.NoError(t, json.Unmarshal(b, &payload))
		assert.Equal(t, int64(12*60*60*1000), payload.Spec.TTL, "12h in milliseconds")
	default:
		t.Fatal("the mint request body was never captured")
	}
}

// TestLoginProviderFlowOmitsTTLFromMintBodyWhenAbsent pins mintDerivedToken's
// omit-when-unset idiom (fix #5) end to end: no --ttl must mean the mint
// request body carries no "ttl" key at all, not an explicit ttl: 0 — the
// unit tests in duration_test.go cover TTLMilliseconds itself, but not that
// login's own request-building actually omits the key.
func TestLoginProviderFlowOmitsTTLFromMintBodyWhenAbsent(t *testing.T) {
	t.Parallel()
	bodyCh := make(chan []byte, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess5:sessionsecret", mintBodyCh: bodyCh})

	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	require.NoError(t, err)

	select {
	case b := <-bodyCh:
		var payload map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(b, &payload))
		var spec map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(payload["spec"], &spec))
		_, hasTTL := spec["ttl"]
		assert.False(t, hasTTL, "no --ttl must omit spec.ttl entirely, not send ttl: 0")
	default:
		t.Fatal("the mint request body was never captured")
	}
}

// TestLoginProviderFlowSubSecondTTLIsValidation exercises the sub-unit
// truncation rejection (fix #6) through login's own flow, not just the
// shared TTLMilliseconds unit test: a --ttl that parses but rounds to 0ms
// must fail before any network call, as VALIDATION.
func TestLoginProviderFlowSubSecondTTLIsValidation(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{
		"server": "https://r.ex", "name": "prod", "auth-provider": "localProvider", "ttl": "500us",
	}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "a rejected ttl must never reach the mint/store step")
}

// newVersionProbeFake stands in for Rancher's unauthenticated /rancherversion
// endpoint. Any path other than /rancherversion 404s here, the same way a
// real server would reject an unrecognised root path.
func newVersionProbeFake(t *testing.T, value string, status int) *httptest.Server {
	t.Helper()
	if status == 0 {
		status = http.StatusOK
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rancherversion" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Version": value, "GitCommit": "abc123",
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginWarnsWhenServerBelowMinVersion(t *testing.T) {
	t.Parallel()
	fake := newVersionProbeFake(t, "v2.13.0", http.StatusOK)
	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "acme"}, b: map[string]bool{"token-stdin": true}}
	res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "below the supported minimum 2.14")
}

func TestLoginNoWarningAtOrAboveMinVersion(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"v2.14.0", "v2.15.2", "2.14"} {
		fake := newVersionProbeFake(t, v, http.StatusOK)
		dir, _ := filePolicyDir(t)
		in := args{s: map[string]string{"server": fake.URL, "name": "acme"}, b: map[string]bool{"token-stdin": true}}
		res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
		require.NoError(t, err, v)
		assert.Empty(t, res.Warnings, v)
	}
}

func TestLoginUnparseableVersionWarnsCannotDetermine(t *testing.T) {
	t.Parallel()
	fake := newVersionProbeFake(t, "dev", http.StatusOK)
	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "acme"}, b: map[string]bool{"token-stdin": true}}
	res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
}

// TestLoginVersionProbeFailureNeverFailsLogin pins PS-8's warn-only
// contract at the login seam: an RBAC-denied/missing-setting probe must
// never turn an otherwise-successful login into an error.
func TestLoginVersionProbeFailureNeverFailsLogin(t *testing.T) {
	t.Parallel()
	fake := newVersionProbeFake(t, "", http.StatusForbidden)
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "acme"}, b: map[string]bool{"token-stdin": true}}
	res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])

	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok, "the token is stored regardless of the version probe's outcome")
}

// TestLoginVersionProbeUnreachableServerNeverFailsLogin covers the
// connectivity-failure sub-case (no listener at all) of the same
// never-fails contract, distinct from an HTTP-level RBAC/404 response.
func TestLoginVersionProbeUnreachableServerNeverFailsLogin(t *testing.T) {
	t.Parallel()
	dir, _ := filePolicyDir(t)
	in := args{s: map[string]string{"server": "https://127.0.0.1:1", "name": "acme"}, b: map[string]bool{"token-stdin": true}}
	res, err := NewLogin(dir).(runtime.ReadCommand).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, config.UnknownServerVersionWarning, res.Warnings[0])
}

// TestLoginCanonicalizesStoredURL pins the live-exercise friction-6 finding:
// a non-canonical --server (host case, explicit default port) must be
// canonicalized ONCE at login and used consistently — the config.yaml server
// entry, the credential-store ref, and the success echo must all carry the
// canonical form the credential helper is keyed by, never the raw spelling.
func TestLoginCanonicalizesStoredURL(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"server": "HTTPS://Rancher2.Example.Local:443", "name": "canon"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	require.NoError(t, err)

	data := res.Data.(map[string]string)
	assert.Equal(t, "https://rancher2.example.local", data["url"], "echo must show the canonical form")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://rancher2.example.local", cfg.Servers["canon"].URL, "config must persist the canonical form")

	tok, err := store.GetToken(config.ServerRef{Name: "canon", URL: "https://rancher2.example.local"})
	require.NoError(t, err)
	assert.Equal(t, "id:secret", tok, "credential must be stored under the canonical ref")
}

// TestLoginResolvesStoreFromLockedConfig pins the review-r1-issue-2 race:
// a credentialStore policy change committed while login waits for the
// config lock must be honored — the token lands in the backend the NEW
// policy names, not the dependency-resolution snapshot's.
//
// Sequence: install a lock hook, start login (it parks on the hook),
// THEN rewrite config.yaml to the new policy, THEN release.  This proves
// the factory is called after the fn-load, not before the lock is
// acquired — a pre-lock snapshot would still see "file".
func TestLoginResolvesStoreFromLockedConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Hook fires inside WithLock, before fn. Login parks here so the config
	// still reads "file" at hook time. We rewrite to "exec:fake" then release:
	// fn loads the rewritten config and calls the factory with the new policy.
	loginParked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	restore := config.SetLockHookForTest(func() {
		once.Do(func() { close(loginParked) })
		<-release
	})
	defer restore()

	var calledWith string
	factory := func(policy, d string) (config.CredStore, string, error) {
		calledWith = policy
		return config.NewFileStore(d), "", nil
	}
	loginDone := make(chan error, 1)
	go func() {
		cmd := &loginCmd{dir: dir, newStore: factory, rancherVersion: stubRancherVersion}
		_, lerr := cmd.Execute(t.Context(),
			args{s: map[string]string{"server": "https://r.ex", "name": "acme"}, b: map[string]bool{"token-stdin": true}},
			stdinPrompter("id:sec\n"))
		loginDone <- lerr
	}()

	// Wait for login to park on the hook (config still "file" at this point),
	// then rewrite the policy before releasing.
	select {
	case <-loginParked:
	case <-time.After(10 * time.Second):
		t.Fatal("login never acquired the config lock")
	}
	newCfg := &config.Config{
		CredentialStore: "exec:fake",
		Servers:         map[string]config.Server{},
	}
	newCfg.SetPath(config.FilePath(dir))
	b, err := json.Marshal(newCfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.FilePath(dir), b, 0600))

	close(release)
	require.NoError(t, <-loginDone)
	assert.Equal(t, "exec:fake", calledWith,
		"factory must be called with the policy from the config loaded under the lock, not before")
}

// TestLoginSerializedUnderConfigLock exercises the boundary-test harness
// for auth login: two concurrent logins serialize correctly and the
// deciding load of the first login (which happens under the lock) sees a
// mutation committed while the lock was held.
func TestLoginSerializedUnderConfigLock(t *testing.T) {
	dir, _ := filePolicyDir(t)
	assertSerializedWithLoadUnderLock(t, dir,
		func() { runLogin(t, dir, "acme", "https://r.ex") },
		func() { writeSentinelServer(t, dir) },
		func() { runLogin(t, dir, "acme2", "https://r2.ex") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, sentinelOK := cfg.Servers["sentinel"]
	assert.True(t, sentinelOK, "sentinel written while the lock was held must survive the first login's write")
	_, acmeOK := cfg.Servers["acme"]
	assert.True(t, acmeOK, "first login's server must be present")
	_, acme2OK := cfg.Servers["acme2"]
	assert.True(t, acme2OK, "second login's server must be present")
}
