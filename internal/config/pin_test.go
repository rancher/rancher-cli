package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPinRoundTrip(t *testing.T) {
	dir := t.TempDir()
	assert.Empty(t, ReadPin(dir), "ReadPin on empty dir")
	require.NoError(t, WritePin(dir, "keychain"))
	assert.Equal(t, "keychain", ReadPin(dir))
	fi, err := os.Stat(filepath.Join(dir, "credstore-pin"))
	if assert.NoError(t, err, "pin file mode/err") && runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (hardenACL is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
}

func TestReadPinIgnoresGarbage(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credstore-pin"), []byte("keyChain\n"), 0o600))
	assert.Empty(t, ReadPin(dir), "garbage pin should re-probe")
}

// TestEffectivePolicyWithSourceConflict asserts what the variable and the
// declaration do to each other. A directory that declares a store owns
// that choice: a variable naming a different one is reported rather than
// silently winning, because two processes on one directory would
// otherwise read different backends and, when both hold a credential,
// each would serve whichever its own environment selected. The declared
// value still comes back beside the error so a caller that only displays
// the pair can show what the directory says.
func TestEffectivePolicyWithSourceConflict(t *testing.T) {
	for _, tc := range []struct {
		name           string
		env, declared  string
		policy, source string
		conflict       bool
	}{
		{name: "nothing set", policy: "auto", source: "default"},
		{name: "declaration only", declared: "file", policy: "file", source: "config"},
		{name: "variable only", env: "file", policy: "file", source: "env"},
		{name: "both agree", env: "file", declared: "file", policy: "file", source: "env"},
		{name: "both disagree", env: "file", declared: "keychain", policy: "keychain", source: "config", conflict: true},
		{name: "variable disagrees with auto", env: "file", declared: "auto", policy: "auto", source: "config", conflict: true},
		{name: "helper against keychain", env: "exec:vault", declared: "keychain", policy: "keychain", source: "config", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCredentialStore, tc.env)
			policy, source, err := EffectivePolicyWithSource(tc.declared)
			assert.Equal(t, tc.policy, policy)
			assert.Equal(t, tc.source, source)
			if !tc.conflict {
				require.NoError(t, err)
				valueOnly, verr := EffectivePolicy(tc.declared)
				require.NoError(t, verr)
				assert.Equal(t, tc.policy, valueOnly, "the value-only form cannot disagree")
				return
			}
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, strconv.Quote(tc.declared))
			require.ErrorContains(t, err, strconv.Quote(tc.env))
			require.ErrorContains(t, err, EnvCredentialStore)
			assert.Equal(t, "unset "+EnvCredentialStore, ce.Hint, "the remedy is one command a caller can run")

			_, verr := EffectivePolicy(tc.declared)
			require.Error(t, verr, "the value-only form carries the conflict too")
		})
	}
}
