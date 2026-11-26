package runtime

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/stretchr/testify/assert"
)

// TestAnInterruptAfterAWriteWasSentKeepsTheUnknownOutcome asserts that a
// command interrupted after a write's request was sent is INTERRUPTED with
// the write's message, hint and OutcomeUnknown, and that a bare cancellation
// is still "interrupted" alone.
func TestAnInterruptAfterAWriteWasSentKeepsTheUnknownOutcome(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	read := clierr.NewCommand("get").Word("configmap").Arg("web")
	cause := fmt.Errorf("Post: %w", context.Canceled)
	write := clierr.UnknownOutcome("the request to create configmaps 'web'", clierr.OutcomeCause(cause), cause, read)

	ce := toClierr(ctx, write)
	assert.Equal(t, clierr.CodeInterrupted, ce.Code)
	assert.True(t, ce.OutcomeUnknown)
	assert.False(t, ce.Retryable)
	assert.Equal(t, "interrupted: "+write.Message, ce.Message)
	assert.Equal(t, write.Hint, ce.Hint)

	bare := toClierr(ctx, context.Canceled)
	assert.Equal(t, "interrupted", bare.Message)
	assert.False(t, bare.OutcomeUnknown)
}

// TestTheTextBlockNeverSaysAnErrorWhoseResultIsUnknownIsRetryable asserts
// that the stderr block of an error with outcomeUnknown does not say
// "retryable" even when a caller set Retryable to true on it directly, and
// that the block of any other retryable error does.
func TestTheTextBlockNeverSaysAnErrorWhoseResultIsUnknownIsRetryable(t *testing.T) {
	t.Parallel()
	lost := clierr.UnknownOutcome("the request to create configmaps 'web'", clierr.OutcomeCause(io.EOF), io.EOF, clierr.Command{})
	lost.Retryable = true
	header, _, _ := strings.Cut(errorText(lost, style.Style{}, plainRemedies), "\n")
	assert.True(t, strings.HasPrefix(header, "error (GENERIC): "), header)

	plain := clierr.New(clierr.CodeGeneric, "the server may be slow; retry later").WithRetryable(true)
	header, _, _ = strings.Cut(errorText(plain, style.Style{}, plainRemedies), "\n")
	assert.True(t, strings.HasPrefix(header, "error (GENERIC, retryable): "), header)
}
