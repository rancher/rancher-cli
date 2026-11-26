package clierr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnknownOutcomeSaysTheChangeMayBeOnTheServer asserts the error for a
// write whose response was lost: GENERIC, not retryable, OutcomeUnknown set,
// the cause worded without the URL, the read command in the message and the
// hint, and the cause kept for errors.Is and HTTPStatus.
func TestUnknownOutcomeSaysTheChangeMayBeOnTheServer(t *testing.T) {
	t.Parallel()
	read := NewCommand("get").Word("configmap").Arg("web").FlagValue("--namespace", "team-a")
	for _, tc := range []struct {
		name  string
		err   error
		cause string
	}{
		{"connection", fmt.Errorf("Post %q: %w", "https://h/api", io.EOF), "the connection failed"},
		{"timeout", fmt.Errorf("Post %q: %w", "https://h/api", context.DeadlineExceeded), "the request timed out"},
		{"cancellation", fmt.Errorf("Post %q: %w", "https://h/api", context.Canceled), "the command was stopped"},
		{"504", ResponseError(http.MethodPost, http.StatusGatewayTimeout, nil), "the server responded with 504"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := UnknownOutcome("the request to create configmaps 'web'", OutcomeCause(tc.err), tc.err, read)
			assert.Equal(t, CodeGeneric, ce.Code)
			assert.False(t, ce.Retryable)
			assert.True(t, ce.OutcomeUnknown)
			assert.Equal(t, "the request to create configmaps 'web' was sent, but whether the server applied the change is unknown ("+
				tc.cause+"); before you run the command again, check with: "+Rancher+" get configmap 'web' --namespace 'team-a'", ce.Message)
			assert.Equal(t, Rancher+" get configmap 'web' --namespace 'team-a'", ce.Hint)
			assert.ErrorIs(t, ce, tc.err)
		})
	}
	err := ResponseError(http.MethodPost, http.StatusBadGateway, nil)
	_, ok := HTTPStatus(UnknownOutcome("the request", OutcomeCause(err), err, read))
	assert.True(t, ok, "the status of the response stays readable")
}

// TestUnknownOutcomeWithoutACommandHasNoHint asserts that a write the CLI
// has no read command for says to check whether the change is on the
// server and has no hint, and that a read command that cannot be printed is
// left out with the note and no hint.
func TestUnknownOutcomeWithoutACommandHasNoHint(t *testing.T) {
	t.Parallel()
	ce := UnknownOutcome("the request to create selfusers.ext.cattle.io", OutcomeCause(io.EOF), io.EOF, Command{})
	assert.Equal(t, "the request to create selfusers.ext.cattle.io was sent, but whether the server applied the change is unknown (the connection failed); before you run the command again, check whether the change is on the server", ce.Message)
	assert.Empty(t, ce.Hint)
	assert.True(t, ce.OutcomeUnknown)

	left := UnknownOutcome("the request to delete configmaps", OutcomeCause(io.EOF), io.EOF, NewCommand("get").Word("configmap").Arg("it's"))
	assert.Empty(t, left.Hint)
	assert.True(t, strings.HasPrefix(left.Message, "the request to delete configmaps was sent, but whether the server applied the change is unknown (the connection failed); before you run the command again, check whether the change is on the server"), left.Message)
	assert.Equal(t, 1, strings.Count(left.Message, LeftOutNote))
}

// TestUnknownOutcomeOfAnObjectTheCallerCannotGetNamesNoGetCommand asserts
// the error for a write of an object the caller cannot get: it says so,
// says to have a user who can get it check, names the list of the kind in
// the message only, and has no hint.
func TestUnknownOutcomeOfAnObjectTheCallerCannotGetNamesNoGetCommand(t *testing.T) {
	t.Parallel()
	list := NewCommand("get").Word("configmap").FlagValue("--namespace", "team-a")
	ce := UnknownOutcomeUnreadable("the request to delete configmaps 'web'", OutcomeCause(io.EOF), io.EOF, list)
	assert.Equal(t, "the request to delete configmaps 'web' was sent, but whether the server applied the change is unknown (the connection failed); you cannot get the object: before you run the command again, ask a user who can get it to check whether the change is on the server, or list the objects of its kind if you can: "+Rancher+" get configmap --namespace 'team-a'", ce.Message)
	assert.Empty(t, ce.Hint)
	assert.True(t, ce.OutcomeUnknown)
	assert.False(t, ce.Retryable)
	assert.Equal(t, CodeGeneric, ce.Code)

	bare := UnknownOutcomeUnreadable("the request to delete selfusers.ext.cattle.io 'x'", OutcomeCause(io.EOF), io.EOF, Command{})
	assert.Equal(t, "the request to delete selfusers.ext.cattle.io 'x' was sent, but whether the server applied the change is unknown (the connection failed); you cannot get the object: before you run the command again, ask a user who can get it to check whether the change is on the server", bare.Message)
	assert.Empty(t, bare.Hint)

	left := UnknownOutcomeUnreadable("the request to delete configmaps 'web'", OutcomeCause(io.EOF), io.EOF, NewCommand("get").Word("configmap").FlagValue("--namespace", "it's"))
	assert.Equal(t, "the request to delete configmaps 'web' was sent, but whether the server applied the change is unknown (the connection failed); you cannot get the object: before you run the command again, ask a user who can get it to check whether the change is on the server; "+LeftOutNote, left.Message)
	assert.Empty(t, left.Hint)
	assert.True(t, left.OutcomeUnknown)
}

// TestAnErrorWhoseResultIsUnknownIsNeverRetryable asserts that WithRetryable
// cannot make an error with OutcomeUnknown retryable, also after a caller
// re-words it, and that it still sets any other error retryable.
func TestAnErrorWhoseResultIsUnknownIsNeverRetryable(t *testing.T) {
	t.Parallel()
	lost := UnknownOutcome("the request", OutcomeCause(io.EOF), io.EOF, Command{})
	assert.False(t, lost.WithRetryable(true).Retryable)
	assert.False(t, lost.WithMessage("other").WithRetryable(true).Retryable)
	assert.False(t, UnknownOutcomeUnreadable("the request", OutcomeCause(io.EOF), io.EOF, Command{}).WithRetryable(true).Retryable)
	assert.True(t, New(CodeGeneric, "x").WithRetryable(true).Retryable)
}

// TestOutcomeUnknownIsInTheEnvelopeOnlyWhenSet asserts the JSON field: an
// error of a write whose result is unknown carries outcomeUnknown true, and
// any other error has no such field.
func TestOutcomeUnknownIsInTheEnvelopeOnlyWhenSet(t *testing.T) {
	t.Parallel()
	decode := func(e *Error) map[string]any {
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}
	assert.Equal(t, true, decode(UnknownOutcome("the request", OutcomeCause(io.EOF), io.EOF, Command{}))["outcomeUnknown"])
	_, has := decode(New(CodeGeneric, "x"))["outcomeUnknown"]
	assert.False(t, has)
}

// TestTheJSONOfAnErrorWhoseResultIsUnknownSaysNotRetryable asserts that an
// error with outcomeUnknown is written with retryable false even when a
// caller set Retryable to true on it directly, and that any other error is
// written with its own Retryable.
func TestTheJSONOfAnErrorWhoseResultIsUnknownSaysNotRetryable(t *testing.T) {
	t.Parallel()
	lost := UnknownOutcome("the request", OutcomeCause(io.EOF), io.EOF, Command{})
	lost.Retryable = true
	assert.False(t, lost.RetryableInOutput())
	raw, err := json.Marshal(lost)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, false, m["retryable"])
	assert.Equal(t, true, m["outcomeUnknown"])

	plain := New(CodeGeneric, "x").WithRetryable(true)
	assert.True(t, plain.RetryableInOutput())
	raw, err = json.Marshal(plain)
	require.NoError(t, err)
	var pm map[string]any
	require.NoError(t, json.Unmarshal(raw, &pm))
	assert.Equal(t, true, pm["retryable"])
}

// TestOutcomeUnknownSurvivesWrapAndWithMessage asserts that a caller that
// re-words the error keeps the outcomeUnknown field.
func TestOutcomeUnknownSurvivesWrapAndWithMessage(t *testing.T) {
	t.Parallel()
	ce := UnknownOutcome("the request", OutcomeCause(io.EOF), io.EOF, Command{})
	assert.True(t, ce.WithMessage("other").OutcomeUnknown)
	assert.True(t, Wrap(fmt.Errorf("step: %w", ce), "step failed").OutcomeUnknown)
	got, ok := errors.AsType[*Error](fmt.Errorf("step: %w", ce))
	require.True(t, ok)
	assert.True(t, got.OutcomeUnknown)
}
