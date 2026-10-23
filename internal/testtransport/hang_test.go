package testtransport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hangTestWait = 10 * time.Second

// holdOutcome runs fn in a goroutine and reports what it did: the value it
// panicked with, or nil when it returned.
func holdOutcome(fn func()) <-chan any {
	out := make(chan any, 1)
	go func() {
		defer func() { out <- recover() }()
		fn()
	}()
	return out
}

// requireHeld asserts that the goroutine behind out has not finished.
func requireHeld(t *testing.T, out <-chan any) {
	t.Helper()
	select {
	case got := <-out:
		require.Fail(t, "the helper returned before its wait was over", "outcome: %v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func requireOutcome(t *testing.T, out <-chan any) any {
	t.Helper()
	select {
	case got := <-out:
		return got
	case <-time.After(hangTestWait):
		require.FailNow(t, "the helper did not finish")
		return nil
	}
}

func requestWithContext(ctx context.Context, t *testing.T) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
}

// TestHangAndAbortPanicsOnceTheContextEnds asserts that HangAndAbort holds
// until the request's context ends and then panics with
// http.ErrAbortHandler instead of returning.
func TestHangAndAbortPanicsOnceTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := holdOutcome(func() { HangAndAbort(requestWithContext(ctx, t)) })
	requireHeld(t, out)
	cancel()
	assert.Equal(t, http.ErrAbortHandler, requireOutcome(t, out))
}

// TestHangAndAbortPanicsForACancelledContext asserts that a context that
// ended before the call gives the same panic.
func TestHangAndAbortPanicsForACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := holdOutcome(func() { HangAndAbort(requestWithContext(ctx, t)) })
	assert.Equal(t, http.ErrAbortHandler, requireOutcome(t, out))
}

// TestHangUntilReleasedReturnsOnRelease asserts that the release form
// returns, with no panic, once its channel is closed.
func TestHangUntilReleasedReturnsOnRelease(t *testing.T) {
	release := make(chan struct{})
	out := holdOutcome(func() { HangUntilReleased(requestWithContext(t.Context(), t), release) })
	requireHeld(t, out)
	close(release)
	assert.Nil(t, requireOutcome(t, out))
}

// TestHangUntilReleasedPanicsOnCancellation asserts that the release form
// panics with http.ErrAbortHandler when the request's context ends before
// the release.
func TestHangUntilReleasedPanicsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := holdOutcome(func() { HangUntilReleased(requestWithContext(ctx, t), make(chan struct{})) })
	requireHeld(t, out)
	cancel()
	assert.Equal(t, http.ErrAbortHandler, requireOutcome(t, out))
}

// TestWaitUntilGoneReturnsOnceTheContextEnds asserts that the hook form
// holds until the request's context ends and then returns without a panic.
func TestWaitUntilGoneReturnsOnceTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := holdOutcome(func() { WaitUntilGone(requestWithContext(ctx, t)) })
	requireHeld(t, out)
	cancel()
	assert.Nil(t, requireOutcome(t, out))
}

// TestHangAndAbortSendsNoResponseToAClosedClient asserts, through a real
// connection, that a client which half-closes its side after sending a
// request reads end of stream or a reset and never a response, while the
// handler sees its request's context end.
func TestHangAndAbortSendsNoResponseToAClosedClient(t *testing.T) {
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(gone)
		HangAndAbort(r)
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(hangTestWait)))

	got, err := io.ReadAll(conn)
	if err != nil {
		if netErr, ok := errors.AsType[net.Error](err); ok {
			require.False(t, netErr.Timeout(), "the read timed out: the server neither answered nor closed the connection")
		}
	}
	assert.Empty(t, string(got), "the connection was aborted: no response is written")
	select {
	case <-gone:
	case <-time.After(hangTestWait):
		require.Fail(t, "the handler never saw its request's context end")
	}
}
