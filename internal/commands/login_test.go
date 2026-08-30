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

	"github.com/rancher/rancher-cli/internal/client/httpx"
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

// beforeReadPrompter wraps a Prompter and runs before once, immediately
// before the FIRST ReadLine call reaches the wrapped Prompter — the seam
// used to simulate a concurrent config change landing in the unlocked
// window between registration commit and the credential-write lock (a
// --token-stdin login reads its token via ReadLine).
type beforeReadPrompter struct {
	runtime.Prompter
	before func()
	fired  bool
}

func (p *beforeReadPrompter) ReadLine(ctx context.Context) (string, error) {
	if !p.fired {
		p.fired = true
		p.before()
	}
	return p.Prompter.ReadLine(ctx)
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

// seedServer writes name/url/ca directly into dir's config.yaml, bypassing
// registration entirely — for tests that need an already-registered entry
// without exercising add-server/delegated-login themselves. When current is
// true, name becomes the current-server pointer.
func seedServer(t *testing.T, dir, name, url, ca string, current bool) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[name] = config.Server{URL: url, CA: ca}
	if current {
		cfg.CurrentServer = name
	}
	require.NoError(t, cfg.Write())
}

// newPlainLoginCmd builds a loginCmd with the credential store fixed via a
// factory and tlsPreflight left nil (the documented inert default) —
// mirroring NewLogin's wiring for direct struct construction in tests that
// exercise an already-registered entry (Case B) and must never dial out.
func newPlainLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
	}
}

// TestLoginCompletesServerNames pins login's positional <name> completion
// (AGENTS.md: completion ships with the surface, in the same change as
// the value-taking argument).
func TestLoginCompletesServerNames(t *testing.T) {
	view := testView{srvNames: []string{"prod", "qa"}}
	assert.Equal(t, []string{"prod", "qa"}, loginCmd{}.CompleteArgs(context.Background(), view))
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
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:secret"))
	require.NoError(t, err, "read-only helper must not fail login")
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "read-only")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["prod"]
	assert.True(t, ok, "config written exactly as on success")
}

// TestLoginRejectsLegacyPrefixedName is the round-7 finding-3 repro: a
// "legacy:"-prefixed name must be rejected before any auth or config write —
// such a server entry would win the token shim's exact-name stored-login
// tier and defeat its documented server-ranking guarantee.
func TestLoginRejectsLegacyPrefixedName(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	_, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{config.LegacyCachePrefix + "alice"}},
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

// TestLoginOtherStoreErrorAborts pins the deliberate new behavior (What it
// costs): a failed credential write after a successful delegated
// registration leaves the freshly added entry behind — a valid first-class
// state, not rolled back.
func TestLoginOtherStoreErrorAborts(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return setErrStore{CredStore: store, setErr: errors.New("keychain exploded")}, "", nil
	}}
	_, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	require.Error(t, err)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["prod"]
	assert.True(t, ok, "registration is atomic and precedes auth; a failed credential write leaves it behind")
	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "no token persisted on a failed store write")
}

func TestLoginDelegatedAddCreatesEntryAndAuthenticates(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"url": "https://rancher.example.com"},
		b: map[string]bool{"token-stdin": true},
		a: []string{"acme"},
	}
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id-1:secret-1\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)
	data := res.Data.(map[string]any)
	assert.Equal(t, "acme", data["server"])
	assert.Equal(t, "https://rancher.example.com", data["url"])
	assert.Equal(t, "acme", data["currentServer"])
	assert.Equal(t, true, data["added"])

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
		s: map[string]string{"url": "https://rancher.example.com"},
		b: map[string]bool{"token-stdin": true},
		a: []string{"acme"},
	}
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("ext/token-abc:secretxyz\n"))
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
		dir, store := filePolicyDir(t)
		seedServer(t, dir, "prod", "https://r.ex", "", false)
		in := args{
			s: map[string]string{extra: "x"},
			b: map[string]bool{"token-stdin": true},
			a: []string{"prod"},
		}
		_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
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

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "azureADProvider", "auth-flow": "devicecode"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false) // no TTY, no --no-input
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.Error(t, err, "device-code will fail on OAuth config/network, but not on login's own gate")
	assert.NotContains(t, err.Error(), "token required", "non-TTY must bypass login's outer gate when --auth-provider is explicit")
}

func TestLoginTokenViaPrompt(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "acme", "https://r.ex", "", false)
	// isTTY=true, noInput=false -> CanPrompt. Two prompts now fire (EXT-R-8's
	// paste-or-provider menu, then the secret itself), so the input needs a
	// reader that doesn't lose the second line to Scanner over-read (see
	// lineReader's doc comment) — a plain strings.Reader would drop "id:sec".
	p := ui.New(&bytes.Buffer{}, newLineReader("1", "id:sec"), false, true)
	in := args{a: []string{"acme"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginNoNameNoURLIsValidationForCacert(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{s: map[string]string{"cacert": "x"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
}

func TestLoginUnknownNameNoURLIsNotFoundWithCandidates(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{a: []string{"ghost"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, ce.Message, "--url")
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "prod", ce.Candidates[0].Name)
}

// TestLoginCacertWithoutURLKnownNameIsValidationWithInterpolatedHint pins
// the three-way name split: a KNOWN name gets a fully interpolated
// update-server hint — trust changes belong to registration.
func TestLoginCacertWithoutURLKnownNameIsValidationWithInterpolatedHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"cacert": "/etc/ca.pem"}, a: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "rancher config update-server 'prod' --cacert '/etc/ca.pem'", ce.Hint)
}

// TestLoginCacertWithoutURLUnknownNameIsNotFound pins that the NOT_FOUND row
// wins over any --cacert-driven hint when the typed name is unknown — a
// suggested command must be valid verbatim, and update-server against an
// unknown name would deterministically fail.
func TestLoginCacertWithoutURLUnknownNameIsNotFound(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"cacert": "/etc/ca.pem"}, a: []string{"ghost"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Hint)
}

func TestLoginNoNameNoURLAuthenticatesCurrentServer(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{b: map[string]bool{"token-stdin": true}}, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginNoNameNoURLZeroEntriesIsNotLoggedIn(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Empty(t, ce.Hint, "the name and URL are unknown on a blank machine")
	assert.Contains(t, ce.Message, "--url")
}

// TestLoginNoNameNoURLOneEntryNoPointerNeverSelectsImplicitly pins that
// login never auto-selects the sole entry — a success there would
// authenticate a target the pointer still doesn't name, so the very next
// command would fail NOT_LOGGED_IN anyway. It fails with a paste-valid
// use-server hint instead.
func TestLoginNoNameNoURLOneEntryNoPointerNeverSelectsImplicitly(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Equal(t, "rancher config use-server 'prod'", ce.Hint)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "no auth flow ran")
}

func TestLoginNoNameNoURLManyEntriesNoPointerIsAmbiguous(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://prod.ex", "", false)
	seedServer(t, dir, "qa", "https://qa.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Empty(t, ce.Hint)
	require.Len(t, ce.Candidates, 2)
}

// TestLoginExplicitNameWithUnsetPointerSucceedsWithAdvisory pins that
// explicit-name login is a pure credential operation: it succeeds even with
// an unset pointer, and appends an operator advisory rather than silently
// leaving the operator to discover the next command still fails.
func TestLoginExplicitNameWithUnsetPointerSucceedsWithAdvisory(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, "", data["currentServer"], "currentServer is always present, even empty")
	assert.Contains(t, res.Text, "rancher config use-server 'prod'")

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

// TestLoginExplicitNameNeverSwitchesValidPointer: logging into a second,
// already-registered server must never move a VALID current-server pointer —
// login can never switch, only registration's bootstrap/repair rule can, and
// that never reaches an already-registered entry.
func TestLoginExplicitNameNeverSwitchesValidPointer(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://prod.ex", "", true)
	seedServer(t, dir, "qa", "https://qa.ex", "", false)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{b: map[string]bool{"token-stdin": true}, a: []string{"qa"}}, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	// A success logging into a registered-but-not-current server must not
	// look like a bare, unremarkable success: the pointer still names a
	// DIFFERENT server, so every next command keeps operating against
	// "prod" unless the operator switches — advise it plainly.
	assert.Contains(t, res.Text, "the current server remains 'prod'")
	assert.Contains(t, res.Text, "rancher config use-server 'qa'")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "prod", cfg.CurrentServer, "login never switches the pointer")
}

func TestLoginTokenRequiredNonInteractive(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{a: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyStdinToken(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStdinNonEOFErrorIsGeneric(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, errReader{}, true, false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestLoginBadTokenShape(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("notacolon\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyPromptToken(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	// TTY prompt returns a blank line -> "no token", not an id:secret shape error.
	p := ui.New(&bytes.Buffer{}, strings.NewReader("\n"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{a: []string{"prod"}}, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// --- delegated (--url) resolution matrix ---

// TestLoginDelegatedRejectsUnsupportedScheme: --token-stdin makes no
// network call at all, so nothing but the registration seam's own scheme
// gate stops an ftp:// (or any non-http(s)) URL from committing an entry
// and a token that config.Effective would then silently refuse to ever
// return — an apparently successful login that reads NOT_LOGGED_IN on the
// very next command.
func TestLoginDelegatedRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "ftp://host"}, b: map[string]bool{"token-stdin": true}, a: []string{"qa1"}},
		stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry written for an unsupported scheme")
	_, terr := store.GetToken(config.ServerRef{Name: "qa1"})
	assert.Error(t, terr, "no token stored either")
}

func TestLoginNoNameWithURLIsValidation(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), args{s: map[string]string{"url": "https://r.ex"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginDelegatedIdenticalURLIsNoOpAddAndAuthenticates(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["added"], "identical delegated add is a no-op, entry-scoped")
}

func TestLoginDelegatedIdenticalURLDifferentCacertConflicts(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex", "cacert": testCAPem1}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Hint, "update-server")
	assert.Contains(t, ce.Hint, "--cacert", "the URL matched — only the CA differed, so the hint must not suggest --url")
	assert.NotContains(t, ce.Hint, "--url")
}

// TestLoginDelegatedDifferentURLIsValidationBeforeAnyProbe pins that a known
// name whose registered URL disagrees with --url is refused immediately,
// with a code and wording distinct from the shared registration commit's own
// CONFLICT (TestCommitCreateExistingDifferentURLConflictsWithUpdateHint) —
// this check runs before any network call, so a name reused for the wrong
// server is never even dialed.
func TestLoginDelegatedDifferentURLIsValidationBeforeAnyProbe(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://old.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://new.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "server prod is registered at https://old.ex", ce.Message)
	assert.Equal(t, "rancher config update-server 'prod' --url 'https://new.ex'", ce.Hint)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "https://old.ex", cfg.Servers["prod"].URL, "the entry must never be re-pointed")
	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "sugar never mutates an existing entry; no auth ran")
}

// TestLoginDelegatedCanonicallyEqualURLDifferentSpellingFallsThrough pins
// that the hijack check compares CANONICAL identity, not literal spelling —
// re-login-with-add using a differently-cased/ported spelling of the same
// server stays the existing idempotent no-op, never the new VALIDATION.
func TestLoginDelegatedCanonicallyEqualURLDifferentSpellingFallsThrough(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "HTTPS://R.EX:443"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["added"])
}

// TestLoginDelegatedInvalidURLOnKnownNameFallsThroughToRegistrationError
// pins that the hijack check only acts on a URL it could canonicalize — a
// malformed --url on a known name still surfaces prepareRegistration's own
// invalid-URL error, not the new VALIDATION message.
func TestLoginDelegatedInvalidURLOnKnownNameFallsThroughToRegistrationError(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://old.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "not-a-url"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "invalid server URL")
	assert.NotContains(t, ce.Message, "is registered at")
}

func TestLoginDelegatedRejectsLegacyPrefixedName(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{config.LegacyCachePrefix + "x"}},
		stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginDelegatedBootstrapsPointerWhenEmpty(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "prod", res.Data.(map[string]any)["currentServer"])
}

func TestLoginDelegatedPreservesValidPointer(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "existing", "https://existing.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"new"}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "existing", res.Data.(map[string]any)["currentServer"], "a valid pointer is never repointed by delegated add")
}

// --- failed delegated auth: stale-hint recheck ---

func delegatedLoginCmdWithHook(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
}

func TestLoginDelegatedFailedAuthExactMatchGetsRetryHintAndUndo(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := delegatedLoginCmdWithHook(dir, store)
	// Empty stdin -> "no token provided", a plain VALIDATION auth failure,
	// with no concurrent change at all: the exact-match case.
	in := args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "rancher auth login 'prod'", ce.Hint)
	assert.Contains(t, ce.Message, "delete-server")

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	_, ok := cfg.Servers["prod"]
	assert.True(t, ok, "registration committed before the auth attempt")
}

func TestLoginDelegatedFailedAuthEntryDeletedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			delete(cfg.Servers, "prod")
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "the entry is gone; the retry command would immediately fail NOT_FOUND")
	assert.Contains(t, ce.Message, "config list-servers")
}

func TestLoginDelegatedFailedAuthURLRepointedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.Servers["prod"] = config.Server{URL: "https://different.ex"}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthSelectorChangedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.Servers["prod"] = config.Server{URL: "https://r.ex", CA: testCAPem1}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthSamePathBytesRotatedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := writeCAFile(t, testCAPem1)
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before: func() {
			require.NoError(t, os.WriteFile(caPath, []byte(testCAPem2), 0o600))
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex", "cacert": caPath}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "the same path selector now resolves to rotated bytes")
}

func TestLoginDelegatedFailedAuthPathUnreadableNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := writeCAFile(t, testCAPem1)
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before:   func() { require.NoError(t, os.Remove(caPath)) },
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex", "cacert": caPath}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthCancellationNoHint(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	p := &beforeReadPrompter{
		Prompter: stdinPrompter(""),
		before:   cancel,
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(ctx, in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "cancellation must skip the recheck entirely, never attach a hint")
}

// TestLoginDeleteBetweenRegisterAndStoreThenReregisterAtNewURLNeverSurfacesOldToken
// pins that a concurrent delete-then-recreate at a DIFFERENT URL during the
// unlocked auth window must never let the old token become live for the new
// address — the credential-write commit seam must conflict on the URL
// mismatch, never write under the stale name alone.
func TestLoginDeleteBetweenRegisterAndStoreThenReregisterAtNewURLNeverSurfacesOldToken(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa", URL: "https://a.example.com"}, "server-a-token"))

	p := &beforeReadPrompter{
		Prompter: stdinPrompter("new-token:secret\n"),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			delete(cfg.Servers, "qa")
			cfg.Servers["qa"] = config.Server{URL: "https://b.example.com"}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := args{s: map[string]string{"url": "https://a.example.com"}, b: map[string]bool{"token-stdin": true}, a: []string{"qa"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	tok, terr := store.GetToken(config.ServerRef{Name: "qa", URL: "https://b.example.com"})
	require.NoError(t, terr)
	assert.Equal(t, "server-a-token", tok, "the pre-existing token must be untouched, and no new token was written under it")
}

// --- authentication-snapshot conflicts against an EXISTING entry (plain login) ---

func TestLoginConflictWhenStoredCAChangesDuringPlainLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "-----BEGIN CERTIFICATE-----old", false)

	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(context.Context, string, string) (string, error) {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return "", err
			}
			cfg.Servers["prod"] = config.Server{URL: "https://r.ex", CA: "-----BEGIN CERTIFICATE-----swapped"}
			return "", cfg.Write()
		},
	}
	_, err := cmd.Execute(t.Context(), args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----swapped", cfg.Servers["prod"].CA, "the concurrent writer's CA is untouched")
	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "the conflicted login must not store a token")
}

func TestLoginConflictWhenEntryDeletedDuringPlainLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "-----BEGIN CERTIFICATE-----doomed", false)

	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(context.Context, string, string) (string, error) {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return "", err
			}
			delete(cfg.Servers, "prod")
			return "", cfg.Write()
		},
	}
	_, err := cmd.Execute(t.Context(), args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Servers, "prod", "the deleted entry must not be resurrected")
}

func TestLoginConflictWhenStoredCAFileContentRotatesDuringPlainLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := writeCAFile(t, testCAPem1)
	seedServer(t, dir, "prod", "https://r.ex", caPath, false)

	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(context.Context, string, string) (string, error) {
			return "", os.WriteFile(caPath, []byte(testCAPem2), 0o600)
		},
	}
	_, err := cmd.Execute(t.Context(), args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

// TestLoginPrecheckConfigLoadFailureAborts: a corrupt config must abort
// before any name resolution, never silently collapse to a blank state.
func TestLoginPrecheckConfigLoadFailureAborts(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("{not yaml"), 0o600))
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "reading config")
}

func TestLoginCanonicalizesStoredURL(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"url": "HTTPS://Rancher2.Example.Local:443"}, b: map[string]bool{"token-stdin": true}, a: []string{"canon"}},
		stdinPrompter("id:secret"))
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	assert.Equal(t, "https://rancher2.example.local", data["url"], "echo must show the canonical form")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://rancher2.example.local", cfg.Servers["canon"].URL, "config must persist the canonical form")

	tok, err := store.GetToken(config.ServerRef{Name: "canon", URL: "https://rancher2.example.local"})
	require.NoError(t, err)
	assert.Equal(t, "id:secret", tok, "credential must be stored under the canonical ref")
}

// TestLoginResolvesStoreFromLockedConfig pins a race: a credentialStore
// policy change committed while login waits for the config lock must be
// honored — the token lands in the backend the NEW policy names, not the
// dependency-resolution snapshot's.
func TestLoginResolvesStoreFromLockedConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{"acme": {URL: "https://r.ex"}},
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
		cmd := &loginCmd{dir: dir, newStore: factory}
		_, lerr := cmd.Execute(t.Context(),
			args{b: map[string]bool{"token-stdin": true}, a: []string{"acme"}},
			stdinPrompter("id:sec\n"))
		loginDone <- lerr
	}()

	select {
	case <-loginParked:
	case <-time.After(10 * time.Second):
		t.Fatal("login never acquired the config lock")
	}
	newCfg := &config.Config{
		CredentialStore: "exec:fake",
		Servers:         map[string]config.Server{"acme": {URL: "https://r.ex"}},
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

// TestLoginSerializedUnderConfigLock exercises the boundary-test harness for
// auth login: two concurrent delegated logins to distinct new names
// serialize correctly and the deciding load of the first login sees a
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
	mintBlock     chan struct{} // if set, the mint handler blocks reading this until closed
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
			if opts.mintBlock != nil {
				<-opts.mintBlock
			}
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
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
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
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "token-sess2:sessionsecret", tok, "no ext group: the session token itself is stored (legacy semantics)")
	assert.Contains(t, errBuf.String(), "does not serve ext.cattle.io")
}

// TestLoginProviderFlowMintTimeoutIsBoundedAndNotMistakenForExtAbsent
// asserts the ext token mint call is bounded by httpx's client-level
// timeout rather than hanging on an unresponsive server, and that a timeout
// surfaces as a plain error distinct from the 404 (ext group absent)
// fallback shape — the two must never collapse into the same behavior.
func TestLoginProviderFlowMintTimeoutIsBoundedAndNotMistakenForExtAbsent(t *testing.T) {
	// Not t.Parallel(): shrinks the package-global httpx.DefaultTimeout,
	// which would race other parallel subtests' real requests.
	restore := httpx.SetDefaultTimeoutForTest(20 * time.Millisecond)
	defer restore()

	block := make(chan struct{})
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess5:sessionsecret", mintBlock: block})
	defer close(block)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)

	start := time.Now()
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.NotErrorIs(t, err, token.ErrExtGroupAbsent, "a timeout must not be mistaken for the ext-group-absent fallback")
	assert.Less(t, elapsed, 5*time.Second, "the mint request must be bounded by the client timeout, not hang on the unresponsive handler")

	_, err = store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, err, "a failed mint must not leave a token stored")
}

func TestLoginProviderFlowRevokeFailureToleratedLoginStillSucceeds(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess3:sessionsecret", revokeStatus: http.StatusInternalServerError})

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
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

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider", "ttl": "12h"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
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

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
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
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := args{s: map[string]string{"auth-provider": "localProvider", "ttl": "500us"}, a: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "a rejected ttl must never reach the mint/store step")
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
		s: map[string]string{"url": "https://r.ex"},
		b: map[string]bool{"token-stdin": true},
		a: []string{"acme"},
	}
	// Inject a file store; under auto with a usable (mock) keychain, login must
	// re-pin to keychain and write the token there, not to the file store.
	_, err := newPlainLoginCmd(dir, config.NewFileStore(dir)).
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

	cmd := &loginCmd{dir: dir, newStore: config.OpenStoreWithNotice}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"acme"}},
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

	cmd := &loginCmd{dir: dir, newStore: config.OpenStoreWithNotice}
	res, err := cmd.Execute(t.Context(),
		args{s: map[string]string{"url": "https://r.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"acme"}},
		stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "credential store re-pinned: file → keychain",
		"file→keychain re-pin notice must reach res.Warnings on the production login path")
	assert.Equal(t, "keychain", config.ReadPin(dir), "pin must be upgraded to keychain after re-probe")
}
