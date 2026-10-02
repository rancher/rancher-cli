package config

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errTransient     = errors.New("transient")
	errBudgetIgnored = errors.New("budget ignored")
)

// attemptCap ends an op that never clears, standing in for a hang if
// retryTransient ignored its budget. The sleep grows by a factor of about
// 1.47 per attempt (the geometric mean of 1+U, U uniform in [0,1)), so a
// 2 s budget takes about 20 attempts; 60 is far past any real run while
// the sleep is still far from overflowing.
const attemptCap = 60

func isErrTransient(err error) bool { return errors.Is(err, errTransient) }

// TestRetryTransientStopsOnSuccessOrOtherErrors asserts that a success or
// an error the classifier rejects ends the loop on that attempt: a real
// failure is returned as it is and never waits out the budget.
func TestRetryTransientStopsOnSuccessOrOtherErrors(t *testing.T) {
	t.Parallel()
	errReal := errors.New("permission denied on the temporary file")
	for _, tc := range []struct {
		name    string
		results []error
		want    error
	}{
		{"first attempt succeeds", []error{nil}, nil},
		{"first attempt fails for real", []error{errReal}, errReal},
		{"succeeds after transient errors", []error{errTransient, errTransient, nil}, nil},
		{"fails for real after transient errors", []error{errTransient, errReal}, errReal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				start := time.Now()
				err := retryTransient(time.Minute, isErrTransient, func() error {
					calls++
					if calls > len(tc.results) {
						return errBudgetIgnored
					}
					return tc.results[calls-1]
				})
				assert.Equal(t, tc.want, err)
				assert.Len(t, tc.results, calls, "one attempt per result, none after the last")
				assert.Less(t, time.Since(start), time.Second, "the budget is not waited out")
			})
		})
	}
}

// TestRetryTransientZeroBudgetMakesOneAttempt asserts the seam the
// Windows tests use to disable the retry.
func TestRetryTransientZeroBudgetMakesOneAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		err := retryTransient(0, isErrTransient, func() error {
			calls++
			if calls > attemptCap {
				return errBudgetIgnored
			}
			return errTransient
		})
		require.ErrorIs(t, err, errTransient)
		assert.Equal(t, 1, calls)
	})
}

// TestRetryTransientStopsAtTheBudget asserts that an error that never
// clears is returned once the budget runs out: after more than one
// attempt, no earlier than half the budget and never past it. Each sleep
// is shorter than 1 ms plus all the sleeps before it, so the loop cannot
// stop before it has slept about half the budget.
func TestRetryTransientStopsAtTheBudget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const budget = 2 * time.Second
		calls := 0
		start := time.Now()
		err := retryTransient(budget, isErrTransient, func() error {
			calls++
			if calls > attemptCap {
				return errBudgetIgnored
			}
			return errTransient
		})
		elapsed := time.Since(start)
		require.ErrorIs(t, err, errTransient)
		assert.Greater(t, calls, 1, "retried before giving up")
		assert.GreaterOrEqual(t, elapsed, budget/2-time.Millisecond, "kept retrying for at least half the budget")
		assert.Less(t, elapsed, budget, "never started an attempt past the budget")
	})
}

// TestRetryTransientReturnsTheLowestErrno asserts that of the errors seen
// before the budget ran out, the one with the lowest errno is returned,
// and an error without one only when no attempt carried an errno. The
// lowest errno comes early and a higher one repeats to the end, so a
// loop that kept the last error would fail on every run.
func TestRetryTransientReturnsTheLowestErrno(t *testing.T) {
	t.Parallel()
	transient := func(err error) bool { return !errors.Is(err, errBudgetIgnored) }
	pathErr := func(errno syscall.Errno) error {
		return &os.PathError{Op: "open", Path: "f", Err: errno}
	}

	synctest.Test(t, func(t *testing.T) {
		results := []error{errTransient, pathErr(32), pathErr(5)}
		calls := 0
		err := retryTransient(time.Second, transient, func() error {
			calls++
			switch {
			case calls <= len(results):
				return results[calls-1]
			case calls > attemptCap:
				return errBudgetIgnored
			}
			return pathErr(32)
		})
		require.ErrorIs(t, err, syscall.Errno(5))

		calls = 0
		err = retryTransient(time.Second, transient, func() error {
			calls++
			if calls > attemptCap {
				return errBudgetIgnored
			}
			return errTransient
		})
		require.ErrorIs(t, err, errTransient)
	})
}
