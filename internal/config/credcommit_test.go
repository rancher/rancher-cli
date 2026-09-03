package config

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// fakeCredStore lets a test control exactly what a commit reports, and
// whether GetToken ran at all, without a real backend's failure modes.
// CommitToken never calls the embedded (nil) CredStore's other methods, so
// leaving them unimplemented is safe.
type fakeCredStore struct {
	CredStore
	out       CommitOutcome
	err       error
	getCalled bool
}

func (s *fakeCredStore) GetToken(ServerRef) (string, error) {
	s.getCalled = true
	return "", nil
}

func (s *fakeCredStore) SetTokenWithOutcome(ServerRef, string) (CommitOutcome, error) {
	return s.out, s.err
}

func TestCommitTokenFileStoreNoPriorTokenCompensateDeletes(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	out, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, out.Compensate)
	assert.Equal(t, clierr.PriorNone, out.PriorSource)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, out.Compensate())
	_, gerr = store.GetToken(ref)
	assert.ErrorIs(t, gerr, ErrNoToken)
}

func TestCommitTokenFileStoreReplacesExistingTokenCompensateRestores(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}
	require.NoError(t, store.SetToken(ref, "old-token"))

	out, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, out.Compensate)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, out.Compensate())
	tok, gerr = store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "old-token", tok, "compensation restores the exact prior token, never a bare delete")
}

func TestCommitTokenExecStoreNeverCompensates(t *testing.T) {
	dir := t.TempDir()
	var store CredStore = newExecStore("does-not-exist", dir)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	out, err := CommitToken(store, ref, "new-token")
	require.Error(t, err, "the helper binary is not on PATH")
	assert.Equal(t, clierr.BackendExec, out.Backend)
	assert.Equal(t, CommitPossiblyCommitted, out.State)
	assert.Nil(t, out.Compensate)
}

func TestCommitTokenKeyringStoreReplacesExistingTokenCompensateRestores(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	store := newPinnedKeyringStore(dir, false)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}
	require.NoError(t, store.SetToken(ref, "old-token"))

	out, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, out.Compensate)
	assert.Equal(t, clierr.PriorKeyring, out.PriorSource)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, out.Compensate())
	tok, gerr = store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "old-token", tok)
}

func TestCommitTokenNeverReadsPrior(t *testing.T) {
	store := &fakeCredStore{out: CommitOutcome{
		Backend:              clierr.BackendExec,
		State:                CommitHelperNonAtomic,
		PriorSource:          clierr.PriorUnknown,
		HelperRecordRetained: true,
	}}
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	out, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	assert.Equal(t, store.out, out, "the backend's own account is returned unchanged")
	assert.False(t, store.getCalled, "CommitToken must never read a prior value itself")
}

func TestCommitTokenFileStoreSetTokenFailurePropagatesNoCompensate(t *testing.T) {
	store := &fileStore{path: string([]byte{0}), notices: &noticeBuffer{}}
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	out, err := CommitToken(store, ref, "new-token")
	require.Error(t, err)
	assert.Equal(t, CommitNotWritten, out.State)
	assert.Nil(t, out.Compensate)
}
