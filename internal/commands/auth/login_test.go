package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/rancher/rancher-cli/internal/ui"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// lineReader yields one line at a time without buffering, so multiple
// sequential Prompt/PromptSecret calls (each creating a fresh bufio.Scanner
// per ui.Prompter.readLine) don't lose data to a Scanner's internal buffer -
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

// beforeReadPrompter wraps a Prompter and runs before once, immediately
// before the FIRST ReadLine call reaches the wrapped Prompter - the seam
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

// seedServer writes name/url/ca directly into dir's config.yaml, bypassing
// registration entirely - for tests that need an already-registered entry
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
// factory and tlsPreflight left nil (the documented inert default) -
// mirroring NewLogin's wiring for direct struct construction in tests that
// exercise an already-registered entry (Case B) and must never dial out.
func newPlainLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
	}
}

// TestLoginCompletesServerNames asserts login's positional <name> completion
// (AGENTS.md: completion ships with the surface, in the same change as
// the value-taking argument).
func TestLoginCompletesServerNames(t *testing.T) {
	view := cmdtest.View{Servers: []string{"prod", "qa"}}
	assert.Equal(t, []string{"prod", "qa"}, loginCmd{}.CompleteArgs(context.Background(), view))
}

func TestLoginReadOnlyHelperWarnsAndWritesConfig(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: store, SetErr: fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported)}, "", nil
	}}
	res, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:secret"))
	require.NoError(t, err, "read-only helper must not fail login")
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "read-only")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["prod"]
	assert.True(t, ok, "config written exactly as on success")
}

// TestLoginRejectsLegacyPrefixedName is the round-7 finding-3 repro: a
// "legacy:"-prefixed name must be rejected before any auth or config write -
// such a server entry would win the token shim's exact-name stored-login
// tier and defeat its documented server-ranking guarantee.
func TestLoginRejectsLegacyPrefixedName(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{config.LegacyCachePrefix + "alice"}},
		cmdtest.StdinPrompter("id:secret"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, "reserved")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers[config.LegacyCachePrefix+"alice"]
	assert.False(t, ok, "the reserved name must never reach the config")
}

// TestLoginOtherStoreErrorAborts asserts the deliberate new behavior (What it
// costs): a failed credential write after a successful delegated
// registration leaves the freshly added entry behind - a valid first-class
// state, not rolled back.
func TestLoginOtherStoreErrorAborts(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: store, SetErr: errors.New("keychain exploded")}, "", nil
	}}
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
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
	dir, store := cmdtest.FilePolicyDir(t)
	in := cmdtest.Input{
		Str:  map[string]string{"url": "https://rancher.example.com"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"acme"},
	}
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id-1:secret-1\n"))
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
	dir, store := cmdtest.FilePolicyDir(t)
	in := cmdtest.Input{
		Str:  map[string]string{"url": "https://rancher.example.com"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"acme"},
	}
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("ext/token-abc:secretxyz\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "ext/token-abc:secretxyz", tok)
}

// TestLoginTokenStdinRejectsProviderFlags asserts the fix for combining
// --token-stdin with any provider flag: the two paths are mutually
// exclusive, and silently ignoring the provider flags would surprise a
// caller who set --ttl expecting it to reach the derived token create request.
func TestLoginTokenStdinRejectsProviderFlags(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{"auth-provider", "auth-flow", "ttl"} {
		dir, store := cmdtest.FilePolicyDir(t)
		seedServer(t, dir, "prod", "https://r.ex", "", false)
		in := cmdtest.Input{
			Str:  map[string]string{extra: "x"},
			Flag: map[string]bool{"token-stdin": true},
			Pos:  []string{"prod"},
		}
		_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, extra)
		assert.Equal(t, clierr.CodeValidation, ce.Code, extra)
	}
}

// TestLoginProviderFlowNonTTYAllowedWithExplicitProvider mirrors get-token's
// own TestOAuthDeviceCodeFlowNonTTYAllowed: naming --auth-provider explicitly
// must reach the provider flow without a TTY - the finer-grained gates
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

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "azureADProvider", "auth-flow": "devicecode"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false) // no TTY, no --no-input
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.Error(t, err, "device-code will fail on OAuth config/network, but not on login's own gate")
	assert.NotContains(t, err.Error(), "token required", "non-TTY must bypass login's outer gate when --auth-provider is explicit")
}

func TestLoginTokenViaPrompt(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "acme", "https://r.ex", "", false)
	// isTTY=true, noInput=false -> CanPrompt. Two prompts now fire (EXT-R-8's
	// paste-or-provider menu, then the secret itself), so the input needs a
	// reader that doesn't lose the second line to Scanner over-read (see
	// lineReader's doc comment) - a plain strings.Reader would drop "id:sec".
	p := ui.New(&bytes.Buffer{}, newLineReader("1", "id:sec"), false, true)
	in := cmdtest.Input{Pos: []string{"acme"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginNoNameNoURLIsValidationForCacert(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": "x"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
}

func TestLoginUnknownNameNoURLIsNotFoundWithCandidates(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Pos: []string{"ghost"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, ce.Message, "--url")
	require.Len(t, ce.Candidates, 1)
	assert.Equal(t, "prod", ce.Candidates[0].Name)
}

// TestLoginCacertWithoutURLKnownNameIsValidationWithInterpolatedHint asserts
// the three-way name split: a KNOWN name gets a fully interpolated
// update-server hint - trust changes belong to registration.
func TestLoginCacertWithoutURLKnownNameIsValidationWithInterpolatedHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"cacert": "/etc/ca.pem"}, Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "rancher config update-server 'prod' --cacert '/etc/ca.pem'", ce.Hint)
}

// TestLoginCacertWithoutURLUnknownNameIsNotFound asserts that the NOT_FOUND row
// wins over any --cacert-driven hint when the typed name is unknown - a
// suggested command must be valid verbatim, and update-server against an
// unknown name would deterministically fail.
func TestLoginCacertWithoutURLUnknownNameIsNotFound(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"cacert": "/etc/ca.pem"}, Pos: []string{"ghost"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, ce.Hint)
}

func TestLoginNoNameNoURLAuthenticatesCurrentServer(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"token-stdin": true}}, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginNoNameNoURLZeroEntriesIsNotLoggedIn(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Empty(t, ce.Hint, "the name and URL are unknown on a blank machine")
	assert.Contains(t, ce.Message, "--url")
}

// TestLoginNoNameNoURLOneEntryNoPointerNeverSelectsImplicitly asserts that
// login never auto-selects the sole entry - a success there would
// authenticate a target the pointer still doesn't name, so the very next
// command would fail NOT_LOGGED_IN anyway. It fails with a paste-valid
// use-server hint instead.
func TestLoginNoNameNoURLOneEntryNoPointerNeverSelectsImplicitly(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
	assert.Equal(t, "rancher config use-server 'prod'", ce.Hint)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "no auth flow ran")
}

func TestLoginNoNameNoURLManyEntriesNoPointerIsAmbiguous(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://prod.ex", "", false)
	seedServer(t, dir, "qa", "https://qa.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Empty(t, ce.Hint)
	require.Len(t, ce.Candidates, 2)
}

// TestLoginExplicitNameWithUnsetPointerSucceedsWithAdvisory asserts that
// explicit-name login is a pure credential operation: it succeeds even with
// an unset pointer, and appends an operator advisory rather than silently
// leaving the operator to discover the next command still fails.
func TestLoginExplicitNameWithUnsetPointerSucceedsWithAdvisory(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, "", data["currentServer"], "currentServer is always present, even empty")
	assert.Contains(t, res.Text, "rancher config use-server 'prod'")

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

// TestLoginExplicitNameNeverSwitchesValidPointer: logging into a second,
// already-registered server must never move a VALID current-server pointer -
// login can never switch, only registration's bootstrap/repair rule can, and
// that never reaches an already-registered entry.
func TestLoginExplicitNameNeverSwitchesValidPointer(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://prod.ex", "", true)
	seedServer(t, dir, "qa", "https://qa.ex", "", false)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"qa"}}, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	// A success logging into a registered-but-not-current server must not
	// look like a bare, unremarkable success: the pointer still names a
	// DIFFERENT server, so every next command keeps operating against
	// "prod" unless the operator switches - advise it plainly.
	assert.Contains(t, res.Text, "the current server remains 'prod'")
	assert.Contains(t, res.Text, "rancher config use-server 'qa'")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "prod", cfg.CurrentServer, "login never switches the pointer")
}

func TestLoginTokenRequiredNonInteractive(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyStdinToken(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStdinNonEOFErrorIsGeneric(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, errReader{}, true, false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestLoginBadTokenShape(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("notacolon\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyPromptToken(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	// TTY prompt returns a blank line -> "no token", not an id:secret shape error.
	p := ui.New(&bytes.Buffer{}, strings.NewReader("\n"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// --- delegated (--url) resolution matrix ---

// TestLoginDelegatedRejectsUnsupportedScheme: --token-stdin makes no
// network call at all, so nothing but the registration seam's own scheme
// gate stops an ftp:// (or any non-http(s)) URL from committing an entry
// and a token that config.Effective would then silently refuse to ever
// return - an apparently successful login that reads NOT_LOGGED_IN on the
// very next command.
func TestLoginDelegatedRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "ftp://host"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"qa1"}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry written for an unsupported scheme")
	_, terr := store.GetToken(config.ServerRef{Name: "qa1"})
	assert.Error(t, terr, "no token stored either")
}

// TestLoginEmptyNameArgumentRefused asserts that an explicitly passed empty
// name (an unset shell variable, usually) is a validation error, never a
// silent fall-through to host derivation - and never an entry.
func TestLoginEmptyNameArgumentRefused(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://rancher.example.com"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{""}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "must not be empty")

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry registered for an empty name")

	_, err = newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Pos: []string{""}}, cmdtest.StdinPrompter(""))
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code, "the refusal fires without --url too")
}

// TestLoginUnsupportedSchemeEchoesRawURL asserts that the scheme refusal
// shows the URL as typed - canonicalization strips path and query, and a
// fat-fingered URL is easier to spot whole.
func TestLoginUnsupportedSchemeEchoesRawURL(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "ftp://rancher.example.com/some/path?x=1"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"qa1"}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ftp://rancher.example.com/some/path?x=1")
}

// TestLoginDerivedNameIPv6LiteralRefused asserts that a name is never
// derived from an IPv6 literal: Hostname() keeps its colons, and a
// colon-bearing entry name reads as a URL fragment everywhere a name is
// printed - the refusal names the explicit-name spelling instead.
func TestLoginDerivedNameIPv6LiteralRefused(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://[::1]:8443"}, Flag: map[string]bool{"token-stdin": true}, Pos: nil},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "IPv6")

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry registered under a derived IPv6 name")
}

// TestLoginDerivedNameCreatesEntryFromHost asserts that --url with no
// positional name registers and logs in under the canonical URL's host,
// naming that entry in both the envelope and the rendered text.
func TestLoginDerivedNameCreatesEntryFromHost(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://rancher.example.com"}, Flag: map[string]bool{"token-stdin": true}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, "rancher.example.com", data["server"])
	assert.Equal(t, true, data["added"])
	assert.Contains(t, res.Text, "rancher.example.com")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["rancher.example.com"]
	assert.True(t, ok)
}

// TestLoginDerivedNameSameURLProceedsAsKnownEntry asserts that a derived name
// matching an existing entry at the same canonical URL is the ordinary
// re-login-with-add no-op, not a new registration.
func TestLoginDerivedNameSameURLProceedsAsKnownEntry(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "r.ex", "https://r.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["added"])
}

// TestLoginDerivedNameCollisionDifferentSchemeRefusesBeforeAnyProbe asserts
// that a derived name already registered under a different canonical URL
// refuses pre-network, naming the taken entry and its URL, with no hint -
// an update-server hint would silently re-point an entry the caller never
// named, and no alternate name can be suggested with certainty.
func TestLoginDerivedNameCollisionDifferentSchemeRefusesBeforeAnyProbe(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "r.ex", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "http://r.ex"}, Flag: map[string]bool{"token-stdin": true}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "r.ex")
	assert.Contains(t, ce.Message, "derived")
	assert.Contains(t, ce.Message, "https://r.ex")
	assert.Empty(t, ce.Hint)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "https://r.ex", cfg.Servers["r.ex"].URL, "the existing entry must never be re-pointed")
}

// TestLoginDerivedNameSameHostDifferentPortRefusesBeforeAnyProbe asserts that
// a same-host, different-port URL derives the identical name (the port is
// never part of a derived name) and so hits the same collision refusal as
// any other different canonical URL.
func TestLoginDerivedNameSameHostDifferentPortRefusesBeforeAnyProbe(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "r.ex", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex:8443"}, Flag: map[string]bool{"token-stdin": true}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "r.ex")
	assert.Contains(t, ce.Message, "derived")
	assert.Empty(t, ce.Hint)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "https://r.ex", cfg.Servers["r.ex"].URL, "the existing entry must never be re-pointed")
}

// TestLoginDerivedNameInvalidURLFallsThroughToRegistrationError asserts that
// an unparseable --url with no positional name skips derivation entirely
// and surfaces register.Prepare's own invalid-URL error, not a name
// validation error.
func TestLoginDerivedNameInvalidURLFallsThroughToRegistrationError(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "not-a-url"}, Flag: map[string]bool{"token-stdin": true}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "invalid server URL")
}

func TestLoginDelegatedIdenticalURLIsNoOpAddAndAuthenticates(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["added"], "identical delegated add is a no-op, entry-scoped")
}

func TestLoginDelegatedIdenticalURLDifferentCacertConflicts(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex", "cacert": cmdtest.CAPem1}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Hint, "update-server")
	assert.Contains(t, ce.Hint, "--cacert", "the URL matched - only the CA differed, so the hint must not suggest --url")
	assert.NotContains(t, ce.Hint, "--url")
}

// TestLoginDelegatedDifferentURLIsValidationBeforeAnyProbe asserts that a known
// name whose registered URL disagrees with --url is refused immediately,
// with a code and wording distinct from the shared registration commit's own
// CONFLICT (TestCommitCreateExistingDifferentURLConflictsWithUpdateHint) -
// this check runs before any network call, so a name reused for the wrong
// server is never even dialed.
func TestLoginDelegatedDifferentURLIsValidationBeforeAnyProbe(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://old.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://new.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
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

// TestLoginDelegatedCanonicallyEqualURLDifferentSpellingFallsThrough asserts
// that the hijack check compares CANONICAL identity, not literal spelling -
// re-login-with-add using a differently-cased/ported spelling of the same
// server stays the existing idempotent no-op, never the new VALIDATION.
func TestLoginDelegatedCanonicallyEqualURLDifferentSpellingFallsThrough(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "HTTPS://R.EX:443"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["added"])
}

// TestLoginDelegatedInvalidURLOnKnownNameFallsThroughToRegistrationError
// asserts that the hijack check only acts on a URL it could canonicalize - a
// malformed --url on a known name still surfaces register.Prepare's own
// invalid-URL error, not the new VALIDATION message.
func TestLoginDelegatedInvalidURLOnKnownNameFallsThroughToRegistrationError(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://old.ex", "", false)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "not-a-url"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "invalid server URL")
	assert.NotContains(t, ce.Message, "is registered at")
}

func TestLoginDelegatedRejectsLegacyPrefixedName(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{config.LegacyCachePrefix + "x"}},
		cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginDelegatedBootstrapsPointerWhenEmpty(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "prod", res.Data.(map[string]any)["currentServer"])
}

func TestLoginDelegatedPreservesValidPointer(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "existing", "https://existing.ex", "", true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"new"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "existing", res.Data.(map[string]any)["currentServer"], "a valid pointer is never repointed by delegated add")
}

// --- failed delegated auth: stale-hint recheck ---

func delegatedLoginCmdWithHook(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
}

func TestLoginDelegatedFailedAuthExactMatchGetsRetryHintAndUndo(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := delegatedLoginCmdWithHook(dir, store)
	// Empty stdin -> "no token provided", a plain VALIDATION auth failure,
	// with no concurrent change at all: the exact-match case.
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, cmdtest.StdinPrompter(""))
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
	dir, store := cmdtest.FilePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			delete(cfg.Servers, "prod")
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "the entry is gone; the retry command would immediately fail NOT_FOUND")
	assert.Contains(t, ce.Message, "config list-servers")
}

func TestLoginDelegatedFailedAuthURLRepointedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.Servers["prod"] = config.Server{URL: "https://different.ex"}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthSelectorChangedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.Servers["prod"] = config.Server{URL: "https://r.ex", CA: cmdtest.CAPem1}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthSamePathBytesRotatedNoHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	caPath := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before: func() {
			require.NoError(t, os.WriteFile(caPath, []byte(cmdtest.CAPem2), 0o600))
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex", "cacert": caPath}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "the same path selector now resolves to rotated bytes")
}

func TestLoginDelegatedFailedAuthPathUnreadableNoHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	caPath := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before:   func() { require.NoError(t, os.Remove(caPath)) },
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex", "cacert": caPath}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint)
}

func TestLoginDelegatedFailedAuthCancellationNoHint(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter(""),
		before:   cancel,
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := cmd.Execute(ctx, in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, ce.Hint, "cancellation must skip the recheck entirely, never attach a hint")
}

// TestLoginDeleteBetweenRegisterAndStoreThenReregisterAtNewURLNeverSurfacesOldToken
// asserts that a concurrent delete-then-recreate at a DIFFERENT URL during the
// unlocked auth window must never let the old token become live for the new
// address - the credential-write commit seam must conflict on the URL
// mismatch, never write under the stale name alone.
func TestLoginDeleteBetweenRegisterAndStoreThenReregisterAtNewURLNeverSurfacesOldToken(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "qa", URL: "https://a.example.com"}, "server-a-token"))

	p := &beforeReadPrompter{
		Prompter: cmdtest.StdinPrompter("new-token:secret\n"),
		before: func() {
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			delete(cfg.Servers, "qa")
			cfg.Servers["qa"] = config.Server{URL: "https://b.example.com"}
			require.NoError(t, cfg.Write())
		},
	}
	cmd := delegatedLoginCmdWithHook(dir, store)
	in := cmdtest.Input{Str: map[string]string{"url": "https://a.example.com"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"qa"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	// The token stored under qa before any entry had the name is removed
	// when the login registers qa, and the conflicting login writes
	// nothing: neither the old token nor a new one is under the name.
	_, terr := store.GetToken(config.ServerRef{Name: "qa", URL: "https://b.example.com"})
	require.ErrorIs(t, terr, config.ErrNoToken, "the old token never becomes live for the new address, and no new token was written")
}

// --- authentication-snapshot conflicts against an EXISTING entry (plain login) ---

func TestLoginConflictWhenStoredCAChangesDuringPlainLogin(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
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
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}, cmdtest.StdinPrompter("id2:sec2\n"))
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
	dir, store := cmdtest.FilePolicyDir(t)
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
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}, cmdtest.StdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Servers, "prod", "the deleted entry must not be resurrected")
}

func TestLoginConflictWhenStoredCAFileContentRotatesDuringPlainLogin(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	caPath := cmdtest.WriteCAFile(t, cmdtest.CAPem1)
	seedServer(t, dir, "prod", "https://r.ex", caPath, false)

	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(context.Context, string, string) (string, error) {
			return "", os.WriteFile(caPath, []byte(cmdtest.CAPem2), 0o600)
		},
	}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}, cmdtest.StdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

// TestLoginPrecheckConfigLoadFailureAborts: a corrupt config must abort
// before any name resolution, never silently collapse to a blank state.
func TestLoginPrecheckConfigLoadFailureAborts(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("{not yaml"), 0o600))
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "reading config")
}

func TestLoginCanonicalizesStoredURL(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	res, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "HTTPS://Rancher2.Example.Local:443"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"canon"}},
		cmdtest.StdinPrompter("id:secret"))
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

// TestLoginRefusesAStoreChangedUnderTheLock asserts a race: a
// credentialStore change committed while the login waits for the config
// lock ends the login instead of redirecting it. The login resolved its
// store, and disclosed it, against the declaration it read before the
// sign-in; committing into a backend nobody told the user about - and
// recording a declaration this login never resolved against - is the
// wrong answer in both directions. Nothing is written and the message
// says to run the command again.
func TestLoginRefusesAStoreChangedUnderTheLock(t *testing.T) {
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
			cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
			cmdtest.StdinPrompter("id:sec\n"))
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
	lerr := <-loginDone
	require.Error(t, lerr)
	require.ErrorIs(t, lerr, config.ErrPersistAborted)
	require.ErrorContains(t, lerr, "exec:fake")
	require.ErrorContains(t, lerr, "run the command again")
	assert.Empty(t, calledWith, "no store is opened once the declaration has moved")

	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "exec:fake", after.CredentialStore, "the concurrent writer's declaration stands")
	tok, terr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme", URL: "https://r.ex"})
	require.ErrorIs(t, terr, config.ErrNoToken)
	assert.Empty(t, tok)
}

// TestLoginSerializedUnderConfigLock exercises the boundary-test harness for
// auth login: two concurrent delegated logins to distinct new names
// serialize correctly and the deciding load of the first login sees a
// mutation committed while the lock was held.
func TestLoginSerializedUnderConfigLock(t *testing.T) {
	dir, _ := cmdtest.FilePolicyDir(t)
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { runLogin(t, dir, "acme", "https://r.ex") },
		func() { cmdtest.WriteSentinelServer(t, dir) },
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
// (/v1-public/*) and the ext Token store create/revoke, both reached ONLY
// through the /k8s/clusters/local proxy prefix - a bare /apis/... path is
// deliberately unregistered, so a proxy-path regression 404s here exactly as
// it would against a real Rancher.
type extLoginFakeOpts struct {
	sessionToken          string // the /v1-public/login response's "token" (S)
	derivedCreateNotFound bool   // ext group absent: derived token create POST 404s
	revokeStatus          int    // revoke DELETE response status (0 -> 200)
	createBodyCh          chan []byte
	revokedPathCh         chan string
	createBlock           chan struct{} // if set, the derived token create handler blocks reading this until closed
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
			id, _, _ := strings.Cut(opts.sessionToken, ":")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "token": opts.sessionToken})

		case r.URL.Path == token.ExtTokensPath && r.Method == http.MethodPost:
			if opts.createBlock != nil {
				<-opts.createBlock
			}
			if opts.createBodyCh != nil {
				b, _ := io.ReadAll(r.Body)
				opts.createBodyCh <- b
			}
			if opts.derivedCreateNotFound {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"derived-1"},"status":{"bearerToken":"ext/derived-1:secretvalue"}}`))

		case (strings.HasPrefix(r.URL.Path, token.V3TokensPath+"/") || strings.HasPrefix(r.URL.Path, token.ExtTokensPath+"/")) && r.Method == http.MethodDelete:
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

func TestLoginProviderFlowCreatesAndStoresDerivedTokenRevokesSession(t *testing.T) {
	t.Parallel()
	revokedPathCh := make(chan string, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess1:sessionsecret", revokedPathCh: revokedPathCh})

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "ext/derived-1:secretvalue", tok, "the derived token D is stored, not the session token S")

	select {
	case revokedPath := <-revokedPathCh:
		assert.Equal(t, token.V3TokensPath+"/token-sess1", revokedPath, "S is v3-shaped, revoked via the v3 Token CRD path")
	default:
		t.Fatal("the transient session token was never revoked")
	}
}

func TestLoginProviderFlowExtAbsentFallsBackToSessionToken(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess2:sessionsecret", derivedCreateNotFound: true})

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	var errBuf bytes.Buffer
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "token-sess2:sessionsecret", tok, "no ext group: the session token itself is stored (legacy semantics)")
	assert.Contains(t, errBuf.String(), "does not serve ext.cattle.io")
}

// TestLoginProviderFlowDerivedCreateTimeoutIsBoundedAndNotMistakenForExtAbsent
// asserts the ext token create call is bounded by httpx's client-level
// timeout rather than hanging on an unresponsive server, and that a timeout
// surfaces as a plain error distinct from the 404 (ext group absent)
// fallback shape - the two must never collapse into the same behavior.
func TestLoginProviderFlowDerivedCreateTimeoutIsBoundedAndNotMistakenForExtAbsent(t *testing.T) {
	// Not t.Parallel(): shrinks the package-global httpx.DefaultTimeout,
	// which would race other parallel subtests' real requests.
	restore := httpx.SetDefaultTimeoutForTest(20 * time.Millisecond)
	defer restore()

	block := make(chan struct{})
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess5:sessionsecret", createBlock: block})
	defer close(block)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)

	start := time.Now()
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.NotErrorIs(t, err, token.ErrExtGroupAbsent, "a timeout must not be mistaken for the ext-group-absent fallback")
	assert.Less(t, elapsed, 5*time.Second, "the derived token create request must be bounded by the client timeout, not hang on the unresponsive handler")

	_, err = store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, err, "a failed create must not leave a token stored")
}

func TestLoginProviderFlowRevokeFailureToleratedLoginStillSucceeds(t *testing.T) {
	t.Parallel()
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess3:sessionsecret", revokeStatus: http.StatusInternalServerError})

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "ext/derived-1:secretvalue", tok, "a revoke failure must not block login")
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "could not be revoked")
	assert.Len(t, res.Data.(map[string]any)["residualTokens"], 1)
}

func TestLoginProviderFlowTTLPlumbedToCreateBodyInMilliseconds(t *testing.T) {
	t.Parallel()
	bodyCh := make(chan []byte, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess4:sessionsecret", createBodyCh: bodyCh})

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider", "ttl": "12h"}, Pos: []string{"prod"}}
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
		t.Fatal("the derived token create request body was never captured")
	}
}

// TestLoginProviderFlowOmitsTTLFromCreateBodyWhenAbsent asserts createDerivedToken's
// omit-when-unset idiom (fix #5) end to end: no --ttl must mean the derived token create
// request body carries no "ttl" key at all, not an explicit ttl: 0 - the
// unit tests in duration_test.go cover TTLMilliseconds itself, but not that
// login's own request-building actually omits the key.
func TestLoginProviderFlowOmitsTTLFromCreateBodyWhenAbsent(t *testing.T) {
	t.Parallel()
	bodyCh := make(chan []byte, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess5:sessionsecret", createBodyCh: bodyCh})

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
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
		t.Fatal("the derived token create request body was never captured")
	}
}

// TestLoginProviderFlowSubSecondTTLIsValidation exercises the sub-unit
// truncation rejection (fix #6) through login's own flow, not just the
// shared TTLMilliseconds unit test: a --ttl that parses but rounds to 0ms
// must fail before any network call, as VALIDATION.
func TestLoginProviderFlowSubSecondTTLIsValidation(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider", "ttl": "500us"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "a rejected ttl must never reach the derived token create/store step")
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
	in := cmdtest.Input{
		Str:  map[string]string{"url": "https://r.ex"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"acme"},
	}
	// The production opener, because the backend the login resolves is what
	// decides here: under auto with a usable (mock) keychain the credential
	// goes to the keychain, and the pin that says so is written by the
	// change's own last step, after the commit.
	cmd := newPlainLoginCmd(dir, nil)
	cmd.newStore = config.OpenResolvedStore
	_, err := cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
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
// (newStore: config.OpenResolvedStore) on a fresh auto+usable-keychain
// machine: the moment this machine decides where the directory keeps its
// credentials must appear in res.Warnings so it reaches the JSON/YAML
// envelope, not just a raw stderr line. The notice comes from the pin
// step of the change the login records, which is the only thing that
// writes that pin.
func TestLoginFirstPinNoticeReachesWarnings(t *testing.T) {
	// Non-parallel: keyring.MockInit and keyringUsable are process-globals.
	// Isolate from the operator's shell so RANCHER_CLI_CREDENTIAL_STORE=file
	// doesn't outrank the "auto" policy under test.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)

	dir := t.TempDir() // fresh dir, no config → auto policy, no prior pin

	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	res, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "pinned credential store: keychain",
		"first-pin notice must reach res.Warnings on the production login path")
	assert.Equal(t, "keychain", config.ReadPin(dir), "auto + usable keychain must pin to keychain")
}

// TestLoginFileToKeychainRepinNoticeReachesWarnings exercises the
// production wiring (newStore: config.OpenResolvedStore) when the machine
// was previously pinned to "file" (a first login over SSH) and now has a
// usable keychain: the login must upgrade the pin and the re-pin notice
// must reach res.Warnings, so it appears in the JSON/YAML envelope and
// not only as a raw stderr line.
func TestLoginFileToKeychainRepinNoticeReachesWarnings(t *testing.T) {
	// Non-parallel: keyring.MockInit and keyringUsable are process-globals.
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)
	dir := t.TempDir()
	require.NoError(t, config.WritePin(dir, "file"))

	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	res, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "credential store re-pinned: file → keychain",
		"file→keychain re-pin notice must reach res.Warnings on the production login path")
	assert.Equal(t, "keychain", config.ReadPin(dir), "pin must be upgraded to keychain after re-probe")
}

// TestLoginProviderFlowDeviceCodeAcrossSplitOrigins asserts the end-to-end
// topology through the login command: Rancher on one TLS origin with its CA
// stored on the entry, the identity provider on another TLS origin with its
// own certificate. The command's identity-provider client is injected
// through the same seam production wires to httpx.NewExternal, pinned here
// to the IdP fixture because a unit test cannot change the process trust
// store; the OS-root proof is the e2e subprocess.
func TestLoginProviderFlowDeviceCodeAcrossSplitOrigins(t *testing.T) {
	t.Parallel()
	var facade *testrancher.Facade
	idp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		facade.IdPHandler().ServeHTTP(w, r)
	}))
	idpCert, idpCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	idp.TLS = &tls.Config{Certificates: []tls.Certificate{idpCert}}
	idp.StartTLS()
	t.Cleanup(idp.Close)
	facade = testrancher.New(testrancher.Options{EnableIdP: true, IdPURL: idp.URL, IdPPendingPolls: 1})
	rancher := httptest.NewTLSServer(facade)
	t.Cleanup(rancher.Close)

	idpClient, err := httpx.New(httpx.Options{CACert: idpCA, Timeout: httpx.DefaultTimeout})
	require.NoError(t, err)

	dir, store := cmdtest.FilePolicyDir(t)
	rancherCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rancher.Certificate().Raw}))
	seedServer(t, dir, "prod", rancher.URL, rancherCA, false)
	cmd := newPlainLoginCmd(dir, store)
	cmd.newIdPClient = func() *http.Client { return idpClient }
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "azureADProvider", "auth-flow": "devicecode"}, Pos: []string{"prod"}}
	_, err = cmd.Execute(t.Context(), in, ui.New(io.Discard, strings.NewReader(""), false, false))
	require.NoError(t, err)
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
}

// TestLoginProviderFlowRancherClientNeverReachesIdP asserts the seam's
// purpose: with the Rancher-pinned client injected as the IdP client, the
// flow fails as an identity-provider certificate error, so the command
// cannot silently reuse the Rancher client.
func TestLoginProviderFlowRancherClientNeverReachesIdP(t *testing.T) {
	t.Parallel()
	var facade *testrancher.Facade
	idp := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		facade.IdPHandler().ServeHTTP(w, r)
	}))
	idpCert, _, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	idp.TLS = &tls.Config{Certificates: []tls.Certificate{idpCert}}
	idp.StartTLS()
	t.Cleanup(idp.Close)
	facade = testrancher.New(testrancher.Options{EnableIdP: true, IdPURL: idp.URL, IdPPendingPolls: 1})
	rancher := httptest.NewTLSServer(facade)
	t.Cleanup(rancher.Close)

	dir, store := cmdtest.FilePolicyDir(t)
	rancherCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rancher.Certificate().Raw}))
	seedServer(t, dir, "prod", rancher.URL, rancherCA, false)
	cmd := newPlainLoginCmd(dir, store)
	rancherPinned, err := httpx.New(httpx.Options{CACert: rancherCA, Timeout: httpx.DefaultTimeout})
	require.NoError(t, err)
	cmd.newIdPClient = func() *http.Client { return rancherPinned }
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "azureADProvider", "auth-flow": "devicecode"}, Pos: []string{"prod"}}
	_, err = cmd.Execute(t.Context(), in, ui.New(io.Discard, strings.NewReader(""), false, false))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	require.ErrorContains(t, err, "identity provider 127.0.0.1: certificate not trusted")
}

// TestLoginProviderFlowRefusesCrossOriginRedirectOnLogin asserts that a
// server answering the login POST with a cross-origin 307 is refused before
// the credential-bearing body reaches the other origin.
func TestLoginProviderFlowRefusesCrossOriginRedirectOnLogin(t *testing.T) {
	t.Parallel()
	var elsewhereHits atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhereHits.Add(1) }))
	t.Cleanup(elsewhere.Close)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1-public/authproviders":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
		case "/v1-public/login":
			http.Redirect(w, r, elsewhere.URL+"/v1-public/login", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	require.ErrorContains(t, err, "refused to follow a redirect from "+fake.URL+" to "+elsewhere.URL)
	assert.Zero(t, elsewhereHits.Load(), "the password-bearing body must never reach the other origin")
}

// unsupportedStore is a read-only credential helper: every write reports
// notWritten with the sentinel, every read reports no token.
type unsupportedStore struct{}

func (unsupportedStore) GetToken(config.ServerRef) (string, error) { return "", config.ErrNoToken }

func (s unsupportedStore) SetToken(ref config.ServerRef, tok string) error {
	_, err := s.SetTokenWithOutcome(ref, tok, config.PriorCredential{})
	return err
}

func (unsupportedStore) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	return config.CommitOutcome{Backend: clierr.BackendExec, State: config.CommitNotWritten, PriorSource: clierr.PriorUnknown},
		fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported)
}

func (unsupportedStore) DeleteToken(config.ServerRef) error { return nil }

func (unsupportedStore) GetCachedCred(string, *config.CacheAnchor) (string, time.Time, error) {
	return "", time.Time{}, config.ErrNoToken
}

func (unsupportedStore) GetCachedCredDetail(string, *config.CacheAnchor) (config.CachedCredDetail, error) {
	return config.CachedCredDetail{}, config.ErrNoToken
}
func (unsupportedStore) MarkCachedCredReady(string, string, string) (bool, error) { return false, nil }
func (unsupportedStore) SetCachedCred(string, string, time.Time, string, string, string, config.CacheBinding) error {
	return nil
}
func (unsupportedStore) DeleteCachedCred(string) error                     { return nil }
func (unsupportedStore) DeleteAllCachedCreds() error                       { return nil }
func (unsupportedStore) ListCachedCreds() ([]config.CachedCredInfo, error) { return nil, nil }
func (unsupportedStore) ResolvedBackend() (string, bool)                   { return "exec:unsupported", true }

// commitThenErrorKeyring is a keyring seam whose Set stores the value and
// then reports an error: the write committed, the transport did not say so.
type commitThenErrorKeyring struct {
	mu    sync.Mutex
	items map[string]string
	err   error
}

func (k *commitThenErrorKeyring) Set(service, user, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"\x00"+user] = password
	return k.err
}

func (k *commitThenErrorKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *commitThenErrorKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"\x00"+user)
	return nil
}

// depositingPrompter stands in for the browser on the SAML path: it reads
// the published login URL and runs onHandoff before the CLI's first poll.
// The embedded prompter is silent and non-TTY with NoInput false - the SAML
// gate refuses a NoInput prompter before any handoff.
type depositingPrompter struct {
	runtime.Prompter
	onHandoff func(requestID, publicKey string)
}

func (h *depositingPrompter) Notice(msg string) {
	before, _, found := strings.Cut(msg, "/dashboard/auth/login?")
	if !found {
		return
	}
	u, err := neturl.Parse(strings.TrimSpace(msg[strings.LastIndex(before, " ")+1:]))
	if err != nil {
		return
	}
	if h.onHandoff != nil {
		h.onHandoff(u.Query().Get("requestId"), u.Query().Get("publicKey"))
	}
}

func newDepositingPrompter(onHandoff func(requestID, publicKey string)) *depositingPrompter {
	return &depositingPrompter{Prompter: ui.New(io.Discard, strings.NewReader(""), false, false), onHandoff: onHandoff}
}

func countOf(lines []string, s string) int {
	n := 0
	for _, l := range lines {
		if l == s {
			n++
		}
	}
	return n
}

func indexOf(lines []string, s string) int {
	for i, l := range lines {
		if l == s {
			return i
		}
	}
	return -1
}

// decodeLoginFailure decodes the details every post-credential failure
// must carry.
func decodeLoginFailure(t *testing.T, err error) (*clierr.Error, clierr.LoginFailureDetails) {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok, "details %T", ce.Details)
	return ce, d
}

// TestLoginConflictAfterDerivedTokenRevokesAndReports asserts a commit-stage
// failure on a derived credential: the derived token is revoked with
// itself as the bearer, the details name both tokens' fates, and the
// primary's code and hint survive composition.
func TestLoginConflictAfterDerivedTokenRevokesAndReports(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := newPlainLoginCmd(dir, store)
	cmd.beforeCommitHook = func() {
		cfg, _ := config.Load(config.FilePath(dir))
		cfg.Servers["prod"] = config.Server{URL: srv.URL + "/changed"}
		_ = cfg.Write()
	}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err := cmd.Execute(t.Context(), in, p)
	ce, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, clierr.CodeConflict, d.Primary.Code)
	assert.Equal(t, "rancher auth login "+clierr.ShellQuote("prod"), ce.Hint)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
	assert.Equal(t, clierr.CredentialNotWritten, d.CredentialState)
	assert.Nil(t, d.CredentialRecovery, "nothing local can be left behind")
	require.NotNil(t, d.DerivedToken)
	assert.Equal(t, clierr.CreateKnown, d.DerivedToken.Outcome)
	require.NotNil(t, d.DerivedToken.Cleanup)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)
	require.NotNil(t, d.SessionToken)
	assert.Equal(t, clierr.ResultRevoked, d.SessionToken.Cleanup.Result)
	recs := f.DeleteRequests()
	require.Len(t, recs, 2, "S revoked during acquire, D revoked at commit failure")
	assert.Equal(t, "ext", recs[1].Kind)
	_, storedErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.ErrorIs(t, storedErr, config.ErrNoToken)
}

// TestLoginPastedTokenFailureNeverRevokes asserts that a user-supplied token
// is never deleted, and the details say so.
func TestLoginPastedTokenFailureNeverRevokes(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := newPlainLoginCmd(dir, store)
	cmd.beforeCommitHook = func() {
		cfg, _ := config.Load(config.FilePath(dir))
		cfg.Servers["prod"] = config.Server{URL: srv.URL + "/changed"}
		_ = cfg.Write()
	}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true, "no-verify": true}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("id:secret"), false, true)
	_, err := cmd.Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Nil(t, d.DerivedToken)
	assert.Nil(t, d.SessionToken)
	assert.Empty(t, d.OperationMarker)
	assert.Empty(t, f.DeleteRequests())
}

// TestLoginReadOnlyHelperRevokesDerivedToken asserts the unsupported-store
// branch: the login succeeds, but the derived token created for it is
// revoked, and a failed revoke is reported as residual with no executable
// hint because nothing was stored.
func TestLoginReadOnlyHelperRevokesDerivedToken(t *testing.T) {
	t.Parallel()
	for _, deleteMode := range []string{"", "fail"} {
		t.Run("delete "+deleteMode, func(t *testing.T) {
			f := testrancher.New(testrancher.Options{DerivedDeleteMode: deleteMode})
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, "", false)
			cmd := newPlainLoginCmd(dir, unsupportedStore{})
			in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
			p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
			res, err := cmd.Execute(t.Context(), in, p)
			require.NoError(t, err)
			recs := f.DeleteRequests()
			require.Len(t, recs, 2)
			assert.Equal(t, "ext", recs[1].Kind, "D is revoked, not stored")
			residual, _ := res.Data.(map[string]any)["residualTokens"].([]clierr.TokenFate)
			if deleteMode == "fail" {
				require.Len(t, residual, 1)
				assert.Equal(t, clierr.ResultFailed, residual[0].Cleanup.Result)
				joined := strings.Join(res.Warnings, "\n")
				assert.Contains(t, joined, "no credential was stored")
				_, revoke := clierr.RecoveryCommands(*residual[0].Ref)
				assert.Contains(t, joined, revoke, "the exact ext delete command follows the re-login")
			} else {
				assert.Empty(t, residual)
			}
		})
	}
}

// TestLoginSuccessWithFailedSessionCleanupWarns asserts the successful login
// whose S revoke failed: the login succeeds, the warning names the v3 token
// with the deprecated-kind delete command, and residualTokens carries it.
func TestLoginSuccessWithFailedSessionCleanupWarns(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{SessionDeleteMode: "fail"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	joined := strings.Join(res.Warnings, "\n")
	assert.Contains(t, joined, "rancher delete management-token ")
	assert.Contains(t, joined, "--apply --yes --allow-deprecated")
	residual := res.Data.(map[string]any)["residualTokens"].([]clierr.TokenFate)
	require.Len(t, residual, 1)
	assert.Equal(t, clierr.TokenKindV3, residual[0].Ref.Kind, "the facade answers a json request with the Norman shape by default")
}

// TestLoginReadOnlyHelperRecoveryMatrix asserts finishUnsupportedStore's
// aggregate rule across credential shapes. Every residual here has a known
// name (S's name arrives with its bearer, D's with the derived token create response), so
// the recovery is re-login followed by the exact kind-correct command, with
// no hint because nothing was persisted; marker search and handoff facts
// are never rendered for a named token, though a SAML fate still carries
// its search data. Not parallel: the SAML cases shrink the token package's
// poll timing.
func TestLoginReadOnlyHelperRecoveryMatrix(t *testing.T) {
	restore := token.SetSAMLTimingForTest(2*time.Second, 10*time.Millisecond)
	defer restore()
	cases := []struct {
		name         string
		opts         testrancher.Options
		saml         bool
		wantResidual int
		wantKinds    []clierr.TokenKind
	}{
		{"derived, S cleanup failed", testrancher.Options{SessionDeleteMode: "fail"}, false, 1, []clierr.TokenKind{clierr.TokenKindV3}},
		{"derived, D revoke failed", testrancher.Options{DerivedDeleteMode: "fail"}, false, 1, []clierr.TokenKind{clierr.TokenKindExt}},
		{"derived, both failed", testrancher.Options{SessionDeleteMode: "fail", DerivedDeleteMode: "fail"}, false, 2, []clierr.TokenKind{clierr.TokenKindV3, clierr.TokenKindExt}},
		{"SAML derived, S cleanup failed", testrancher.Options{SessionDeleteMode: "fail"}, true, 1, []clierr.TokenKind{clierr.TokenKindV3}},
		{"SAML legacy, S revoke failed", testrancher.Options{Legacy: true, SessionDeleteMode: "fail"}, true, 1, []clierr.TokenKind{clierr.TokenKindV3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			var f *testrancher.Facade
			var p runtime.Prompter
			if tc.saml {
				opts.EnableSAMLAuthTokens = true
				opts.ExtraProviders = []string{"oktaProvider"}
				f = testrancher.New(opts)
				p = newDepositingPrompter(func(id, pub string) { f.DepositAuthToken(id, pub) })
			} else {
				f = testrancher.New(opts)
				p = ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
			}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, "", false)
			provider := "localProvider"
			if tc.saml {
				provider = "oktaProvider"
			}
			in := cmdtest.Input{Str: map[string]string{"auth-provider": provider}, Pos: []string{"prod"}}
			res, err := newPlainLoginCmd(dir, unsupportedStore{}).Execute(t.Context(), in, p)
			require.NoError(t, err)
			residual, _ := res.Data.(map[string]any)["residualTokens"].([]clierr.TokenFate)
			require.Len(t, residual, tc.wantResidual)
			for i, r := range residual {
				require.NotNil(t, r.Ref)
				assert.Equal(t, tc.wantKinds[i], r.Ref.Kind)
				assert.Equal(t, clierr.ResultFailed, r.Cleanup.Result)
				assert.Equal(t, !tc.saml || r.Ref.Kind == clierr.TokenKindExt, r.Marked)
				if tc.saml && r.Ref.Kind == clierr.TokenKindV3 {
					require.NotNil(t, r.Search)
					assert.Equal(t, "oktaProvider", r.Search.AuthProvider)
				}
			}
			joined := strings.Join(res.Warnings, "\n")
			assert.Contains(t, joined, "no credential was stored")
			readOnly := strings.Index(joined, "credential helper is read-only")
			firstResidual := strings.Index(joined, "could not be revoked")
			require.GreaterOrEqual(t, readOnly, 0)
			require.GreaterOrEqual(t, firstResidual, 0)
			assert.Less(t, readOnly, firstResidual, "why nothing was persisted is read before what to clean up")
			// Every residual here has a known name, so the prose is the exact
			// command after a re-login; marker search and handoff facts are
			// for tokens without a name and must not appear.
			assert.NotContains(t, joined, "[op:")
			assert.NotContains(t, joined, "at or after")
			login := strings.Index(joined, "rancher auth login "+clierr.ShellQuote("prod"))
			require.GreaterOrEqual(t, login, 0)
			for _, r := range residual {
				_, revoke := clierr.RecoveryCommands(*r.Ref)
				at := strings.Index(joined, revoke)
				require.GreaterOrEqual(t, at, 0, "a known name gets its exact revoke command in the prose: %s", revoke)
				assert.Less(t, login, at, "re-login is ordered before the delete")
				if r.Ref.Kind == clierr.TokenKindV3 {
					assert.Contains(t, revoke, "--allow-deprecated")
				}
			}
		})
	}
}

// TestLoginTLSFailureKeepsCertificateFactsInText asserts the default-format
// projection of a composed failure: the TLS classification's own detail
// lines are rendered before the login stage and cleanup lines, once. The
// classified primary is produced by a real x509 failure against a
// self-signed server and handed to token.FailLogin directly: in the login
// flow an x509 failure happens before any create and never reaches the
// schema, so the projection is a property of composition, tested at that
// level.
func TestLoginTLSFailureKeepsCertificateFactsInText(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)
	_, err := http.Get(srv.URL + "/rancherversion") // the default client does not trust the fixture
	require.Error(t, err)
	u, perr := neturl.Parse(srv.URL)
	require.NoError(t, perr)
	origin, perr := httpx.OriginOf(u)
	require.NoError(t, perr)
	primary := tlstrust.ClassifyForOrigin(err, origin, time.Now())
	require.NotNil(t, primary)
	inner, ok := primary.Details.(clierr.TextDetails)
	require.True(t, ok, "the TLS classification carries text details: %T", primary.Details)

	composed := token.FailLogin(primary, clierr.StageCommit, token.LoginStates{}, token.UserSupplied("id:secret"), "prod")
	_, d := decodeLoginFailure(t, composed)
	lines := d.TextDetails()
	for _, want := range inner.TextDetails() {
		assert.Equal(t, 1, countOf(lines, want), "certificate fact %q rendered exactly once", want)
	}
	assert.Less(t, indexOf(lines, inner.TextDetails()[0]), indexOf(lines, "stage: "+string(d.Stage)), "certificate facts precede the login stage")
}

// TestLoginLostResponseRecoveryNamesBothCollections asserts recovery for a
// marked S whose login response was lost: its kind is unknown, so the
// procedure lists both token collections for the marker; with the facade
// answering the json request with an ext object, the leftover really is
// in the ext collection, which a v3-only search would have missed.
func TestLoginLostResponseRecoveryNamesBothCollections(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"v3", "ext"} {
		t.Run("server created "+shape, func(t *testing.T) {
			t.Parallel()
			f := testrancher.New(testrancher.Options{DropLoginResponse: true, LoginShape: shape})
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, "", false)
			in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
			p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
			_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
			_, d := decodeLoginFailure(t, err)
			require.NotNil(t, d.SessionToken)
			assert.Equal(t, clierr.CreateUnknown, d.SessionToken.Outcome)
			assert.Empty(t, d.SessionToken.Kind, "a lost response establishes no kind")
			assert.Nil(t, d.SessionToken.Ref)
			assert.True(t, d.SessionToken.Marked)
			v3List, _ := clierr.RecoveryCommands(clierr.TokenRef{Kind: clierr.TokenKindV3})
			extList, _ := clierr.RecoveryCommands(clierr.TokenRef{Kind: clierr.TokenKindExt})
			assert.Contains(t, d.Recovery, v3List)
			assert.Contains(t, d.Recovery, extList)
			assert.Contains(t, d.Recovery, "[op:"+d.OperationMarker+"]")
			assert.Contains(t, strings.Join(d.TextDetails(), "\n"), "(kind unknown)")
			assert.Len(t, f.TokensWithDescriptionContaining(shape, "[op:"+d.OperationMarker+"]"), 1, "the leftover is in the collection the server chose")
		})
	}
}

// TestLoginSuccessWithFailedExtSessionCleanupNamesTheTokenKind asserts the
// kind-correct command for an ext-shaped S whose revoke failed after a
// successful login: rancher delete token, never management-token.
func TestLoginSuccessWithFailedExtSessionCleanupNamesTheTokenKind(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{LoginShape: "ext", SessionDeleteMode: "fail"})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	residual := res.Data.(map[string]any)["residualTokens"].([]clierr.TokenFate)
	require.Len(t, residual, 1)
	assert.Equal(t, clierr.TokenKindExt, residual[0].Ref.Kind)
	joined := strings.Join(res.Warnings, "\n")
	_, revoke := clierr.RecoveryCommands(*residual[0].Ref)
	assert.Contains(t, joined, revoke)
	assert.Contains(t, revoke, "rancher delete token ")
	assert.NotContains(t, joined, "management-token")
}

// TestLoginSAMLTimeoutRecoveryNamesHandoffFacts asserts the one place the
// unmarked-token prose is rendered: a SAML sign-in the browser never
// completes. The session token is unknown and unmarked, the details carry
// the handoff facts, the recovery names them and the management-tokens
// route, no marker exists, and no marker text is shown. Not parallel: it
// shrinks the token package's poll timing.
func TestLoginSAMLTimeoutRecoveryNamesHandoffFacts(t *testing.T) {
	restore := token.SetSAMLTimingForTest(100*time.Millisecond, 10*time.Millisecond)
	defer restore()
	f := testrancher.New(testrancher.Options{EnableSAMLAuthTokens: true, ExtraProviders: []string{"oktaProvider"}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "oktaProvider"}, Pos: []string{"prod"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, newDepositingPrompter(nil))
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.StageAuthentication, d.Stage)
	assert.Empty(t, d.OperationMarker, "no marked create was attempted")
	require.NotNil(t, d.SessionToken)
	assert.Equal(t, clierr.CreateUnknown, d.SessionToken.Outcome)
	assert.False(t, d.SessionToken.Marked)
	assert.Nil(t, d.SessionToken.Ref)
	require.NotNil(t, d.SessionToken.Search)
	assert.Equal(t, "oktaProvider", d.SessionToken.Search.AuthProvider)
	assert.NotEmpty(t, d.SessionToken.Search.HandoffAt)
	assert.NotEmpty(t, d.SessionToken.Search.PollEndedAt)
	assert.Nil(t, d.DerivedToken)
	assert.Contains(t, d.Recovery, "oktaProvider")
	assert.Contains(t, d.Recovery, "at or after "+d.SessionToken.Search.HandoffAt)
	assert.Contains(t, d.Recovery, "stopped waiting at "+d.SessionToken.Search.PollEndedAt)
	assert.Contains(t, d.Recovery, "rancher get management-tokens -o json")
	assert.NotContains(t, d.Recovery, "[op:")
	assert.Empty(t, f.DeleteRequests())
}

// TestLoginCommitStageFailureCarriesCreateTimestamps asserts that a failure
// after acquisition still reports when each token was created.
func TestLoginCommitStageFailureCarriesCreateTimestamps(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := newPlainLoginCmd(dir, store)
	cmd.beforeCommitHook = func() {
		cfg, _ := config.Load(config.FilePath(dir))
		cfg.Servers["prod"] = config.Server{URL: srv.URL + "/changed"}
		_ = cfg.Write()
	}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err := cmd.Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	require.NotNil(t, d.SessionToken)
	require.NotNil(t, d.DerivedToken)
	_, serr := time.Parse(time.RFC3339, d.SessionToken.CreatedAt)
	require.NoError(t, serr, "session createdAt %q", d.SessionToken.CreatedAt)
	_, derr := time.Parse(time.RFC3339, d.DerivedToken.CreatedAt)
	require.NoError(t, derr, "derived createdAt %q", d.DerivedToken.CreatedAt)
	assert.LessOrEqual(t, d.SessionToken.CreatedAt, d.DerivedToken.CreatedAt)
}

// TestLoginReconciledKeyringNoticeIsAWarning asserts the committed-with-notice
// keyring branch through the command: the login succeeds, the credential
// is usable, and the keyring's advisory appears exactly once in warnings.
// Not parallel: it swaps the package-global keyring seam.
func TestLoginReconciledKeyringNoticeIsAWarning(t *testing.T) {
	keyring.MockInit()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	fk := &commitThenErrorKeyring{items: map[string]string{}, err: errors.New("dbus: reply lost")}
	restore := config.SetKeyringBackendForTest(fk)
	defer restore()
	store, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	joined := strings.Join(res.Warnings, "\n")
	assert.Equal(t, 1, strings.Count(joined, "keyring reported an error after storing the token"), "the notice appears exactly once")
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
}

// TestLoginAuthStageFailureCarriesDetails asserts the authentication-stage
// shape: a derived token create 500 after S exists yields stage authentication, S known
// and revoked, D unknown, marker present, no config or credential state.
func TestLoginAuthStageFailureCarriesDetails(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{DerivedCreateStatus: 500})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.StageAuthentication, d.Stage)
	assert.Empty(t, d.ConfigState)
	assert.Empty(t, d.CredentialState)
	assert.NotEmpty(t, d.OperationMarker)
	assert.Equal(t, clierr.CreateKnown, d.SessionToken.Outcome)
	assert.Equal(t, clierr.ResultRevoked, d.SessionToken.Cleanup.Result)
	assert.Equal(t, clierr.CreateUnknown, d.DerivedToken.Outcome)
	assert.Contains(t, d.Recovery, "[op:"+d.OperationMarker+"]")
	assert.Contains(t, d.Recovery, "rancher get tokens")
}

// newTLSFacade serves the facade over TLS with fix's certificate, answering
// /rancherversion itself: guided trust re-verifies a consented certificate
// through the version preflight, which the facade does not serve.
func newTLSFacade(t *testing.T, fix tlsFixture, f *testrancher.Facade) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rancherversion" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.20.0"}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{fix.cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestLoginCombinedWorstCaseCarriesFourDistinctErrors asserts that S
// cleanup, local compensation, D revoke, and the primary each land in
// their own field of the decoded JSON envelope. The config write that
// fails is the guided-trust commit, so the login runs over a self-signed
// facade with consent given. Not parallel: it mutates the package-level
// trustCommitConfigWriteHookForTest.
func TestLoginCombinedWorstCaseCarriesFourDistinctErrors(t *testing.T) {
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	f := testrancher.New(testrancher.Options{SessionDeleteMode: "fail", DerivedDeleteMode: "fail"})
	srv := newTLSFacade(t, fix, f)

	dir, realStore := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	compErr := errors.New("keychain locked")
	store := &compensationFailingStore{CredStore: realStore, compErr: compErr}
	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: register.TLSPreflight,
	}
	configErr := errors.New("disk full")
	restore := setTrustCommitConfigWriteHookForTest(func() error { return configErr })
	defer restore()

	p := ui.New(&bytes.Buffer{}, newLineReader("y", "e2e-user", "e2e-pass"), false, true) // consent, then the local credentials
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	b, merr := json.Marshal(d)
	require.NoError(t, merr)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got)) // decoded field access, not a rendered-JSON string match
	assert.Contains(t, got["primary"].(map[string]any)["message"], "disk full")
	assert.Equal(t, "notWritten", got["configState"])
	assert.Equal(t, "compensationFailed", got["credentialState"])
	assert.Equal(t, "keychain locked", got["credentialRecovery"].(map[string]any)["compensationError"])
	assert.Contains(t, got["derivedToken"].(map[string]any)["cleanup"].(map[string]any)["error"], "500")
	assert.Contains(t, got["sessionToken"].(map[string]any)["cleanup"].(map[string]any)["error"], "500")
	assert.Len(t, f.DeleteRequests(), 2, "S at acquire, D at commit failure")
}

// compensationFailingStore commits through the real store and replaces
// the backend's compensation with one that fails.
type compensationFailingStore struct {
	config.CredStore
	compErr error
}

func (s *compensationFailingStore) SetTokenWithOutcome(ref config.ServerRef, token string, prior config.PriorCredential) (config.CommitOutcome, error) {
	out, err := s.CredStore.SetTokenWithOutcome(ref, token, prior)
	if err == nil && out.Compensate != nil {
		out.Compensate = func() error { return s.compErr }
	}
	return out, err
}

// TestLoginUnknownDerivedLeavesOneMarkedToken asserts the state the recovery
// procedure starts from: after a lost create response exactly one facade
// token carries the marker, and the recovery names re-login, the
// kind-correct list command, and the marker. The procedure itself is
// executed with the real binary by test/e2e/scripts/login-recovery.txtar.
func TestLoginUnknownDerivedLeavesOneMarkedToken(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{DropDerivedCreateResponse: true})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.CreateUnknown, d.DerivedToken.Outcome)
	assert.Equal(t, clierr.ResultNotAttempted, d.DerivedToken.Cleanup.Result)
	assert.Contains(t, d.Recovery, "rancher auth login "+clierr.ShellQuote("prod"))
	list, _ := clierr.RecoveryCommands(clierr.TokenRef{Kind: clierr.TokenKindExt})
	assert.Contains(t, d.Recovery, list)
	assert.Contains(t, d.Recovery, "[op:"+d.OperationMarker+"]")
	assert.Len(t, f.TokensWithDescriptionContaining("ext", "[op:"+d.OperationMarker+"]"), 1, "exactly one ext token carries the marker")
}

// unreadablePriorKeyring is a keyring seam whose Set stores the value and
// then reports an error, so the read-back that follows sees it; a Get for
// an item it does not hold fails with a non-not-found error rather than a
// clean miss.
type unreadablePriorKeyring struct {
	mu    sync.Mutex
	items map[string]string
	err   error
}

func (k *unreadablePriorKeyring) Set(service, user, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"\x00"+user] = password
	return k.err
}

func (k *unreadablePriorKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"\x00"+user]
	if !ok {
		return "", errors.New("keychain locked")
	}
	return v, nil
}

func (k *unreadablePriorKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"\x00"+user)
	return nil
}

// refusingKeyring is a keyring seam that stores nothing and reports an
// error: every read is a miss, so the read-back proves the write did not
// land.
type refusingKeyring struct{ err error }

func (k refusingKeyring) Set(string, string, string) error   { return k.err }
func (k refusingKeyring) Get(string, string) (string, error) { return "", keyring.ErrNotFound }
func (k refusingKeyring) Delete(string, string) error        { return nil }

// unknownReadbackKeyring is a keyring seam that fails the write and then
// fails the read-back that follows it: what the keychain now holds is
// unknown.
type unknownReadbackKeyring struct{ err error }

func (k *unknownReadbackKeyring) Set(string, string, string) error { return k.err }

func (k *unknownReadbackKeyring) Get(string, string) (string, error) {
	return "", errors.New("dbus: no reply")
}

func (k *unknownReadbackKeyring) Delete(string, string) error { return nil }

// TestLoginKeyringCommitThenErrorSucceedsWithNotice asserts the success
// path through a keychain that stores the token and then reports an
// error: the read-back proves the write landed, the login succeeds, and
// the advisory reaches warnings. The prior is classified before the write
// now, so this login is compensatable; the uncompensatable branch is
// reachable only by a caller that hands the store a prior it could not
// classify, which the store's own tests cover.
// Not parallel: it swaps the package-global keyring seam.
func TestLoginKeyringCommitThenErrorSucceedsWithNotice(t *testing.T) {
	keyring.MockInit()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	fk := &unreadablePriorKeyring{items: map[string]string{}, err: errors.New("dbus: reply lost")}
	restore := config.SetKeyringBackendForTest(fk)
	defer restore()
	store, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "keyring reported an error after storing the token")
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
}

// TestLoginKeyringTrustWriteFailureCompensatesCredentialAndIdentity
// asserts the compensation of both halves on a fresh entry: the config
// write after the commit fails, the credential the login stored is
// deleted, and the identity record that named its account goes with it, so
// nothing is left addressable that the login did not finish.
// Not parallel: it swaps the package-global keyring seam and mutates
// trustCommitConfigWriteHookForTest.
func TestLoginKeyringTrustWriteFailureCompensatesCredentialAndIdentity(t *testing.T) {
	keyring.MockInit()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	f := testrancher.New(testrancher.Options{})
	srv := newTLSFacade(t, fix, f)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	fk := &recordingKeyring{items: map[string]string{}}
	restoreKeyring := config.SetKeyringBackendForTest(fk)
	defer restoreKeyring()
	store, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	restore := setTrustCommitConfigWriteHookForTest(func() error { return errors.New("disk full") })
	defer restore()

	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: register.TLSPreflight,
	}
	p := ui.New(&bytes.Buffer{}, newLineReader("y", "e2e-user", "e2e-pass"), false, true) // consent, then the local credentials
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err = cmd.Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
	assert.Equal(t, clierr.CredentialCompensated, d.CredentialState)
	assert.Empty(t, fk.items, "the credential the login stored is gone")
	require.NotNil(t, d.DerivedToken)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)

	_, gerr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.ErrorIs(t, gerr, config.ErrNoToken, "the identity the login recorded is gone too")
}

// recordingKeyring is a keyring seam backed by a map, for a test that has
// to see exactly which accounts are left behind. It keys the map the way
// unreadablePriorKeyring does, so the two fakes in this file cannot
// disagree about what a key is.
type recordingKeyring struct {
	mu    sync.Mutex
	items map[string]string
}

func (k *recordingKeyring) Set(service, user, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"\x00"+user] = password
	return nil
}

func (k *recordingKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (k *recordingKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"\x00"+user)
	return nil
}

// TestLoginKeyringNotWrittenFailsWithoutCredentialRecovery asserts the
// notWritten branch through the command: the read-back proves the keychain
// is untouched, so the failure reports no local residue at all.
// Not parallel: it swaps the package-global keyring seam.
func TestLoginKeyringNotWrittenFailsWithoutCredentialRecovery(t *testing.T) {
	keyring.MockInit()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	restoreKeyring := config.SetKeyringBackendForTest(refusingKeyring{err: errors.New("dbus: reply lost")})
	defer restoreKeyring()
	store, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err = newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
	assert.Equal(t, clierr.CredentialNotWritten, d.CredentialState)
	assert.Nil(t, d.CredentialRecovery)
	require.NotNil(t, d.DerivedToken)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)
}

// TestLoginKeyringPossiblyCommittedFailsWithKeychainRecovery asserts the
// possiblyCommitted branch through the command: the read-back itself failed,
// so the recovery says the keychain item cannot be identified.
// Not parallel: it swaps the package-global keyring seam.
func TestLoginKeyringPossiblyCommittedFailsWithKeychainRecovery(t *testing.T) {
	keyring.MockInit()
	f := testrancher.New(testrancher.Options{})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	restoreKeyring := config.SetKeyringBackendForTest(&unknownReadbackKeyring{err: errors.New("dbus: reply lost")})
	defer restoreKeyring()
	store, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
	_, err = newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.CredentialPossiblyCommitted, d.CredentialState)
	require.NotNil(t, d.CredentialRecovery)
	assert.Equal(t, clierr.BackendKeyring, d.CredentialRecovery.Backend)
	assert.Equal(t, "rancher auth logout "+clierr.ShellQuote("prod"), d.CredentialRecovery.Command)
	assert.Contains(t, d.Recovery, "may be this login's token, the earlier value, or another value")
	require.NotNil(t, d.DerivedToken)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)
}

// deleteJSONPath removes the value at a dotted path from a decoded JSON
// document, walking only through objects. A path that does not resolve is a
// no-op: the fields it names are the per-run ones, and their absence in one
// side is itself what the comparison must not depend on.
func deleteJSONPath(doc map[string]any, path string) {
	head, rest, more := strings.Cut(path, ".")
	if !more {
		delete(doc, head)
		return
	}
	child, ok := doc[head].(map[string]any)
	if !ok {
		return
	}
	deleteJSONPath(child, rest)
}

// assertJSONEqualIgnoring compares two JSON documents field by field after
// dropping the paths whose values change from run to run (names, timestamps,
// the marker, and the recovery text that embeds them).
func assertJSONEqualIgnoring(t *testing.T, want, got []byte, ignore ...string) {
	t.Helper()
	var w, g map[string]any
	require.NoError(t, json.Unmarshal(want, &w))
	require.NoError(t, json.Unmarshal(got, &g))
	for _, p := range ignore {
		deleteJSONPath(w, p)
		deleteJSONPath(g, p)
	}
	assert.Equal(t, w, g)
}

// loginFailureEnvelope renders err through the error envelope exactly as an
// invocation would, so the golden is the document an agent parses. The
// fixture's ephemeral URL becomes a placeholder, which keeps the failure
// messages themselves contractual.
func loginFailureEnvelope(t *testing.T, err error, serverURL string) []byte {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "expected a structured error, got %v", err)
	var buf bytes.Buffer
	require.NoError(t, output.Render(&buf, "json", output.ErrorEnvelope(ce)))
	return bytes.ReplaceAll(buf.Bytes(), []byte(serverURL), []byte("$SERVER"))
}

// updateLoginGoldens rewrites the login-failure goldens under -update.
var updateLoginGoldens = flag.Bool("update", false, "rewrite golden files")

// checkLoginFailureGolden compares the rendered envelope with the stored
// one, or rewrites it under -update.
func checkLoginFailureGolden(t *testing.T, path string, got []byte, ignore ...string) {
	t.Helper()
	if *updateLoginGoldens {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o600))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assertJSONEqualIgnoring(t, want, got, ignore...)
}

// TestLoginFailureEnvelopeGoldens asserts the JSON an agent parses for the
// three failure shapes. The per-run names, timestamps, marker, and the
// recovery text built from them are dropped; every other field is
// contractual. Not parallel: the combined case mutates
// trustCommitConfigWriteHookForTest.
func TestLoginFailureEnvelopeGoldens(t *testing.T) {
	ignore := []string{
		"data.error.details.sessionToken.createdAt",
		"data.error.details.derivedToken.createdAt",
		"data.error.details.operationMarker",
		"data.error.details.sessionToken.ref.name",
		"data.error.details.derivedToken.ref.name",
		"data.error.details.recovery",
	}
	t.Run("unknown derived token", func(t *testing.T) {
		f := testrancher.New(testrancher.Options{DropDerivedCreateResponse: true})
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		dir, store := cmdtest.FilePolicyDir(t)
		seedServer(t, dir, "prod", srv.URL, "", false)
		in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
		p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
		_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
		checkLoginFailureGolden(t, "testdata/loginfailure/unknown-derived.json", loginFailureEnvelope(t, err, srv.URL), ignore...)
	})
	t.Run("authentication stage with failed session cleanup", func(t *testing.T) {
		f := testrancher.New(testrancher.Options{DerivedCreateStatus: 500, SessionDeleteMode: "fail"})
		srv := httptest.NewServer(f)
		t.Cleanup(srv.Close)
		dir, store := cmdtest.FilePolicyDir(t)
		seedServer(t, dir, "prod", srv.URL, "", false)
		in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
		p := ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true)
		_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, p)
		checkLoginFailureGolden(t, "testdata/loginfailure/auth-stage-session-residue.json", loginFailureEnvelope(t, err, srv.URL), ignore...)
	})
	t.Run("combined worst case", func(t *testing.T) {
		fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		f := testrancher.New(testrancher.Options{SessionDeleteMode: "fail", DerivedDeleteMode: "fail"})
		srv := newTLSFacade(t, fix, f)
		dir, realStore := cmdtest.FilePolicyDir(t)
		seedServer(t, dir, "prod", srv.URL, "", false)
		store := &compensationFailingStore{CredStore: realStore, compErr: errors.New("keychain locked")}
		cmd := &loginCmd{
			dir:          dir,
			newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
			tlsPreflight: register.TLSPreflight,
		}
		restore := setTrustCommitConfigWriteHookForTest(func() error { return errors.New("disk full") })
		defer restore()
		p := ui.New(&bytes.Buffer{}, newLineReader("y", "e2e-user", "e2e-pass"), false, true)
		in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
		_, err := cmd.Execute(t.Context(), in, p)
		checkLoginFailureGolden(t, "testdata/loginfailure/combined-worst-case.json", loginFailureEnvelope(t, err, srv.URL), ignore...)
	})
}

// TestLoginReadOnlyHelperThenConfigWriteFailureRevokesOnce asserts the
// read-only-helper branch followed by a failed guided-trust config write:
// the derived token created for the login is revoked exactly once, the
// failure reports that revoke, and the read-only warning the first step
// produced reaches the operator on the error. Not parallel: it mutates the
// persistence primitive's package-level write hook.
func TestLoginReadOnlyHelperThenConfigWriteFailureRevokesOnce(t *testing.T) {
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	f := testrancher.New(testrancher.Options{})
	srv := newTLSFacade(t, fix, f)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return unsupportedStore{}, "", nil },
		tlsPreflight: register.TLSPreflight,
	}
	// The read-only branch persists the accepted certificate in the write
	// that restores the entry, so that is the step this failure targets.
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "rollback" {
			return errors.New("disk full")
		}
		return nil
	})
	defer restore()

	p := ui.New(&bytes.Buffer{}, newLineReader("y", "e2e-user", "e2e-pass"), false, true)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	ce, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.ConfigRollbackFailed, d.ConfigState)
	assert.Equal(t, clierr.CredentialNotWritten, d.CredentialState)
	require.NotNil(t, d.DerivedToken)
	require.NotNil(t, d.DerivedToken.Cleanup)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result, "the one revoke's outcome, not a second attempt's 404")
	var extDeletes int
	for _, r := range f.DeleteRequests() {
		if r.Kind == "ext" {
			extDeletes++
		}
	}
	assert.Equal(t, 1, extDeletes, "the derived token is revoked exactly once")
	assert.Contains(t, strings.Join(ce.Warnings(), "\n"), "credential helper is read-only", "the first step's warning survives the failure")
}

// TestRecoveryCommandsNameKindsFromTheTable asserts the recovery commands to
// the kind table they cannot import: the list and delete commands name a
// kind or alias the table declares, and the deprecated gate appears exactly
// when the table marks the kind deprecated.
func TestRecoveryCommandsNameKindsFromTheTable(t *testing.T) {
	t.Parallel()
	for _, kind := range []clierr.TokenKind{clierr.TokenKindV3, clierr.TokenKindExt} {
		list, revoke := clierr.RecoveryCommands(clierr.TokenRef{Kind: kind, Name: "tok-1"})
		listWords, revokeWords := strings.Fields(list), strings.Fields(revoke)
		require.GreaterOrEqual(t, len(listWords), 3, list)
		require.GreaterOrEqual(t, len(revokeWords), 3, revoke)
		assert.Equal(t, []string{"rancher", "get"}, listWords[:2], list)
		assert.Equal(t, []string{"rancher", "delete"}, revokeWords[:2], revoke)
		// The kind each command must name is fixed by the input, not read
		// back from the output: a v3 token is the management-token kind, an
		// ext token the token kind.
		wantKey := map[clierr.TokenKind]string{clierr.TokenKindV3: "managementtoken", clierr.TokenKindExt: "token"}[kind]
		k, ok := kinds.Lookup(revokeWords[2])
		require.True(t, ok, "delete names a kind the table knows: %q", revokeWords[2])
		assert.Equal(t, wantKey, k.Key, "delete names the kind the input kind maps to")
		lk, ok := kinds.Lookup(listWords[2])
		require.True(t, ok, "list names a kind or alias the table knows: %q", listWords[2])
		assert.Equal(t, wantKey, lk.Key, "list names the same kind")
		assert.Equal(t, k.Binding.Stability == safety.Deprecated, slices.Contains(revokeWords, "--allow-deprecated"),
			"the deprecated gate follows the table for %s", k.Name)
		assert.Contains(t, revokeWords, "tok-1")
	}
}

// TestLoginReadOnlyHelperThenConfigWriteFailureLegacySession asserts the
// legacy-session variant of the read-only-helper-then-failed-write case:
// the credential is the session token itself, the finalizer revokes it
// once, and the failure reports that revoke on the session fate.
func TestLoginReadOnlyHelperThenConfigWriteFailureLegacySession(t *testing.T) {
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	f := testrancher.New(testrancher.Options{Legacy: true})
	srv := newTLSFacade(t, fix, f)
	dir, _ := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return unsupportedStore{}, "", nil },
		tlsPreflight: register.TLSPreflight,
	}
	// The read-only branch persists the accepted certificate in the write
	// that restores the entry, so that is the step this failure targets.
	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "rollback" {
			return errors.New("disk full")
		}
		return nil
	})
	defer restore()

	p := ui.New(&bytes.Buffer{}, newLineReader("y", "e2e-user", "e2e-pass"), false, true)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	_, d := decodeLoginFailure(t, err)
	assert.Nil(t, d.DerivedToken, "a legacy login creates no derived token")
	require.NotNil(t, d.SessionToken)
	require.NotNil(t, d.SessionToken.Cleanup)
	assert.Equal(t, clierr.ResultRevoked, d.SessionToken.Cleanup.Result)
	var v3Deletes int
	for _, r := range f.DeleteRequests() {
		if r.Kind == "v3" {
			v3Deletes++
		}
	}
	assert.Equal(t, 1, v3Deletes, "the session token is revoked exactly once")
}

// TestLoginEarlyFailureKeepsTheVersionWarning asserts that a failure before
// the commit (a pasted token the verify round-trip rejects) still carries
// the server-version warning learned during registration, as every later
// failure does.
func TestLoginEarlyFailureKeepsTheVersionWarning(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.example", "", false)
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		verify: func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeAuth, "token rejected")
		},
	}
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}
	const versionWarning = "server 2.10.0 is below the supported minimum 2.14; some commands may fail"
	_, err := cmd.authenticateAndStore(t.Context(), in, cmdtest.StdinPrompter("id:secret"), "prod", "https://r.example", "", "", []string{versionWarning}, "", false, false)
	ce, d := decodeLoginFailure(t, err)
	assert.Equal(t, clierr.CodeAuth, d.Primary.Code)
	assert.Contains(t, ce.Warnings(), versionWarning)
}

// TestLoginRejectsExecLoginPrefix asserts the reserved namespace: an
// explicit name, a derived name, and add-server all refuse it.
func TestLoginRejectsExecLoginPrefix(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Pos: []string{"exec-login-r.ex"}}, ui.Discard())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, `"exec-login-" are reserved`)
}

// TestLoginPersistsProvenanceAndTokenRefMetadataFirst asserts a provider
// login writes the entry's provenance, tokenRef, owner, and expiry in the
// entry write that precedes the credential commit, returns them in the
// result, and never changes CurrentServer.
func TestLoginPersistsProvenanceAndTokenRefMetadataFirst(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-alice"}}})
	fake := httptest.NewServer(f)
	t.Cleanup(fake.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	seedServer(t, dir, "other", "https://other.ex", "", true)
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	res, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("alice", "pw"), false, true))
	require.NoError(t, err)
	assert.Equal(t, "derived", res.Data.(map[string]any)["provenance"])
	assert.Equal(t, "user-alice", res.Data.(map[string]any)["tokenUserId"])
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	got := cfg.Servers["prod"]
	assert.Equal(t, clierr.ProvenanceDerived, got.Provenance)
	require.NotNil(t, got.TokenRef)
	assert.Equal(t, clierr.TokenKindExt, got.TokenRef.Kind)
	assert.Equal(t, "user-alice", got.TokenUserID)
	assert.NotEmpty(t, got.CredentialExpiresAt)
	assert.Equal(t, "other", cfg.CurrentServer)
}

// TestLoginRollsTheEntryBackOnACommitFailure asserts the metadata written
// before the commit is gone after a failed commit, and the failure shape
// reports configState notWritten with the credential revoked.
func TestLoginRollsTheEntryBackOnACommitFailure(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	fake := httptest.NewServer(f)
	t.Cleanup(fake.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: store, SetErr: errors.New("keyring refused")}, "", nil
	}}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok)
	assert.Equal(t, clierr.StageCommit, d.Stage)
	assert.Equal(t, clierr.ConfigNotWritten, d.ConfigState)
	assert.Equal(t, clierr.ResultRevoked, d.DerivedToken.Cleanup.Result)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers["prod"].Provenance, "rolled back to the pre-provenance snapshot")
	assert.Nil(t, cfg.Servers["prod"].TokenRef)
	assert.Equal(t, 0, f.TokenCount(), "every token this login created is gone")
}

// TestLoginReadOnlyHelperKeepsWarnAndSucceed asserts a read-only credential
// helper at the login boundary: the command succeeds with the read-only
// warning, nothing is stored, and the entry carries no tokenRef for a token
// that was revoked.
func TestLoginReadOnlyHelperKeepsWarnAndSucceed(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	fake := httptest.NewServer(f)
	t.Cleanup(fake.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: store, SetErr: fmt.Errorf("helper: %w", config.ErrTokenStoreUnsupported)}, "", nil
	}}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	res, err := cmd.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "credential helper is read-only")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Nil(t, cfg.Servers["prod"].TokenRef)
	assert.Equal(t, 0, f.TokenCount())
}

// TestLoginUpdateRecordsThePriorDerivedToken asserts a re-login over a
// derived entry appends the prior token with its owner to supersededTokens
// (retirement is exercised separately).
func TestLoginUpdateRecordsThePriorDerivedToken(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	fake := httptest.NewServer(f)
	t.Cleanup(fake.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	s := cfg.Servers["prod"]
	s.Provenance, s.TokenRef, s.TokenUserID = clierr.ProvenanceDerived, &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-prior"}, "user-someone-else"
	cfg.Servers["prod"] = s
	require.NoError(t, cfg.Write())
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
	_, err = newPlainLoginCmd(dir, store).Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.Len(t, cfg.Servers["prod"].SupersededTokens, 1)
	assert.Equal(t, "token-prior", cfg.Servers["prod"].SupersededTokens[0].Ref.Name)
	assert.Equal(t, "user-someone-else", cfg.Servers["prod"].SupersededTokens[0].UserID)
}

// TestLoginNamelessURLBesideAPluginEntryCreatesANormalEntry asserts login
// never reuses a plugin login entry: with exec-login-<host> present at the
// same URL, `auth login --url` derives <host> and creates it beside the
// plugin entry, whose credential is untouched.
func TestLoginNamelessURLBesideAPluginEntryCreatesANormalEntry(t *testing.T) {
	t.Parallel()
	dir, store := cmdtest.FilePolicyDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["exec-login-r.ex"] = config.Server{URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin,
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-plugin"}}
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "exec-login-r.ex", URL: "https://r.ex"}, "ext/token-plugin:s"))
	cmd := newPlainLoginCmd(dir, store)
	cmd.verify = func(context.Context, config.Effective) (string, error) { return "", nil }
	in := cmdtest.Input{Str: map[string]string{"url": "https://r.ex"}, Flag: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("id:secret"), false, false))
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Contains(t, cfg.Servers, "r.ex")
	assert.Equal(t, clierr.ProvenanceUserSupplied, cfg.Servers["r.ex"].Provenance)
	assert.Equal(t, "token-plugin", cfg.Servers["exec-login-r.ex"].TokenRef.Name, "the plugin entry is untouched")
	tok, err := store.GetToken(config.ServerRef{Name: "exec-login-r.ex", URL: "https://r.ex"})
	require.NoError(t, err)
	assert.Equal(t, "ext/token-plugin:s", tok)
}

// TestLoginReLoginRetiresThePriorDerivedTokenAndWarnsOnCrossUser asserts
// the login side: a same-user re-login deletes the prior token on the
// facade with the new credential and drops its record; a re-login as
// another user keeps the record, which still identifies the prior owner,
// and the result carries the warning with the recovery sequence.
func TestLoginReLoginRetiresThePriorDerivedTokenAndWarnsOnCrossUser(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{Users: []testrancher.User{{Name: "a", Password: "pw", UserID: "user-a"}, {Name: "b", Password: "pw", UserID: "user-b"}}})
	fake := httptest.NewServer(f)
	t.Cleanup(fake.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", fake.URL, "", false)
	login := func(user string) runtime.Result {
		res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
			cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}},
			ui.New(&bytes.Buffer{}, newLineReader(user, "pw"), false, true))
		require.NoError(t, err)
		return res
	}
	login("a")
	cfg, _ := config.Load(config.FilePath(dir))
	first := cfg.Servers["prod"].TokenRef.Name

	res := login("a")
	cfg, _ = config.Load(config.FilePath(dir))
	assert.Empty(t, cfg.Servers["prod"].SupersededTokens, "same owner: retired")
	assert.Empty(t, res.Warnings)
	_, alive := f.TokenOwner(first)
	assert.False(t, alive)
	second := cfg.Servers["prod"].TokenRef.Name

	res = login("b")
	cfg, _ = config.Load(config.FilePath(dir))
	require.Len(t, cfg.Servers["prod"].SupersededTokens, 1)
	assert.Equal(t, second, cfg.Servers["prod"].SupersededTokens[0].Ref.Name)
	assert.Equal(t, "user-a", cfg.Servers["prod"].SupersededTokens[0].UserID, "the record identifies the prior owner after the bearer is overwritten")
	assert.Equal(t, "user-b", cfg.Servers["prod"].TokenUserID)
	_, alive = f.TokenOwner(second)
	assert.True(t, alive, "another user's token is never deleted on a guess")
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "rancher auth login 'recover-")
}

// TestLoginNamesTheStoreItWasToldToKeep asserts the flag reaches the
// login it names: the credential goes to that store, the directory
// records it, and the variable is only consulted to check they agree.
func TestLoginNamesTheStoreItWasToldToKeep(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	dir := t.TempDir()
	// A directory that already declares a store: the flag has to outrank
	// that declaration, or a login naming a store would commit to it and
	// leave the directory pointing every reader at the other one.
	seedServer(t, dir, "acme", "https://r.ex", "", true)
	cfg0, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg0.CredentialStore = "keychain"
	require.NoError(t, cfg0.Write())

	in := cmdtest.Input{
		Str:  map[string]string{"credential-store": "file"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"acme"},
	}
	cmd := newPlainLoginCmd(dir, nil)
	cmd.newStore = config.OpenResolvedStore
	_, err = cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.NoError(t, err)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "file", cfg.CredentialStore, "the directory records the store it was told to keep")
	tok, terr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme", URL: "https://r.ex"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
	assert.Empty(t, config.ReadPin(dir), "an explicit store never asks the pin, so it writes none")
}

// TestLoginRefusesAStoreTheVariableDisagreesWith asserts the one
// precedence rule the flag has: it outranks the directory's declaration,
// which is what it is for, but the variable and the flag must agree.
func TestLoginRefusesAStoreTheVariableDisagreesWith(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "keychain")
	dir := t.TempDir()
	in := cmdtest.Input{
		Str:  map[string]string{"url": "https://r.ex", "credential-store": "file"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"acme"},
	}
	_, err := newPlainLoginCmd(dir, config.NewFileStore(dir)).
		Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "--credential-store")
	require.ErrorContains(t, err, "RANCHER_CLI_CREDENTIAL_STORE")
	assert.NoFileExists(t, config.FilePath(dir), "nothing was written")
}

// TestLoginRefusesAStoreChangeWhileAnotherIsUnfinished asserts the other
// arm of the check before the sign-in: a change that still has work left,
// here the delete of another entry's old copy, stops a login that would
// start a second change, before anyone authenticates. Two changes cannot
// stack - the second would forget the work the first still has.
func TestLoginRefusesAStoreChangeWhileAnotherIsUnfinished(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	dir := t.TempDir()
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialStore = "file"
	cfg.Servers["other"] = config.Server{URL: "https://other.ex"}
	require.NoError(t, cfg.Write())

	id := "0123456789abcdef0123456789abcdef"
	manifest := "version: 1\nid: " + id + "\nsteps:\n" +
		"- kind: delete\n  entry: other\n  source: file\n" +
		"  verificationId: fedcba9876543210fedcba9876543210\n" +
		"  credentialDigest: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte(manifest), 0o600))
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialTransitionID = id
	require.NoError(t, cfg.Write())

	in := cmdtest.Input{
		Str:  map[string]string{"credential-store": "keychain"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"prod"},
	}
	cmd := newPlainLoginCmd(dir, nil)
	cmd.newStore = config.OpenResolvedStore
	reached := false
	cmd.verify = func(context.Context, config.Effective) (string, error) { reached = true; return "", nil }

	_, err = cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeCredentialTransitionIncomplete, ce.Code)
	assert.False(t, reached, "the refusal arrived before the sign-in")
}

// TestNewLoginOpensTheBackendItResolved asserts the opener production
// wires in. A login resolves an already-physical backend under the lock;
// opening it again through the variable's precedence would refuse, after
// the sign-in, a login doing exactly what the variable asked for.
func TestNewLoginOpensTheBackendItResolved(t *testing.T) {
	cmd, ok := NewLogin(t.TempDir()).(*loginCmd)
	require.True(t, ok)
	assert.Equal(t, reflect.ValueOf(config.OpenResolvedStore).Pointer(), reflect.ValueOf(cmd.newStore).Pointer())
}

// TestLoginRefusesAStoreChangeBeforeTheSignIn asserts the check that runs
// before anyone is asked to authenticate. A store change the directory
// cannot make is refused up front; without the check the refusal arrives
// from the commit instead, after the server has issued a token that then
// has to be revoked.
func TestLoginRefusesAStoreChangeBeforeTheSignIn(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "")
	keyring.MockInit()
	dir := t.TempDir()
	seedServer(t, dir, "prod", "https://r.ex", "", true)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.CredentialStore = "file"
	require.NoError(t, cfg.Write())
	// `other` holds a credential in both stores, which is the one entry
	// state a change into the keychain cannot decide.
	seedBothStores(t, dir, cfg, "other", "https://other.ex")

	in := cmdtest.Input{
		Str:  map[string]string{"credential-store": "keychain"},
		Flag: map[string]bool{"token-stdin": true},
		Pos:  []string{"prod"},
	}
	cmd := newPlainLoginCmd(dir, nil)
	cmd.newStore = config.OpenResolvedStore
	// A token source that records whether anything reached it: the point
	// of the check is that nothing does.
	reached := false
	p := cmdtest.StdinPrompter("id:sec\n")
	cmd.verify = func(context.Context, config.Effective) (string, error) { reached = true; return "", nil }

	_, err = cmd.Execute(t.Context(), in, p)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "other")
	assert.False(t, reached, "the refusal arrived before the sign-in")

	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "file", after.CredentialStore, "and nothing was written")
}

// seedBothStores gives an entry a credential in the file store and a
// keychain account this directory still addresses.
func seedBothStores(t *testing.T, dir string, cfg *config.Config, entry, url string) {
	t.Helper()
	cfg.Servers[entry] = config.Server{URL: url}
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: entry, URL: url}, "file-copy"))
	store, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: entry, URL: url}, "keychain-copy"))
}
