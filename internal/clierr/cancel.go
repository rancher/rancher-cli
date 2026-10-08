package clierr

import (
	"context"
	"errors"
)

// WrapUnlessCanceled returns err unchanged when it is the caller's own
// cancellation - errors.Is(err, context.Canceled) keeps working for the
// error's consumers - and otherwise returns wrap(err). A call site that
// dresses an error from a locked or deleting operation, and has nothing
// more to report on a cancellation, routes through this, so the
// passthrough rule has one implementation instead of a hand-copied guard
// per call site. A command that has partial results to report on a
// cancellation returns INTERRUPTED with them instead.
func WrapUnlessCanceled(err error, wrap func(error) error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return wrap(err)
}
