package ui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func TestCanPrompt(t *testing.T) {
	t.Parallel()
	assert.True(t, New(nil, nil, false, true).CanPrompt())
	assert.False(t, New(nil, nil, true, true).CanPrompt(), "NoInput disables")
	assert.False(t, New(nil, nil, false, false).CanPrompt(), "no TTY disables")
}

func TestNoInput(t *testing.T) {
	t.Parallel()
	assert.True(t, New(nil, nil, true, true).NoInput())
	assert.False(t, New(nil, nil, false, true).NoInput())
	assert.True(t, Discard().NoInput())
}

func TestPromptReadsLineToErr(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("alice\n"), false, true)
	got, err := p.Prompt(t.Context(), "user: ")
	require.NoError(t, err)
	assert.Equal(t, "alice", got)
	assert.Equal(t, "user: ", errw.String(), "label goes to Err")
}

func TestPromptRefusesWhenDisabled(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader("x\n"), true, true)
	_, err := p.Prompt(t.Context(), "user: ")
	require.ErrorIs(t, err, ErrNoInput)
	_, err = p.PromptSecret(t.Context(), "pw: ")
	assert.ErrorIs(t, err, ErrNoInput)
}

func TestPromptSecretReadsLineFromBuffer(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("s3cret\n"), false, true)
	got, err := p.PromptSecret(t.Context(), "pw: ")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", got)
}

func TestPromptHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A reader that would block forever if read synchronously.
	p := New(&bytes.Buffer{}, blockingReader{}, false, true)
	_, err := p.Prompt(ctx, "user: ")
	assert.ErrorIs(t, err, context.Canceled, "cancelled ctx aborts the read")
}

func TestNoticeWritesToErr(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	New(&errw, nil, false, false).Notice("heads up")
	assert.Equal(t, "heads up\n", errw.String())
}

// A background goroutine (the OAuth loopback callback handler) calling
// Notice concurrently with the main goroutine's own Notice/Prompt calls
// must not race on the shared writer. Run with -race.
func TestNoticeIsSafeForConcurrentUse(t *testing.T) {
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader(""), true, false)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 200 {
				p.Notice("concurrent notice")
			}
		})
	}
	wg.Wait()
}

func TestDiscardRefuses(t *testing.T) {
	t.Parallel()
	_, err := Discard().Prompt(t.Context(), "x")
	assert.ErrorIs(t, err, ErrNoInput)
}

// TestSelectCancelableReturnsCancellationAndRunsOnCancel asserts the
// select seam behind readPasswordTTY: a cancelled ctx returns ctx.Err()
// immediately and runs onCancel, without waiting for the blocked read.
func TestSelectCancelableReturnsCancellationAndRunsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var onCancelCalled bool
	block := make(chan struct{})
	defer close(block)
	_, err := selectCancelable(ctx, func() (string, error) {
		<-block
		return "too late", nil
	}, func() { onCancelCalled = true })

	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, onCancelCalled, "onCancel must run on the cancellation path")
}

// TestSelectCancelableReturnsReadResult asserts the non-cancelled path
// returns the read's value and error untouched, without invoking onCancel.
func TestSelectCancelableReturnsReadResult(t *testing.T) {
	t.Parallel()
	var onCancelCalled bool
	got, err := selectCancelable(t.Context(), func() (string, error) {
		return "s3cret", nil
	}, func() { onCancelCalled = true })

	require.NoError(t, err)
	assert.Equal(t, "s3cret", got)
	assert.False(t, onCancelCalled)
}

// blockingReader blocks on Read until the test goroutine is torn down.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestReadLineReadsFromInputUngated(t *testing.T) {
	t.Parallel()
	// noInput=true, isTTY=false: ReadLine is NOT gated (unlike Prompt/PromptSecret).
	p := New(&bytes.Buffer{}, strings.NewReader("id:secret\n"), true, false)
	got, err := p.ReadLine(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "id:secret", got)
}

func TestReadLineEmptyIsEOF(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader(""), true, false)
	_, err := p.ReadLine(t.Context())
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadLineHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := New(&bytes.Buffer{}, blockingReader{}, true, false)
	_, err := p.ReadLine(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestReadLineCancellationEndsPromptLine asserts a cancelled read terminates
// the pending prompt line on the error writer, so the error text that renders
// next starts on its own line instead of after "Select [1]: ".
func TestReadLineCancellationEndsPromptLine(t *testing.T) {
	t.Parallel()
	var errBuf bytes.Buffer
	blocked, w := io.Pipe()
	defer func() { _ = w.Close() }()
	p := New(&errBuf, blocked, false, true)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := p.ReadLine(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "\n", errBuf.String(), "the open prompt line must be terminated")
}

// TestPrompterWritesNoticesAndLabelsWithoutControlCharacters asserts a
// Prompter with no rewrite writes this process's marker as it is and no
// escape character, carriage return or format character for a notice, a
// prompt label and a secret prompt label; the newlines of a notice stay.
func TestPrompterWritesNoticesAndLabelsWithoutControlCharacters(t *testing.T) {
	t.Parallel()
	text := "a\x1b[2Jb\r\nrun " + clierr.Rancher + " auth status\u202e\u2063\nlast"
	want := "a[2Jb\nrun " + clierr.Rancher + " auth status\nlast"
	t.Run("notice", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		New(&errw, strings.NewReader(""), false, true).Notice(text)
		assert.Equal(t, want+"\n", errw.String())
	})
	t.Run("prompt label", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).Prompt(t.Context(), text)
		require.NoError(t, err)
		assert.Equal(t, want, errw.String())
	})
	t.Run("secret prompt label", func(t *testing.T) {
		t.Parallel()
		var errw bytes.Buffer
		_, err := New(&errw, strings.NewReader("x\n"), false, true).PromptSecret(t.Context(), text)
		require.NoError(t, err)
		assert.Equal(t, want, errw.String())
	})
}

// TestPrompterRunsTheNoticeRewriteBeforeTheFilter asserts that a rewrite
// that appends an escape sequence leaves none in the written notice, that
// the rewrite sees the message as it was given, and that a label is not
// rewritten: only a notice is.
func TestPrompterRunsTheNoticeRewriteBeforeTheFilter(t *testing.T) {
	t.Parallel()
	var seen string
	rewrite := func(s string) string {
		seen = s
		return strings.ReplaceAll(s, clierr.Rancher, "rancher --config 'd'") + "\x1b[2J"
	}
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("x\n"), false, true).WithNoticeRewrite(rewrite)
	given := "run " + clierr.Rancher + " auth status\x1b[31m"
	p.Notice(given)
	assert.Equal(t, given, seen)
	assert.Equal(t, "run rancher --config 'd' auth status[31m[2J\n", errw.String())

	errw.Reset()
	_, err := p.Prompt(t.Context(), "label "+clierr.Rancher+" x")
	require.NoError(t, err)
	assert.Equal(t, "label "+clierr.Rancher+" x", errw.String(), "a label is written as it is, with this process's marker")
}
