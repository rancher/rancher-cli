package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAddServerCmd(dir string) *configAddServerCmd {
	return &configAddServerCmd{dir: dir} // nil tlsPreflight: no network in tests
}

func TestAddServerCreatesEntryAndBootstrapsPointer(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)

	res, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://qa1.example.com"}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, "qa1", data["name"])
	assert.Equal(t, "qa1", data["currentServer"])
	assert.Equal(t, false, data["unchanged"])

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "qa1", cfg.CurrentServer)
	assert.Equal(t, "https://qa1.example.com", cfg.Servers["qa1"].URL)
}

func TestAddServerIdenticalReRunIsUnchanged(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)
	in := args{s: map[string]string{"url": "https://qa1.example.com"}, a: []string{"qa1"}}

	_, err := c.Execute(context.Background(), in, nil)
	require.NoError(t, err)

	res, err := c.Execute(context.Background(), in, nil)
	require.NoError(t, err)
	assert.Equal(t, true, res.Data.(map[string]any)["unchanged"])
}

func TestAddServerDifferentURLConflicts(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://old.example.com"}, a: []string{"qa1"}}, nil)
	require.NoError(t, err)

	_, err = c.Execute(context.Background(), args{s: map[string]string{"url": "https://new.example.com"}, a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Equal(t, "rancher config update-server 'qa1' --url 'https://new.example.com'", ce.Hint)
}

func TestAddServerRejectsLegacyPrefixedName(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "https://qa1.example.com"}, a: []string{config.LegacyCachePrefix + "x"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestAddServerRequiresURL(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), args{a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestAddServerRejectsUnsupportedScheme: an ftp:// (or any non-http(s))
// URL must never commit an entry — config.Effective's credential-read
// path would silently withhold the token for it later, reading as an
// unexplained NOT_LOGGED_IN after an apparently successful add.
func TestAddServerRejectsUnsupportedScheme(t *testing.T) {
	dir, _ := filePolicyDir(t)
	c := newAddServerCmd(dir)
	_, err := c.Execute(context.Background(), args{s: map[string]string{"url": "ftp://host"}, a: []string{"qa1"}}, nil)
	ce, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Empty(t, cfg.Servers, "no entry written for an unsupported scheme")
}
