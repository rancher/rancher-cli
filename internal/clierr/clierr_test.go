package clierr

import (
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
		WithPhase("plan").
		WithTier("crd").
		WithRetryable(true)
	require.Equal(t, "needs confirm", e.Error())
	assert.Equal(t, "re-run with: --apply --yes", e.Hint)
	assert.Equal(t, "plan", e.Phase)
	assert.True(t, e.Retryable)
}

func TestAllCodesNonEmpty(t *testing.T) {
	assert.Contains(t, AllCodes(), CodeContextNotSet)
	assert.Contains(t, AllCodes(), CodeDryRunNotApplicable)
}
