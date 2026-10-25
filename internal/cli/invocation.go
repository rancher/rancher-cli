package cli

import (
	"sync/atomic"

	"github.com/rancher/rancher-cli/internal/invocation"
)

// invocationContext is how the process was started, as main detected it.
var invocationContext atomic.Pointer[invocation.Context]

// SetInvocation sets how the process was started, which a recorded usage
// event carries, and returns a function that puts the previous value back.
// main sets it once, before BuildRoot. With no call, as in tests, the zero
// Context is used. Tests that call it must not run in parallel with tests
// that run a command.
func SetInvocation(c invocation.Context) (restore func()) {
	old := invocationContext.Swap(&c)
	return func() { invocationContext.Store(old) }
}

// currentInvocation returns what SetInvocation last set, or the zero
// Context.
func currentInvocation() invocation.Context {
	if c := invocationContext.Load(); c != nil {
		return *c
	}
	return invocation.Context{}
}
