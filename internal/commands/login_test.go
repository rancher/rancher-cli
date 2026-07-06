package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type args struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (x args) String(k string) string { return x.s[k] }
func (x args) Bool(k string) bool     { return x.b[k] }
func (x args) Args() []string         { return x.a }

func TestLoginWritesConfigAndToken(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cmd := NewLogin(dir, store)

	in := args{
		s: map[string]string{"token": "id-1:secret-1", "name": "acme", "context": "c-1:p-1"},
		a: []string{"https://rancher.example.com"},
	}
	res, err := cmd.Execute(context.Background(), in, safety.Effect{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentContext)
	assert.Equal(t, "https://rancher.example.com", cfg.Servers["acme"].URL)
	assert.Equal(t, "c-1", cfg.Contexts["acme"].Cluster)

	tok, err := store.GetToken("acme")
	require.NoError(t, err)
	assert.Equal(t, "id-1:secret-1", tok)
}

func TestLoginRequiresServerURL(t *testing.T) {
	cmd := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir()))
	_, err := cmd.Execute(context.Background(), args{s: map[string]string{"token": "a:b"}}, safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code) // must be exit-2, not CodeGeneric
}

func TestLoginRequiresToken(t *testing.T) {
	cmd := NewLogin(t.TempDir(), config.NewFileStore(t.TempDir()))
	_, err := cmd.Execute(context.Background(), args{a: []string{"https://r.ex"}}, safety.Effect{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLoginStripsUserinfoAndQuery(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	_, err := NewLogin(dir, store).Execute(context.Background(),
		args{s: map[string]string{"token": "id:sec", "name": "acme"},
			a: []string{"https://admin:pw@rancher.ex/v3?debug=1#frag"}}, safety.Effect{}, ui.Discard())
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "https://rancher.ex", cfg.Servers["acme"].URL) // no userinfo/path/query/fragment
}
