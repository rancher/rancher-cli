package cli

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// InterruptContext returns the context a command runs under and the stop
// func that releases it. The first interrupt or termination signal ends
// the context and, with it, every request bound to it; the second ends the
// process through exit with 128 plus the signal's number, the shell
// convention (130 for an interrupt, 143 for a termination), because a path
// that did not honor the first signal would otherwise hold the terminal
// and nothing else can end the process while the signals are held. One
// channel serves both signals, registered before the context exists, so
// no signal can fall between the two.
func InterruptContext(parent context.Context, exit func(code int)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-sig:
			cancel()
		}
		select {
		case <-done:
		case s := <-sig:
			signal.Stop(sig)
			exit(exitCodeFor(s))
		}
	}()
	return ctx, sync.OnceFunc(func() {
		signal.Stop(sig)
		close(done)
		cancel()
	})
}

// exitCodeFor returns 128 plus the signal's number, or 130 for a signal
// without a number.
func exitCodeFor(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return 130
}
