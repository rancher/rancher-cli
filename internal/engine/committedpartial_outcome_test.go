package engine

import (
	"io"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
)

// TestCommittedPartialSuccessKeepsOutcomeUnknown asserts that a step error
// whose result is unknown keeps outcomeUnknown, its message and its hint
// when it gets the committed details, and that any other error gets none.
func TestCommittedPartialSuccessKeepsOutcomeUnknown(t *testing.T) {
	t.Parallel()
	details := clierr.CommittedContextDetails{Committed: true, ResourceKind: "User", ResourceName: "u-1", Recovery: "read it"}
	read := clierr.NewCommand("get").Word("user").Arg("u-1")
	lost := clierr.UnknownOutcome("the request to create secrets 'u-1'", clierr.OutcomeCause(io.EOF), io.EOF, read)
	ce := CommittedPartialSuccess(lost, details)
	assert.True(t, ce.OutcomeUnknown)
	assert.False(t, ce.Retryable)
	assert.Equal(t, lost.Message, ce.Message)
	assert.Equal(t, lost.Hint, ce.Hint)
	assert.Equal(t, details, ce.Details)

	assert.False(t, CommittedPartialSuccess(clierr.New(clierr.CodeGeneric, "boom"), details).OutcomeUnknown)
}
