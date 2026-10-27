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

// pollInterval is the bespoke poll helper's tick - a package var so tests
// can shrink it (resources.pollInterval's test-shrinkable pattern).
var pollInterval = 2 * time.Second

// defaultWaitTimeout is the bespoke verbs' default --timeout: a DELIBERATE
// divergence from the generated wait's 120s defaultWaitTimeout - annotation
// consumption takes seconds on a real server (both editions), unlike a
// provisioning wait.
const defaultWaitTimeout = 60 * time.Second

// waitOutcome is one poll's classification for waitForOIDCClient's caller-
// supplied check function.
type waitOutcome struct {
	// done, when true, ends the wait successfully with value (the caller's
	// extracted result, e.g. a new secret's key name).
	done  bool
	value string
	// terminal, when non-nil, ends the wait IMMEDIATELY with this
	// non-retryable error - an impossible or ambiguous observed shape a
	// concurrent actor produced must never be misdiagnosed by waiting to
	// timeout.
	terminal *clierr.Error
}

// waitForOIDCClient mirrors resources.waitFor's poll contract for the
// bespoke secret-lifecycle verbs - a separate seam because the generated
// --for grammar expresses only status.state/conditions, not these key-set
// predicates; it must never diverge from waitFor's
// semantics: a Get error retries unless it ends the wait, a NOT_FOUND fails
// fast exactly like waitFor (returned raw, unclassified), check's terminal
// verdict ends the wait immediately, and an ordinary timeout calls onTimeout
// with the last-observed object (nil if the client was never successfully
// observed) to build the caller's per-verb classified error.
//
// An error engine.WaitShouldStop accepts (an authentication or certificate
// failure) ends the wait at once, like waitFor. The verb has already written
// to the server when the wait starts, so the error goes through onAbort,
// which wraps it with the verb's committed details. A timeout names the last
// Get error: "; last error: <CODE>: <message>" ends the message onTimeout
// built. The last Get error is the one of the most recent Get that ended
// with the wait's context still live; a later Get that returns an object
// clears it.
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
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var last *unstructured.Unstructured
	var lastErr *clierr.Error
	for {
		obj, err := client.Get(deadline, gvr, "", name)
		switch {
		case err != nil && clierr.IsNotFound(err):
			return "", err
		case err != nil && engine.WaitShouldStop(err):
			return "", onAbort(err)
		case err != nil:
			// A Get cut by the wait's own deadline or by a cancel says nothing
			// about the server, so only a Get that ended with the context
			// still live is recorded.
			if deadline.Err() == nil {
				lastErr = clierr.From(err)
			}
		case err == nil:
			last = obj
			lastErr = nil
			// check is handed the SAME deadline-bound context the poll Get
			// above used, never the outer ctx: any secret read a check
			// performs (rotate-secret's per-tick correlation read) must be
			// bounded by the user's --timeout too, not run unbounded after
			// it has notionally expired.
			out := check(deadline, obj)
			if out.terminal != nil {
				return "", out.terminal
			}
			if out.done {
				return out.value, nil
			}
		}
		// Mirrors resources.waitFor's select exactly: deadline.Done() fires
		// on EITHER the timeout elapsing or ctx being canceled by the
		// caller - the two are not distinguished, matching waitFor's own
		// behavior of always reporting the deadline branch as a timeout.
		select {
		case <-deadline.Done():
			ce := onTimeout(last)
			if lastErr != nil {
				ce = ce.WithMessage(fmt.Sprintf("%s; last error: %s: %s", ce.Message, lastErr.Code, lastErr.Message))
			}
			return "", ce
		case <-ticker.C:
		}
	}
}
