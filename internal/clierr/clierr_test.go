package clierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitCodeMapping(t *testing.T) {
	cases := map[string]int{
		CodeValidation:           2,
		CodeConfirmationRequired: 2,
		CodeDryRunNotApplicable:  2,
		CodeAmbiguousSelection:   2,
		CodeAuth:                 3,
		CodeNotFound:             4,
		CodeConflict:             5,
		CodeRBAC:                 6,
		CodeGeneric:              1,
		CodeContextNotSet:        2,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			assert.Equal(t, want, New(code, "x").ExitCode())
		})
	}
}

func TestBuilderAndError(t *testing.T) {
	e := New(CodeConfirmationRequired, "needs confirm").
		WithHint("re-run with: --apply --yes").
		WithRetryable(true)
	require.Equal(t, "needs confirm", e.Error())
	assert.Equal(t, "re-run with: --apply --yes", e.Hint)
	assert.True(t, e.Retryable)
}

func TestIsNotFound(t *testing.T) {
	assert.True(t, IsNotFound(New(CodeNotFound, "x")))
	assert.False(t, IsNotFound(New(CodeGeneric, "x")))
	assert.False(t, IsNotFound(nil))
}

func TestAllCodesNonEmpty(t *testing.T) {
	assert.Contains(t, AllCodes(), CodeContextNotSet)
	assert.Contains(t, AllCodes(), CodeDryRunNotApplicable)
}

// TestAllCodesDropsNotImplemented pins DR-013: NOT_IMPLEMENTED is a
// permanently-dead branch (no construction site ever advertises it) and has
// been removed. The count pins the full enum: 10 originals plus LOCK_TIMEOUT.
func TestAllCodesDropsNotImplemented(t *testing.T) {
	codes := AllCodes()
	assert.Len(t, codes, 11)
	assert.NotContains(t, codes, "NOT_IMPLEMENTED")
	assert.Contains(t, codes, CodeLockTimeout)
}

func TestErrorDetailsSerialization(t *testing.T) {
	e := New(CodeConflict, "partial failure").WithDetails(map[string]any{
		"deleted": []string{"a"},
	})
	b, err := json.Marshal(e)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	details, ok := out["details"].(map[string]any)
	require.True(t, ok, "details must serialize as an object")
	assert.Equal(t, []any{"a"}, details["deleted"])

	// omitempty: absent when unset
	b2, err := json.Marshal(New(CodeNotFound, "x"))
	require.NoError(t, err)
	assert.NotContains(t, string(b2), "details")
}

// TestWrapPreservesClassification pins the exercise-8/9-fixes review fix:
// command wrappers adding context must not flatten a classified error (a
// LOCK_TIMEOUT from a store lock) into the GENERIC catch-all.
func TestWrapPreservesClassification(t *testing.T) {
	t.Run("classified error keeps code and retryability under the new message", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "locking token-cache.yaml: context deadline exceeded")
		got := Wrap(inner, "failed to drain: boom")
		assert.Equal(t, CodeLockTimeout, got.Code)
		assert.True(t, got.Retryable)
		assert.Equal(t, "failed to drain: boom", got.Message)
		assert.Equal(t, 8, got.ExitCode())
	})

	t.Run("wrapped classified error is found through the chain", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "locked").WithCause(errors.New("flock"))
		chained := fmt.Errorf("outer: %w", inner)
		got := Wrap(chained, "ctx: outer: locked")
		assert.Equal(t, CodeLockTimeout, got.Code)
		assert.True(t, got.Retryable)
	})

	t.Run("plain error becomes GENERIC with the cause kept", func(t *testing.T) {
		plain := errors.New("disk failure")
		got := Wrap(plain, "ctx: disk failure")
		assert.Equal(t, CodeGeneric, got.Code)
		assert.False(t, got.Retryable)
		assert.ErrorIs(t, got, plain)
	})

	t.Run("does not mutate the wrapped error", func(t *testing.T) {
		inner := Retryable(CodeLockTimeout, "original")
		_ = Wrap(inner, "new message").WithHint("added hint")
		assert.Equal(t, "original", inner.Message)
		assert.Empty(t, inner.Hint)
	})
}
