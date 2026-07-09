package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	got, err := p.Prompt(context.Background(), "user: ")
	require.NoError(t, err)
	assert.Equal(t, "alice", got)
	assert.Equal(t, "user: ", errw.String(), "label goes to Err")
}

func TestPromptRefusesWhenDisabled(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader("x\n"), true, true)
	_, err := p.Prompt(context.Background(), "user: ")
	assert.ErrorIs(t, err, ErrNoInput)
	_, err = p.PromptSecret(context.Background(), "pw: ")
	assert.ErrorIs(t, err, ErrNoInput)
}

func TestPromptSecretReadsLineFromBuffer(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader("s3cret\n"), false, true)
	got, err := p.PromptSecret(context.Background(), "pw: ")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", got)
}

func TestPromptHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A reader that would block forever if read synchronously.
	p := New(&bytes.Buffer{}, blockingReader{}, false, true)
	_, err := p.Prompt(ctx, "user: ")
	assert.True(t, errors.Is(err, context.Canceled), "cancelled ctx aborts the read")
}

func TestNoticeWritesToErr(t *testing.T) {
	t.Parallel()
	var errw bytes.Buffer
	New(&errw, nil, false, false).Notice("heads up")
	assert.Equal(t, "heads up\n", errw.String())
}

// Round-3 repro: a background goroutine (the OAuth loopback callback
// handler) calling Notice concurrently with the main goroutine's own
// Notice/Prompt calls raced on the shared writer with no synchronization.
// Run with -race; fails on the unguarded implementation.
func TestNoticeIsSafeForConcurrentUse(t *testing.T) {
	var errw bytes.Buffer
	p := New(&errw, strings.NewReader(""), true, false)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				p.Notice("concurrent notice")
			}
		}()
	}
	wg.Wait()
}

func TestDiscardRefuses(t *testing.T) {
	t.Parallel()
	_, err := Discard().Prompt(context.Background(), "x")
	assert.ErrorIs(t, err, ErrNoInput)
}

// blockingReader blocks on Read until the test goroutine is torn down.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestReadLineReadsFromInputUngated(t *testing.T) {
	t.Parallel()
	// noInput=true, isTTY=false: ReadLine is NOT gated (unlike Prompt/PromptSecret).
	p := New(&bytes.Buffer{}, strings.NewReader("id:secret\n"), true, false)
	got, err := p.ReadLine(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "id:secret", got)
}

func TestReadLineEmptyIsEOF(t *testing.T) {
	t.Parallel()
	p := New(&bytes.Buffer{}, strings.NewReader(""), true, false)
	_, err := p.ReadLine(context.Background())
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadLineHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(&bytes.Buffer{}, blockingReader{}, true, false)
	_, err := p.ReadLine(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
