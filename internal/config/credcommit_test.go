package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// fakeCredStore lets a test control exactly what GetToken/SetToken return,
// including whether GetToken ran at all, without a real backend's failure
// modes. CommitToken never calls the embedded (nil) CredStore's other
// methods, so leaving them unimplemented is safe.
type fakeCredStore struct {
	CredStore
	getErr    error
	getCalled bool
	setErr    error
}

func (s *fakeCredStore) GetToken(ServerRef) (string, error) {
	s.getCalled = true
	return "", s.getErr
}

func (s *fakeCredStore) SetToken(ServerRef, string) error { return s.setErr }

// fakeNonAtomicStore is a fakeCredStore that also declares itself
// NonAtomicStore.
type fakeNonAtomicStore struct{ fakeCredStore }

func (s *fakeNonAtomicStore) NonAtomic() bool { return true }

func TestCommitTokenFileStoreNoPriorTokenCompensateDeletes(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	compensate, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, compensate)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, compensate())
	_, gerr = store.GetToken(ref)
	assert.ErrorIs(t, gerr, ErrNoToken)
}

func TestCommitTokenFileStoreReplacesExistingTokenCompensateRestores(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}
	require.NoError(t, store.SetToken(ref, "old-token"))

	compensate, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, compensate)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, compensate())
	tok, gerr = store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "old-token", tok, "compensation restores the exact prior token, never a bare delete")
}

func TestCommitTokenExecStoreNonAtomicNeverCompensates(t *testing.T) {
	dir := t.TempDir()
	execStore := newExecStore("does-not-exist", dir)
	var store CredStore = execStore
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	_, err := CommitToken(store, ref, "new-token")
	require.Error(t, err, "the helper binary is not on PATH")

	assert.True(t, execStore.NonAtomic())
}

func TestCommitTokenKeyringStoreReplacesExistingTokenCompensateRestores(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	store := newPinnedKeyringStore(dir, false)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}
	require.NoError(t, store.SetToken(ref, "old-token"))

	compensate, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	require.NotNil(t, compensate)

	tok, gerr := store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "new-token", tok)

	require.NoError(t, compensate())
	tok, gerr = store.GetToken(ref)
	require.NoError(t, gerr)
	assert.Equal(t, "old-token", tok)
}

func TestCommitTokenUnreadablePriorTokenNeverCompensates(t *testing.T) {
	store := &fakeCredStore{getErr: errors.New("keychain locked")}
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	compensate, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	assert.Nil(t, compensate, "an unreadable prior token must never yield a delete-compensation")
}

func TestCommitTokenNeverReadsPriorOnNonAtomicStore(t *testing.T) {
	store := &fakeNonAtomicStore{}
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	compensate, err := CommitToken(store, ref, "new-token")
	require.NoError(t, err)
	assert.Nil(t, compensate)
	assert.False(t, store.getCalled, "GetToken must never run on a NonAtomicStore")
}

func TestCommitTokenFileStoreSetTokenFailurePropagatesNoCompensate(t *testing.T) {
	store := &fileStore{path: string([]byte{0}), notices: &noticeBuffer{}}
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	compensate, err := CommitToken(store, ref, "new-token")
	require.Error(t, err)
	assert.Nil(t, compensate)
}
