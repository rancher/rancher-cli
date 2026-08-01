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
func waitFor(ctx context.Context, client engine.ClusterClient, gvr schema.GroupVersionResource, ns, name string, cond waitCond, timeout time.Duration) (*unstructured.Unstructured, error) {
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
			if transitioning, _, _ := unstructured.NestedString(obj.Object, "status", "transitioning"); transitioning == "error" {
				msg, _, _ := unstructured.NestedString(obj.Object, "status", "transitioningMessage")
				return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("%s failed: %s", name, msg))
			}
			if waitSatisfied(obj, cond) {
				return obj, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, waitTimeoutErr(name, last, cond)
		case <-time.After(pollInterval):
		}
	}
}

func waitTimeoutErr(name string, last *unstructured.Unstructured, cond waitCond) *clierr.Error {
	summary := "<no state observed>"
	if last != nil {
		summary = summarizeState(last, cond)
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
