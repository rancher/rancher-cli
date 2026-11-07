package oidcclient

import (
	"context"
	"fmt"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pollInterval is the Interval of the wait: the spacing of one request. A
// package variable so tests can shrink it.
var pollInterval = 2 * time.Second

// defaultWaitTimeout is the verbs' default --timeout. It differs from the
// generated wait's 120 seconds on purpose: the controller consumes an
// annotation within seconds on a real server, unlike a provisioning wait.
const defaultWaitTimeout = 60 * time.Second

// waitOutcome is what a verb's check function decided about one OIDCClient.
type waitOutcome struct {
	// done, when true, ends the wait successfully with value (the verb's
	// result, for example the name of a new client secret).
	done  bool
	value string
	// terminal, when non-nil, ends the wait at once with this error: a shape
	// another actor produced that waiting longer cannot fix.
	terminal *clierr.Error
	// checkAgain, when true, runs check again on this object every poll
	// interval while the watch is open: the verdict depends on a request the
	// watch does not see.
	checkAgain bool
}

// waitForOIDCClient waits on the OIDCClient name with engine.WaitObject, so it
// watches the object, and polls when the server refuses the watch. check
// decides about each object the wait sees. An error engine.WaitShouldStop
// accepts (an authentication or certificate failure) ends the wait at once
// through onAbort, which adds the verb's committed details, because the verb
// has already written to the server when the wait starts. A NOT_FOUND answer
// ends the wait with the error as it is. A timeout, and a cancel too, calls
// onTimeout with the last object seen (nil when none) for the verb's own error
// and appends "; last error: <CODE>: <message>" when a request failed and no
// object came after it. A request cut by the deadline or by a cancel is never
// that last error. check gets the wait's context, which the timeout bounds.
func waitForOIDCClient(
	ctx context.Context,
	client engine.ClusterClient,
	gvr schema.GroupVersionResource,
	name string,
	timeout time.Duration,
	check func(ctx context.Context, obj *unstructured.Unstructured) waitOutcome,
	onTimeout func(last *unstructured.Unstructured) *clierr.Error,
	onAbort func(err error) *clierr.Error,
) (string, error) {
	var value string
	timedOut := func(last *unstructured.Unstructured, lastErr *clierr.Error) error {
		ce := onTimeout(last)
		if lastErr != nil {
			ce = ce.WithMessage(fmt.Sprintf("%s; last error: %s: %s", ce.Message, lastErr.Code, lastErr.Message))
		}
		return ce
	}
	err := engine.WaitObject(ctx, client, engine.WaitParams{
		GVR:         gvr,
		Name:        name,
		Timeout:     timeout,
		Interval:    pollInterval,
		SpacingFrom: engine.SpacingFromStart,
		Observe: func(ctx context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			out := check(ctx, obj)
			switch {
			case out.terminal != nil:
				return engine.WaitContinue, out.terminal
			case out.done:
				value = out.value
				return engine.WaitDone, nil
			case out.checkAgain:
				return engine.WaitCheckAgain, nil
			}
			return engine.WaitContinue, nil
		},
		RecordErr: func(ctx context.Context, _ error) bool { return ctx.Err() == nil },
		OnAbort:   func(err error) error { return onAbort(err) },
		OnCancel: func(_ context.Context, last *unstructured.Unstructured, lastErr *clierr.Error) error {
			return timedOut(last, lastErr)
		},
		OnTimeout: timedOut,
	})
	if err != nil {
		return "", err
	}
	return value, nil
}
