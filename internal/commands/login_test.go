package commands

import (
	"bytes"
	"context"
	"errors"
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

func TestLoginTokenViaPrompt(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	// isTTY=true, noInput=false -> CanPrompt; PromptSecret reads the line from the buffer.
	p := ui.New(&bytes.Buffer{}, strings.NewReader("id:sec\n"), false, true)
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
	assert.Equal(t, "https://r.ex", cfg.Servers["rancherDefault"].URL)
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
