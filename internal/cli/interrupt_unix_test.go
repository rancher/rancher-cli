//go:build !windows

package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestInterruptContextEndsOnFirstSignalAndExitsOnSecond asserts that the
// first signal ends the context and no earlier signal exits, and that the
// very next signal, sent as soon as the context has ended, exits with 128
// plus its number. The test sends the signals to its own process, which
// the context holds for the duration.
func TestInterruptContextEndsOnFirstSignalAndExitsOnSecond(t *testing.T) {
	exited := make(chan int, 1)
	ctx, stop := InterruptContext(context.Background(), func(code int) { exited <- code })
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first interrupt did not end the context")
	}
	select {
	case code := <-exited:
		t.Fatalf("exited with %d on the first interrupt", code)
	default:
	}

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case code := <-exited:
		require.Equal(t, 143, code)
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not end the process")
	}
}

// TestInterruptContextStopReleasesTheSignals asserts that stop ends the
// context and releases the signal channel, so a later signal is not acted
// on by the helper; the test restores the default handler through the
// same call and sends nothing after it.
func TestInterruptContextStopReleasesTheSignals(t *testing.T) {
	exited := make(chan int, 1)
	ctx, stop := InterruptContext(context.Background(), func(code int) { exited <- code })
	stop()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	stop()
	select {
	case code := <-exited:
		t.Fatalf("exited with %d without a signal", code)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestExitCodeForSignal asserts the shell convention for the two signals
// the process listens for.
func TestExitCodeForSignal(t *testing.T) {
	t.Parallel()
	require.Equal(t, 130, exitCodeFor(syscall.SIGINT))
	require.Equal(t, 143, exitCodeFor(syscall.SIGTERM))
}
