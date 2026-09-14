package cmdtest

import (
	"errors"

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
