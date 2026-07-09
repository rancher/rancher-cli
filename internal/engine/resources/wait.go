package resources

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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

// parseWaitCondition parses --for into a waitCond. An empty expr applies the
// scope default: state=active for Management kinds (legacy semantics),
// condition=Ready for Downstream kinds (kubectl semantics). JSONPath is
// deliberately unsupported — that grammar belongs to `kubectl wait` via
// `rancher kubeconfig`.
func parseWaitCondition(forExpr string, k kinds.Kind) (waitCond, error) {
	if forExpr == "" {
		if k.Scope == kinds.Downstream {
			return waitCond{kind: "condition", name: "Ready", status: "True"}, nil
		}
		return waitCond{kind: "state", name: "active"}, nil
	}
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
		case err != nil && isNotFound(err):
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

func isNotFound(err error) bool {
	var ce *clierr.Error
	return errors.As(err, &ce) && ce.Code == clierr.CodeNotFound
}

func waitTimeoutErr(name string, last *unstructured.Unstructured, cond waitCond) *clierr.Error {
	summary := "<no state observed>"
	if last != nil {
		summary = summarizeState(last, cond)
	}
	return clierr.New(clierr.CodeGeneric,
		fmt.Sprintf("timed out waiting for %s; last state: %s", name, summary)).
		WithRetryable(true)
}

// waitSatisfied checks whether obj meets cond: state-kind matches
// status.state exactly and (when the field exists) requires
// status.transitioning != "yes"; condition-kind matches any
// status.conditions[] entry with the target type+status.
func waitSatisfied(obj *unstructured.Unstructured, cond waitCond) bool {
	switch cond.kind {
	case "state":
		state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
		if state != cond.name {
			return false
		}
		if transitioning, ok, _ := unstructured.NestedString(obj.Object, "status", "transitioning"); ok && transitioning == "yes" {
			return false
		}
		return true
	case "condition":
		status, ok := conditionStatus(obj, cond.name)
		return ok && status == cond.status
	default:
		return false
	}
}

// conditionStatus returns the status of the first status.conditions[] entry
// whose type matches name, and whether one was found — the scan shared by
// waitSatisfied and summarizeState.
func conditionStatus(obj *unstructured.Unstructured, name string) (string, bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(cm, "type"); t == name {
			s, _, _ := unstructured.NestedString(cm, "status")
			return s, true
		}
	}
	return "", false
}

// summarizeState renders the last-observed signal for the timeout error.
func summarizeState(obj *unstructured.Unstructured, cond waitCond) string {
	switch cond.kind {
	case "state":
		s, ok, _ := unstructured.NestedString(obj.Object, "status", "state")
		if !ok || s == "" {
			return "<no state>"
		}
		return s
	case "condition":
		if status, ok := conditionStatus(obj, cond.name); ok {
			return fmt.Sprintf("%s=%s", cond.name, status)
		}
		return fmt.Sprintf("%s not present", cond.name)
	default:
		return ""
	}
}
