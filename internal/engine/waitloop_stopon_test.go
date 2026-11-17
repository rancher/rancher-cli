package engine

import (
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errStopped is what the test OnAbort returns for an error StopOn accepts.
var errStopped = errors.New("stopped")

// stopOn403 sets StopOn to accept a 403 and OnAbort to return errStopped
// for it, recording every error OnAbort gets.
func stopOn403(aborts *[]error) func(*WaitParams) {
	return func(p *WaitParams) {
		p.StopOn = func(err error) bool {
			status, ok := clierr.HTTPStatus(err)
			return ok && status == http.StatusForbidden
		}
		p.OnAbort = func(err error) error {
			*aborts = append(*aborts, err)
			if s, ok := clierr.HTTPStatus(err); ok && s == http.StatusForbidden {
				return errStopped
			}
			return err
		}
	}
}

// TestWaitObjectStopOnEndsAtAGetResponseItAccepts asserts a Get response
// StopOn accepts goes to OnAbort at once, with no retry.
func TestWaitObjectStopOnEndsAtAGetResponseItAccepts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{state: "pending"}, {err: statusErr(http.StatusForbidden)}, {state: "done"}}
		var aborts []error
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, stopOn403(&aborts))
		require.ErrorIs(t, res.err, errStopped)
		assert.Equal(t, "get@0 get@5", f.sequence())
		require.Len(t, aborts, 1)
	})
}

// TestWaitObjectStopOnNilRetriesAGetRefusal asserts that without StopOn a
// 403 on a Get is retried, as before StopOn existed.
func TestWaitObjectStopOnNilRetriesAGetRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{err: statusErr(http.StatusForbidden)}, {state: "done"}}
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, nil)
		require.NoError(t, res.err)
		assert.Equal(t, "get@0 get@5", f.sequence())
	})
}

// TestWaitObjectStopOnIgnoresAListRefusal asserts a 403 on the List still
// makes the wait poll when StopOn accepts a 403: StopOn sees only Get
// responses.
func TestWaitObjectStopOnIgnoresAListRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.lists = []answer{{err: statusErr(http.StatusForbidden)}}
		f.gets = []answer{{state: "pending"}, {state: "done"}}
		var aborts []error
		res := runLoop(t.Context(), f, time.Minute, stopOn403(&aborts))
		require.NoError(t, res.err)
		assert.Equal(t, "list@0 get@0 get@10", f.sequence())
		assert.Empty(t, aborts)
	})
}

// TestWaitObjectStopOnIgnoresAWatchOpenRefusal asserts a 403 on the watch
// open still makes the wait poll when StopOn accepts a 403.
func TestWaitObjectStopOnIgnoresAWatchOpenRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.streams = []streamScript{{err: statusErr(http.StatusForbidden)}}
		f.gets = []answer{{state: "done"}}
		var aborts []error
		res := runLoop(t.Context(), f, time.Minute, stopOn403(&aborts))
		require.NoError(t, res.err)
		assert.Equal(t, "list@0 watch@0 get@10", f.sequence())
		assert.Empty(t, aborts)
	})
}

// TestWaitObjectStopOnLeavesAnAuthenticationFailureToOnAbort asserts an
// authentication failure on a Get still ends the wait through OnAbort when
// StopOn does not accept it.
func TestWaitObjectStopOnLeavesAnAuthenticationFailureToOnAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeLoop(t)
		f.gets = []answer{{err: authErr()}}
		var aborts []error
		res := runLoop(t.Context(), getOnly{f: f}, time.Minute, stopOn403(&aborts))
		require.Error(t, res.err)
		assert.Equal(t, clierr.CodeAuth, clierr.From(res.err).Code)
		assert.Equal(t, "get@0", f.sequence())
		require.Len(t, aborts, 1)
	})
}
