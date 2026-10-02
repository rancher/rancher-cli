package config

import (
	"errors"
	"math/rand/v2"
	"syscall"
	"time"
)

// retryTransient calls op until it succeeds, returns an error transient
// rejects, or the next attempt would start past budget. The sleep
// between attempts starts at 1 ms and grows by a random amount up to
// doubling, so callers contending for one file spread out. A budget of
// zero makes exactly one attempt. When the budget runs out it returns the
// error with the lowest errno seen, the rule the Go toolchain's own retry
// (cmd/internal/robustio) uses; an error that carries no errno is
// returned only when no attempt produced one.
func retryTransient(budget time.Duration, transient func(error) bool, op func() error) error {
	var (
		best      error
		bestErrno syscall.Errno
		start     time.Time
		sleep     = time.Millisecond
	)
	for {
		err := op()
		if err == nil || !transient(err) {
			return err
		}
		if errno, ok := errors.AsType[syscall.Errno](err); ok && (bestErrno == 0 || errno < bestErrno) {
			best, bestErrno = err, errno
		} else if best == nil {
			best = err
		}
		if start.IsZero() {
			start = time.Now()
		}
		if time.Since(start)+sleep >= budget {
			return best
		}
		time.Sleep(sleep)
		sleep += rand.N(sleep)
	}
}
