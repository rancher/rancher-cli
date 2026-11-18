package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// pollInterval is how often the wait reads the management node when it
// cannot watch it. Tests shorten it and run serially.
var pollInterval = 5 * time.Second

// drainedCondition is the Drained condition of a management node.
type drainedCondition struct {
	present                                  bool
	typ, status, reason, message, lastUpdate string
}

// readDrained reads the Drained condition of a management node.
func readDrained(obj map[string]any) drainedCondition {
	conditions, _, _ := unstructured.NestedFieldNoCopy(obj, "status", "conditions")
	list, _ := conditions.([]any)
	for _, c := range list {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := cond["type"].(string); typ != "Drained" {
			continue
		}
		out := drainedCondition{present: true, typ: "Drained"}
		out.status, _ = cond["status"].(string)
		out.reason, _ = cond["reason"].(string)
		out.message, _ = cond["message"].(string)
		out.lastUpdate, _ = cond["lastUpdateTime"].(string)
		return out
	}
	return drainedCondition{}
}

// waitEnd is how a finished wait ended.
type waitEnd struct {
	// condition is the Drained condition of the object that ended a drain
	// wait with success.
	condition drainedCondition
	// refused is true when a read was refused with 403 and the wait ended
	// with no confirmation.
	refused bool
}

// nodeWait waits for Rancher to finish the request just written on a
// management node.
type nodeWait struct {
	action      kinds.NodeDrainAction
	client      engine.ClusterClient
	gvr         schema.GroupVersionResource
	t           target
	timeout     time.Duration
	timeoutText string
	// planned is the Drained condition the management node had before the
	// write.
	planned drainedCondition
	flags   drainFlags
	// evictedNamespaces are the namespaces of the pods the preview listed as
	// will be evicted.
	evictedNamespaces []string

	end waitEnd
	// stop is why the wait ended without success. The callbacks of the wait
	// record it and return errWaitStopped; run turns it into the error, so no
	// command is built in a callback.
	stop *waitStop
}

// stopKind is how a wait ended without success.
type stopKind int

const (
	// stopRetries: Rancher failed the drain and retries it until it is stopped.
	stopRetries stopKind = iota
	// stopFailed: Rancher ended the drain as failed.
	stopFailed
	// stopOther: any other end with an error whose hint is the command that
	// reads the management node.
	stopOther
	// stopAborted: a read failed in a way that ends the wait.
	stopAborted
)

// waitStop is the record of a wait that ended without success.
type waitStop struct {
	kind stopKind
	// message is the text of the error.
	message string
	// err is the read error of a stopAborted wait.
	err error
}

// errWaitStopped is what the callbacks of the wait return after they record a
// waitStop.
var errWaitStopped = errors.New("the wait ended")

func (w *nodeWait) stopWith(kind stopKind, message string) error {
	w.stop = &waitStop{kind: kind, message: message}
	return errWaitStopped
}

// updatedAfterPlan reports whether cond is newer than the condition read
// before the write, or no condition was read then.
func (w *nodeWait) updatedAfterPlan(cond drainedCondition) bool {
	return !w.planned.present || cond.lastUpdate != w.planned.lastUpdate
}

// run waits until Rancher reports how the request ended.
func (w *nodeWait) run(ctx context.Context) (waitEnd, error) {
	err := engine.WaitObject(ctx, w.client, engine.WaitParams{
		GVR:         w.gvr,
		Namespace:   w.t.id,
		Name:        w.t.mid,
		Timeout:     w.timeout,
		Interval:    pollInterval,
		SpacingFrom: engine.SpacingFromReturn,
		Observe:     w.observe,
		StopOn:      resources.ReadRefused,
		// A request cut by the wait's own deadline or by a cancel, with no
		// response, says nothing about the server.
		RecordErr: func(ctx context.Context, err error) bool {
			_, responded := clierr.HTTPStatus(err)
			return ctx.Err() == nil || responded
		},
		OnAbort: func(err error) error {
			if !resources.ReadRefused(err) {
				w.stop = &waitStop{kind: stopAborted, err: err}
				return errWaitStopped
			}
			w.end.refused = true
			return nil
		},
		OnCancel: func(context.Context, *unstructured.Unstructured, *clierr.Error) error {
			return w.stopWith(stopOther, w.cancelText())
		},
		OnTimeout: w.onTimeout,
	})
	switch {
	case errors.Is(err, errWaitStopped):
		return waitEnd{}, w.stopError()
	case clierr.IsNotFound(err):
		return waitEnd{}, w.committedError(w.removedText(), w.t.recovery())
	case err != nil:
		return waitEnd{}, err
	}
	return w.end, nil
}

// stopError is the error of the end the callbacks recorded: GENERIC and not
// retryable, with the committed details and the command to read next.
func (w *nodeWait) stopError() error {
	switch w.stop.kind {
	case stopRetries:
		return w.committedError(w.stop.message, stopDrainCommand(w.t.node, w.t.cluster))
	case stopFailed:
		return w.drainFailed(w.stop.message)
	case stopAborted:
		return w.abortedError(w.stop.err)
	}
	return w.committedError(w.stop.message, w.t.recovery())
}

// observe reads the management node and says whether the wait goes on, ends
// or fails.
func (w *nodeWait) observe(_ context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
	request, _, _ := unstructured.NestedString(obj.Object, "spec", "desiredNodeUnschedulable")
	cond := readDrained(obj.Object)
	if w.action == kinds.NodeStopDrain {
		return w.observeStop(request, cond)
	}
	return w.observeDrain(request, cond)
}

func (w *nodeWait) observeDrain(request string, cond drainedCondition) (engine.WaitOutcome, error) {
	switch request {
	case "drain":
		if cond.status == "False" && w.updatedAfterPlan(cond) {
			return engine.WaitContinue, w.stopWith(stopRetries, fmt.Sprintf("Rancher could not drain node %s: %s; it retries until you stop it", w.t.node, cond.message))
		}
		return engine.WaitContinue, nil
	case "":
		switch {
		case cond.status == "True":
			w.end.condition = cond
			return engine.WaitDone, nil
		case cond.status == "False" && w.updatedAfterPlan(cond):
			return engine.WaitContinue, w.stopWith(stopFailed, cond.message)
		}
		return engine.WaitContinue, nil
	}
	return engine.WaitContinue, w.stopWith(stopOther, fmt.Sprintf("another request changed node %s while it was draining: desiredNodeUnschedulable is %s", w.t.node, request))
}

func (w *nodeWait) observeStop(request string, cond drainedCondition) (engine.WaitOutcome, error) {
	switch request {
	case "stopDrain":
		return engine.WaitContinue, nil
	case "":
		if !cond.present {
			return engine.WaitDone, nil
		}
		return engine.WaitContinue, w.stopWith(stopOther, fmt.Sprintf("a new drain of node %s started after the stop", w.t.node))
	case "drain":
		return engine.WaitContinue, w.stopWith(stopOther, fmt.Sprintf("a drain of node %s started again", w.t.node))
	}
	return engine.WaitContinue, w.stopWith(stopOther, fmt.Sprintf("another request changed node %s: desiredNodeUnschedulable is %s", w.t.node, request))
}

// drainFailed is the error for a drain Rancher ended as failed: the node stays
// cordoned, and the hint is the drain command with the flag the message names.
func (w *nodeWait) drainFailed(message string) error {
	prose := fmt.Sprintf("drain of node %s failed: %s; the node stays cordoned", w.t.node, message)
	retry := drainRetryCommand(w.t.node, w.t.cluster, w.flags, message)
	if !strings.Contains(message, "did not complete within") || len(w.evictedNamespaces) == 0 {
		return w.committedError(prose, retry)
	}
	return w.committedError(pdbText(prose, w.t.cluster, w.evictedNamespaces), retry)
}

func (w *nodeWait) cancelText() string {
	if w.action == kinds.NodeStopDrain {
		return fmt.Sprintf("stopped waiting for the drain of node %s to stop; the stop was requested", w.t.node)
	}
	return fmt.Sprintf("stopped waiting for node %s to drain; the drain was requested", w.t.node)
}

func (w *nodeWait) removedText() string {
	if w.action == kinds.NodeStopDrain {
		return fmt.Sprintf("node %s was removed while its drain was stopping", w.t.node)
	}
	return fmt.Sprintf("node %s was removed while it was draining", w.t.node)
}

// onTimeout records the end of a wait that ran out of time.
func (w *nodeWait) onTimeout(last *unstructured.Unstructured, lastErr *clierr.Error) error {
	if w.action == kinds.NodeDrain && last != nil {
		request, _, _ := unstructured.NestedString(last.Object, "spec", "desiredNodeUnschedulable")
		if request == "" && !readDrained(last.Object).present {
			return w.stopWith(stopOther, fmt.Sprintf("the drain request of node %s was removed and Rancher reported neither done nor failed: a stop-drain, an uncordon, or Rancher's node sync removed it", w.t.node))
		}
	}
	var msg string
	if w.action == kinds.NodeStopDrain {
		msg = fmt.Sprintf("timed out after %s waiting for the drain of node %s to stop; the stop was requested; last state: %s", w.timeoutText, w.t.node, lastState(last))
	} else {
		msg = fmt.Sprintf("timed out after %s waiting for node %s to drain; the drain was requested and goes on in Rancher; last state: %s", w.timeoutText, w.t.node, lastState(last))
	}
	if lastErr != nil {
		msg += fmt.Sprintf("; last error: %s: %s", lastErr.Code, lastErr.Message)
	}
	return w.stopWith(stopOther, msg)
}

// lastState describes the last management node the wait read.
func lastState(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "<no state observed>"
	}
	request, _, _ := unstructured.NestedString(obj.Object, "spec", "desiredNodeUnschedulable")
	if request == "" {
		request = `""`
	}
	cond := readDrained(obj.Object)
	drained := "absent"
	if cond.present {
		drained = cond.status
		if cond.message != "" {
			drained += " (" + cond.message + ")"
		}
	}
	return "desiredNodeUnschedulable " + request + ", Drained " + drained
}

// details are the committed details of an error after the request was
// accepted: the node, and the command that reads the management node.
func (w *nodeWait) details() clierr.CommittedContextDetails {
	return clierr.CommittedContextDetails{Committed: true, ResourceKind: "node", ResourceName: w.t.node, Recovery: w.t.recovery().Field()}
}

// committedError is the error that ends the wait with code GENERIC: the
// request was accepted, a second request is not the next step, and hint is
// the command to read next.
func (w *nodeWait) committedError(msg string, hint clierr.Command) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, msg).WithRetryable(false).WithHintCommand(hint).WithDetails(w.details())
}

// abortedError is an authentication or certificate failure during the wait:
// its code and hint are kept, and the command that reads the management node
// is the hint when it has none.
func (w *nodeWait) abortedError(err error) *clierr.Error {
	out := engine.CommittedPartialSuccess(err, w.details()).WithRetryable(false)
	if out.Hint == "" {
		return out.WithHintCommand(w.t.recovery())
	}
	if !w.t.recovery().OK() {
		out = out.WithMessage(clierr.WithLeftOutNote(out.Message))
	}
	return out
}
