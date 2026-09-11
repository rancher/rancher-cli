package clierr

import (
	"context"
	"errors"
)

// WrapUnlessCanceled returns err unchanged when it is the caller's own
// cancellation - errors.Is(err, context.Canceled) must keep working for
// every consumer - and otherwise returns wrap(err). Every command that
// dresses an error from a locked or deleting operation routes through
// this, so the cancellation-passthrough rule has one implementation
// instead of a hand-copied guard per call site.
func WrapUnlessCanceled(err error, wrap func(error) error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return wrap(err)
}
