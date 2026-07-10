package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
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

func (x args) String(k string) string { return x.s[k] }
func (x args) Bool(k string) bool     { return x.b[k] }
func (x args) Args() []string         { return x.a }

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
		Contexts:        map[string]config.Context{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, config.NewFileStore(dir)
}

func TestLoginTokenViaStdin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme", "context": "c-1:p-1"},
		b: map[string]bool{"token-stdin": true},
	}
	res, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter("id-1:secret-1\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentContext)
	assert.Equal(t, "https://rancher.example.com", cfg.Servers["acme"].URL)
	assert.Equal(t, "c-1", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project)

	tok, err := store.GetToken("acme")
	require.NoError(t, err)
	assert.Equal(t, "id-1:secret-1", tok)
}

func TestLoginExtTokenViaStdin(t *testing.T) {
	// ext-issued bearers look like "ext/token-abc:secret"; the id:secret split
	// cuts at the FIRST colon so the ext/ prefix stays inside the id (EXT-3).
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme", "context": "c-1:p-1"},
		b: map[string]bool{"token-stdin": true},
	}
	res, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter("ext/token-abc:secretxyz\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken("acme")
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
		_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).
			Execute(context.Background(), in, stdinPrompter("id:sec\n"))
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
	in := args{s: map[string]string{
		"server": fake.URL, "name": "prod", "auth-provider": "azureADProvider", "auth-flow": "devicecode",
	}}
	p := ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false) // no TTY, no --no-input
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
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
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
	require.NoError(t, err)
	tok, err := store.GetToken("acme")
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
}

func TestLoginServerRequired(t *testing.T) {
	t.Parallel()
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"token-stdin": true}}, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// Exact reviewer repro: a global --context flag with no local --context on
// auth login must not be silently dropped from the write.
func TestLoginRejectsShadowedGlobalOverride(t *testing.T) {
	dir, store := filePolicyDir(t)
	config.SetContextOverride("c-global:p-global")
	t.Cleanup(func() { config.SetContextOverride("") })

	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	_, err := NewLogin(dir, store).(runtime.ReadCommand).
		Execute(context.Background(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, statErr := config.Load(config.FilePath(dir))
	require.NoError(t, statErr)
}

func TestLoginUnaffectedWithoutGlobalOverride(t *testing.T) {
	dir, store := filePolicyDir(t)
	in := args{
		s: map[string]string{"server": "https://rancher.example.com", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	_, err := NewLogin(dir, store).(runtime.ReadCommand).
		Execute(context.Background(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
}

func TestLoginTokenRequiredNonInteractive(t *testing.T) {
	t.Parallel()
	// No --token-stdin and !CanPrompt (Discard) -> VALIDATION "token required".
	in := args{s: map[string]string{"server": "https://r.ex"}}
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), in, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginEmptyStdinToken(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter(""))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStdinNonEOFErrorIsGeneric(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	p := ui.New(&bytes.Buffer{}, errReader{}, true, false)
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestLoginBadTokenShape(t *testing.T) {
	t.Parallel()
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter("notacolon\n"))
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
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://rancher.ex", cfg.Servers["acme"].URL)
}

func TestLoginDefaultName(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://r.ex", cfg.Servers["default"].URL)
}

func TestLoginEmptyPromptToken(t *testing.T) {
	t.Parallel()
	// TTY prompt returns a blank line -> "no token", not an id:secret shape error.
	p := ui.New(&bytes.Buffer{}, strings.NewReader("\n"), false, true)
	in := args{s: map[string]string{"server": "https://r.ex"}}
	_, err := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir())).(runtime.ReadCommand).Execute(context.Background(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginUpgradesAutoPinToKeychain(t *testing.T) {
	// Non-parallel: keyring.MockInit swaps a process-global in-memory backend.
	// Isolate from the operator's shell: RANCHER_CREDENTIAL_STORE=file (this
	// project's own recommended hermetic-manual-run setting) would otherwise
	// outrank both the "auto" policy under test and the explicit "keychain"
	// argument to OpenStore below (config.EffectivePolicy: env > argument).
	t.Setenv("RANCHER_CREDENTIAL_STORE", "")
	keyring.MockInit()
	dir := t.TempDir() // no config on disk → auto policy; mock keychain is usable
	in := args{
		s: map[string]string{"server": "https://r.ex", "name": "acme"},
		b: map[string]bool{"token-stdin": true},
	}
	// Inject a file store; under auto with a usable (mock) keychain, login must
	// re-pin to keychain and write the token there, not to the file store.
	_, err := NewLogin(dir, config.NewFileStore(dir)).(runtime.ReadCommand).
		Execute(context.Background(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	if got := config.ReadPin(dir); got != "keychain" {
		t.Fatalf("pin after auto login = %q, want keychain", got)
	}
	ks, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	tok, err := ks.GetToken("acme")
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok)
	if _, ferr := config.NewFileStore(dir).GetToken("acme"); !errors.Is(ferr, config.ErrNoToken) {
		t.Errorf("token must land in the keychain, not the file store; got %v", ferr)
	}
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

		case r.URL.Path == mintTokensPath && r.Method == http.MethodPost:
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

		case (strings.HasPrefix(r.URL.Path, v3TokensPath+"/") || strings.HasPrefix(r.URL.Path, mintTokensPath+"/")) && r.Method == http.MethodDelete:
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
	res, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, err := store.GetToken("prod")
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
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken("prod")
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
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
	require.NoError(t, err)

	tok, err := store.GetToken("prod")
	require.NoError(t, err)
	assert.Equal(t, "ext/derived-1:secretvalue", tok, "a revoke failure must not block login")
	assert.Contains(t, errBuf.String(), "could not revoke the transient session token")
}

func TestLoginProviderFlowTTLPlumbedToMintBodyInMilliseconds(t *testing.T) {
	t.Parallel()
	bodyCh := make(chan []byte, 1)
	fake := newExtLoginFake(t, extLoginFakeOpts{sessionToken: "token-sess4:sessionsecret", mintBodyCh: bodyCh})

	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider", "ttl": "12h"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
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
	in := args{s: map[string]string{"server": fake.URL, "name": "prod", "auth-provider": "localProvider"}}
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
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
	_, err := NewLogin(dir, store).(runtime.ReadCommand).Execute(context.Background(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	_, tokErr := store.GetToken("prod")
	assert.Error(t, tokErr, "a rejected ttl must never reach the mint/store step")
}
