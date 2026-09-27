package cmdtest

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// SetErrStore wraps a real store, failing the token write with a fixed
// error. The failure is injected at SetTokenWithOutcome - the method the
// commit path calls - so SetToken delegating to it keeps both spellings on
// the same behavior.
type SetErrStore struct {
	config.CredStore
	SetErr error
}

func (s SetErrStore) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	backend := clierr.BackendFile
	if errors.Is(s.SetErr, config.ErrTokenStoreUnsupported) {
		backend = clierr.BackendExec
	}
	return config.CommitOutcome{Backend: backend, State: config.CommitNotWritten, PriorSource: clierr.PriorUnknown}, s.SetErr
}

func (s SetErrStore) SetToken(ref config.ServerRef, tok string) error {
	_, err := s.SetTokenWithOutcome(ref, tok, config.PriorCredential{})
	return err
}

// SeedUnpinned writes an `auto` directory with no pin whose entries keep
// their credentials where the maps say: file names entries with a token in
// credentials.yaml, keychain entries with an identity record and a slot in
// the (mocked) keychain. Every entry is in config.yaml. The caller runs
// keyring.MockInit first.
func SeedUnpinned(t *testing.T, dir string, file, keychain map[string]string) {
	t.Helper()
	cfg := &config.Config{Servers: map[string]config.Server{}}
	for _, m := range []map[string]string{file, keychain} {
		for name := range m {
			cfg.Servers[name] = config.Server{URL: "https://" + name + ".ex"}
			cfg.CurrentServer = name
		}
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	for name, tok := range file {
		require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: name, URL: "https://" + name + ".ex"}, tok))
	}
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	for name, tok := range keychain {
		require.NoError(t, ks.SetToken(config.ServerRef{Name: name, URL: "https://" + name + ".ex"}, tok))
	}
}
