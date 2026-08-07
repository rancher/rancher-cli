package resources

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var pollInterval = 5 * time.Second // package var: tests shrink it

// waitCond is a parsed --for expression: either a status.state equality or a
// status.conditions[] type+status match.
type waitCond struct {
	kind   string // "condition" | "state"
	name   string
	status string // conditions only; default "True"
}

// isStalledTarget reports whether the caller is explicitly waiting FOR the
// Stalled condition itself — the one case where seeing it is the answer,
// not an abort, and where a fresh satisfied result is the controller's real
// verdict rather than a birth-time default. Shared by the abort check and
// the fresh-success advisory gate so the exemption cannot drift.
func (c waitCond) isStalledTarget() bool {
	return c.kind == "condition" && c.name == "Stalled"
}

// parseWaitCondition parses a --for expression (either the user's explicit
// flag or a kind's WaitDefault) into a waitCond. JSONPath is deliberately
// unsupported — that grammar belongs to `kubectl wait` via `rancher
// kubeconfig`. Callers resolve the empty-expression default (if any) via
// kinds.Kind.WaitDefault before calling; an empty expr here is invalid.
func parseWaitCondition(forExpr string) (waitCond, error) {
	if rest, ok := strings.CutPrefix(forExpr, "state="); ok && rest != "" {
		return waitCond{kind: "state", name: rest}, nil
	}
	if rest, ok := strings.CutPrefix(forExpr, "condition="); ok && rest != "" {
		name, status, hasStatus := strings.Cut(rest, "=")
		if name == "" {
			return waitCond{}, invalidForErr(forExpr)
		}
		switch {
		case !hasStatus:
			status = "True"
		case status == "":
			// "condition=<Type>=" with nothing after the second "=" — a real
			// condition status is never empty, so this would wait forever.
			return waitCond{}, invalidForErr(forExpr)
		}
		return waitCond{kind: "condition", name: name, status: status}, nil
	}
	return waitCond{}, invalidForErr(forExpr)
}

func invalidForErr(forExpr string) *clierr.Error {
	return clierr.New(clierr.CodeValidation,
		fmt.Sprintf("--for %q is not a supported wait grammar", forExpr)).
		WithHint("use state=<value> or condition=<Type>[=<Status>]; JSONPath is not supported — use `kubectl wait` via `rancher kubeconfig` for that")
}

// waitFor polls client.Get every pollInterval until cond is satisfied, the
// object reports a Rancher fail-fast (status.transitioning == "error"), or
// timeout elapses. A NOT_FOUND Get error fails immediately (the target is
// gone, not slow); any other Get error is treated as transient and retried
// with the last-observed object carried into the eventual timeout message.
// Fail-fast returns immediately with a non-retryable error; a timeout always
// returns a retryable error carrying the last-observed state, never a raw Get
// error.
func waitFor(ctx context.Context, client engine.ClusterClient, gvr schema.GroupVersionResource, ns, name string, cond waitCond, timeout time.Duration, failOnStalled bool) (*unstructured.Unstructured, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *unstructured.Unstructured
	for {
		obj, err := client.Get(ctx, gvr, ns, name)
		switch {
		case err != nil && clierr.IsNotFound(err):
			return nil, err
		case err != nil:
			// Transient error: fall through to the poll-or-timeout select below,
			// carrying whatever state was last observed into the eventual message.
		default:
			last = obj
			// The generation staleness gate applies only to kstatus-opted
			// kinds (failOnStalled=true). A controller that has not
			// reconciled the object's current generation yet has written
			// none of the conditions we are about to trust — a freshly
			// created object can carry a default Ready=True before its
			// controller ever runs. When the object tracks
			// observedGeneration and it lags, nothing on it is meaningful:
			// keep polling. Objects without the field (no generation
			// tracking) are always considered settled.
			//
			// Non-opted kinds (failOnStalled=false) keep pre-branch
			// behaviour: transitioning=error and waitSatisfied are checked
			// on every poll regardless of generation bookkeeping.
			// TestWaitForSkipsUnsettledGenerations and
			// TestWaitForUnsettledUnstalledSucceedsImmediately pin the two
			// sides of this invariant.
			if !failOnStalled || settled(obj) {
				if transitioning, _, _ := unstructured.NestedString(obj.Object, "status", "transitioning"); transitioning == "error" {
					msg, _, _ := unstructured.NestedString(obj.Object, "status", "transitioningMessage")
					return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("%s failed: %s", name, msg))
				}
				// Stalled=True is the kstatus abort convention (Fleet sets it on a
				// failed GitRepo clone while still reporting Ready=True), so it
				// fails the wait even when the target condition reads satisfied —
				// unless Stalled itself is what the caller is waiting for. Opt-in
				// per kind row (WaitFailsOnStalled): not every controller treats
				// Stalled as terminal, so a kind must be known-kstatus to abort.
				if failOnStalled && !cond.isStalledTarget() {
					if status, msg, ok := conditionStatus(obj.Object, "Stalled"); ok && status == "True" {
						if msg == "" {
							msg = "condition Stalled=True"
						}
						return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("%s stalled: %s", name, engine.Printable(msg)))
					}
				}
				if waitSatisfied(obj, cond) {
					return obj, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, waitTimeoutErr(name, last, cond, failOnStalled)
		case <-time.After(pollInterval):
		}
	}
}

func waitTimeoutErr(name string, last *unstructured.Unstructured, cond waitCond, failOnStalled bool) *clierr.Error {
	summary := "<no state observed>"
	if last != nil {
		summary = summarizeState(last, cond)
		if failOnStalled && !settled(last) {
			summary += " (from an older generation, not yet reconciled)"
		}
	}
	return clierr.Retryable(clierr.CodeGeneric,
		fmt.Sprintf("timed out waiting for %s; last state: %s", name, summary)).
		WithHint("retry with a longer --timeout or inspect the resource for what is holding it back")
}

// resolvedState reads status.state (the Rancher idiom); when absent it falls
// back to status.phase (the core Kubernetes idiom, e.g. a Pod's "Running"),
// reporting which field supplied the value so callers can gate the
// Rancher-only status.transitioning check on the status.state path.
func resolvedState(obj *unstructured.Unstructured) (value string, fromState bool) {
	if s, ok, _ := unstructured.NestedString(obj.Object, "status", "state"); ok && s != "" {
		return s, true
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, false
}

// waitSatisfied checks whether obj meets cond: state-kind matches
// status.state exactly, falling back to a case-insensitive match against
// status.phase when status.state is absent (phase values are capitalized,
// e.g. "Running" — exact status.state values are Rancher's own vocabulary
// and stay exact-match), and (when matched via status.state) requires
// status.transitioning != "yes"; condition-kind matches any
// status.conditions[] entry with the target type+status.
func waitSatisfied(obj *unstructured.Unstructured, cond waitCond) bool {
	switch cond.kind {
	case "state":
		state, fromState := resolvedState(obj)
		if fromState {
			if state != cond.name {
				return false
			}
			if transitioning, ok, _ := unstructured.NestedString(obj.Object, "status", "transitioning"); ok && transitioning == "yes" {
				return false
			}
			return true
		}
		return strings.EqualFold(state, cond.name)
	case "condition":
		status, _, ok := conditionStatus(obj.Object, cond.name)
		return ok && status == cond.status
	default:
		return false
	}
}

// conditionStatus returns the status and message of the first
// status.conditions[] entry whose type matches name, and whether one was
// found — the scan shared by waitSatisfied and summarizeState. Rides
// engine.EachCondition for the shared decoding logic; stops at the first
// match.
func conditionStatus(obj map[string]any, name string) (status, message string, ok bool) {
	engine.EachCondition(obj, func(typ, s, _, msg string) bool {
		if typ != name {
			return true
		}
		status, message, ok = s, msg, true
		return false
	})
	return status, message, ok
}

// summarizeState renders the last-observed signal for the timeout error,
// mirroring waitSatisfied's status.state -> status.phase fallback so the
// message shows the real phase when a kind only carries one.
func summarizeState(obj *unstructured.Unstructured, cond waitCond) string {
	switch cond.kind {
	case "state":
		s, _ := resolvedState(obj)
		if s == "" {
			return "<no state>"
		}
		return s
	case "condition":
		if status, message, ok := conditionStatus(obj.Object, cond.name); ok {
			if message != "" {
				return fmt.Sprintf("%s=%s (%s)", cond.name, status, engine.Printable(message))
			}
			return fmt.Sprintf("%s=%s", cond.name, status)
		}
		return fmt.Sprintf("%s not present", cond.name)
	default:
		return ""
	}
}

// settled reports whether the object's controller has reconciled its
// current generation: when status.observedGeneration is present and lags
// metadata.generation, every condition describes an older spec and cannot
// be trusted. Objects without the field report settled — plenty of kinds
// never track generations.
//
// Omitempty reality: a freshly created object whose controller never ran
// usually serializes NO observedGeneration (int64 omitempty zero), so an
// absent-field object passes this gate as settled. That birth window is
// deliberately covered by freshSuccessWarning, not by this gate; the two
// are a pair.
func settled(obj *unstructured.Unstructured) bool {
	og, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	if !found {
		return true
	}
	return og >= obj.GetGeneration()
}

// freshSuccessWarning covers the window generation tracking cannot: a
// kstatus controller may abort into Stalled only after retry backoff
// (Fleet flips a bad GitRepo ~15s post-create), while the object carries
// a default satisfied condition from birth. A wait that succeeds while
// the object is younger than freshWindow gets an advisory warning naming
// the re-check, because the success may predate the controller's verdict.
// Applied only to kinds that opted into Stalled semantics.
const freshWindow = 30 * time.Second

func freshSuccessWarning(obj *unstructured.Unstructured, kindName, name, ns string) string {
	created := obj.GetCreationTimestamp()
	if created.IsZero() {
		return ""
	}
	elapsed := time.Since(created.Time)
	if elapsed > freshWindow {
		return ""
	}
	var when string
	if elapsed < time.Second {
		// Covers both sub-second-old objects and a future creationTimestamp
		// from server/client clock skew (elapsed goes negative).
		when = "immediately after creation"
	} else {
		when = waitElapsed(elapsed) + " after creation"
	}
	recheck := fmt.Sprintf("rancher get %s %s", kindName, name)
	if ns != "" {
		// The re-check must target the same namespace the wait resolved —
		// a copy-pasted command falling back to the kind's default
		// workspace could 404 or, worse, report a same-named stranger.
		recheck += " --namespace " + ns
	}
	return fmt.Sprintf("wait satisfied %s — a fresh %s's status may predate its controller's verdict; re-check shortly with: %s",
		when, kindName, recheck)
}
