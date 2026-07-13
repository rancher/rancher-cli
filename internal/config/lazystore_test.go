package config

import (
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLazyStoreResolvesBackendExactlyOnce pins the sync.Once contract: many
// calls across many goroutines resolve the backend a single time, and every
// call observes the same backend.
func TestLazyStoreResolvesBackendExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken("prod", "tok"))

	var calls int
	l := &lazyStore{resolve: func() (CredStore, error) {
		calls++
		return fs, nil
	}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := l.GetToken("prod")
			assert.NoError(t, err)
			assert.Equal(t, "tok", tok)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, calls)
}

// TestLazyStoreDeferredErrorIsStable: a failed resolution surfaces the SAME
// raw error from the first and every later call (spec R1-3: returned as-is,
// callers add their own context).
func TestLazyStoreDeferredErrorIsStable(t *testing.T) {
	boom := errors.New("writing credential store pin: disk full")
	l := &lazyStore{resolve: func() (CredStore, error) { return nil, boom }}

	_, err1 := l.GetToken("prod")
	err2 := l.SetToken("prod", "tok")
	require.ErrorIs(t, err1, boom)
	require.ErrorIs(t, err2, boom)
}

// TestNewLazyStoreValidatesPolicyEagerly: an unknown effective policy fails
// at CONSTRUCTION with OpenStore's exact error text (CS-6 unchanged), and no
// filesystem state appears.
func TestNewLazyStoreValidatesPolicyEagerly(t *testing.T) {
	t.Setenv(EnvCredentialStore, "wat")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore("", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown credential store "wat"`)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr))
}

// TestNewLazyStoreCapturesEffectivePolicy (spec R1-4): the policy resolved
// at construction is the policy opened, even if env changes before first
// use.
func TestNewLazyStoreCapturesEffectivePolicy(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()
	l, err := NewLazyStore("", dir)
	require.NoError(t, err)

	// Env flips to an INVALID policy after construction; the captured
	// "file" policy must still be what resolves.
	t.Setenv(EnvCredentialStore, "wat")
	require.NoError(t, l.SetToken("prod", "tok"))
	tok, err := NewFileStore(dir).GetToken("prod")
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}

// TestNewLazyStoreNoSideEffectsBeforeFirstUse pins LS-1's core: constructing
// the store for a fresh dir creates NOTHING until a credential call happens.
func TestNewLazyStoreNoSideEffectsBeforeFirstUse(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore("", dir)
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr), "construction must not create the config dir or pin")
}
