package resources

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// deleteTimeoutDefault limits a delete's wait when --timeout is not
	// given; deleteTimeoutDefaultText is how messages print it.
	deleteTimeoutDefault     = 5 * time.Minute
	deleteTimeoutDefaultText = "5m"

	// ownTokenNote is the warning of a delete of the token this command
	// uses: once the server accepts the delete request, no read with that
	// token can confirm anything.
	ownTokenNote = "the server accepted the delete request for the token this command used; the command cannot read with a deleted token, and the deletion is not confirmed; " +
		"log in again before the next command (if RANCHER_CLI_TOKEN holds this token, set it to another token or unset it)"
)

// waitMode says whether a delete waits; its zero value is a DeleteWait that
// ParseDeleteWait did not make, which PlanFromFetched refuses.
type waitMode int

const (
	waitUnset waitMode = iota
	waitOn
	waitOff
)

// DeleteWait is how a delete waits after the server accepted it. Only
// ParseDeleteWait makes one; its methods return a changed copy.
type DeleteWait struct {
	mode    waitMode
	timeout time.Duration
	// timeoutText is --timeout as given, or the default, as messages
	// print it.
	timeoutText string
	// ownToken marks the delete of the token this command uses: nothing is
	// read after the delete.
	ownToken bool
	// also are objects waited for after the deleted one.
	also []deleteTarget
	// notWaited are the warnings for objects the command does not wait
	// for; each makes the result's waited false.
	notWaited []string
}

// ParseDeleteWait reads --wait and --timeout: the wait is on unless
// --wait=false is given, --timeout is a positive duration (default 5m),
// and --wait=false with --timeout is refused. Every delete calls it before
// any request.
func ParseDeleteWait(in runtime.Input) (DeleteWait, error) {
	if in.IsSet(flagWait) && !in.Bool(flagWait) {
		if in.String(flagTimeout) != "" {
			return DeleteWait{}, clierr.New(clierr.CodeValidation, "--wait=false conflicts with --timeout; remove one")
		}
		return DeleteWait{mode: waitOff}, nil
	}
	d, err := runtime.DurationFlag(in, flagTimeout, deleteTimeoutDefault, "10m")
	if err != nil {
		return DeleteWait{}, err
	}
	text := in.String(flagTimeout)
	if text == "" {
		text = deleteTimeoutDefaultText
	}
	return DeleteWait{mode: waitOn, timeout: d, timeoutText: text}, nil
}

// OwnToken marks the delete as deleting the token this command uses: the
// executor sends the delete request and reads nothing after it, whatever
// --wait says.
func (w DeleteWait) OwnToken() DeleteWait {
	w.ownToken = true
	return w
}

// AlsoWaitFor adds an object the wait waits for after the deleted one,
// within the same time limit.
func (w DeleteWait) AlsoWaitFor(t deleteTarget) DeleteWait {
	w.also = append(append([]deleteTarget(nil), w.also...), t)
	return w
}

// NotWaitedFor adds the warning for an object the command does not wait
// for; the result then says waited false.
func (w DeleteWait) NotWaitedFor(warning string) DeleteWait {
	w.notWaited = append(append([]string(nil), w.notWaited...), warning)
	return w
}

// deleteTarget is one object the wait after a delete waits for.
type deleteTarget struct {
	gvr      schema.GroupVersionResource
	ns, name string
	// uid is the uid the command read before the delete; "" when it read
	// none. An object of the same name with a different uid is a new object.
	uid string
	// kind is the kind name that messages and the committed details use.
	kind string
	// recovery is the get command that reads the object.
	recovery clierr.Command
	// readBefore is true when the command read the object before the
	// delete: a refused read after it means the delete took away the
	// caller's access, not that the caller never had it.
	readBefore bool
	// subjectKind and subjectName, when set, replace the target's kind and
	// name in the committed details of an error: when the wait for a
	// namespace of a project fails, the details name the project, which the
	// command did not delete.
	subjectKind, subjectName string
}

// newDeleteTarget is the target of the wait for obj, an object of kind k that
// the command read before it sent the delete.
func newDeleteTarget(k kinds.Kind, ns, name string, obj *unstructured.Unstructured, effectCluster string) deleteTarget {
	return deleteTarget{gvr: k.GVR, ns: ns, name: name, uid: string(obj.GetUID()), kind: k.Name,
		recovery: deleteRecovery(k, ns, name, effectCluster), readBefore: true}
}

// deleteRecovery builds the get command that reads the object just deleted,
// given its kind k, its name and its namespace ns, routed as the delete was:
// --cluster when get of the kind takes it and the delete had a cluster, and
// --namespace when the object has a namespace and get takes it. A kind with
// ProjectBackingNamespace never gets --cluster: its get command runs on the
// local cluster and names the project's backing namespace with --namespace.
func deleteRecovery(k kinds.Kind, ns, name, effectCluster string) clierr.Command {
	return routedCommand("get", getFlags(k), k, ns, name, effectCluster)
}

// routedCommand builds the command verb of the object name of kind k in
// namespace ns, whose flags are flags, routed as the delete was, by the
// rules of deleteRecovery.
func routedCommand(verb clierr.CommandWord, flags []runtime.ArgSpec, k kinds.Kind, ns, name, effectCluster string) clierr.Command {
	cmd := clierr.NewCommand(verb).Word(k.Word()).Arg(name)
	has := func(flag string) bool {
		for _, f := range flags {
			if f.Name == flag {
				return true
			}
		}
		return false
	}
	if has(FlagCluster) && !k.ProjectBackingNamespace && effectCluster != "" {
		cmd = cmd.FlagValue("--cluster", effectCluster)
	}
	if ns != "" && has(FlagNamespace) {
		cmd = cmd.FlagValue("--namespace", ns)
	}
	return cmd
}

// deleteEnd is how the wait after a delete ended: waited when every
// object was seen deleted, and a warning for each one whose wait ended
// without that.
type deleteEnd struct {
	waited   bool
	warnings []string
}

// waitDeleted waits for each target in order until it is deleted: the
// server responds NOT_FOUND, a watch reports it deleted, or an object of
// that name has a different uid from the one read before the delete. One
// time limit covers every target. A refused read of an object the command
// read before the delete ends that object's wait with a warning, and the
// next object is still waited for. A timeout, an interrupt, an
// authentication or certificate failure, or a refused read of an object the
// command did not read before ends the wait with an error that says the
// deletion was requested.
func waitDeleted(ctx context.Context, client engine.ClusterClient, targets []deleteTarget, deadline time.Time, timeoutText string) (deleteEnd, error) {
	end := deleteEnd{waited: true}
	for _, t := range targets {
		warning, err := t.wait(ctx, client, deadline, timeoutText)
		if err != nil {
			return deleteEnd{}, err
		}
		if warning != "" {
			end.waited = false
			end.warnings = append(end.warnings, warning)
		}
	}
	return end, nil
}

// wait waits for one target; it returns the warning of a wait that ended
// without seeing the object deleted.
func (t deleteTarget) wait(ctx context.Context, client engine.ClusterClient, deadline time.Time, timeoutText string) (string, error) {
	var warning string
	err := engine.WaitObject(ctx, client, engine.WaitParams{
		GVR:         t.gvr,
		Namespace:   t.ns,
		Name:        t.name,
		Timeout:     time.Until(deadline),
		Interval:    pollInterval,
		SpacingFrom: engine.SpacingFromReturn,
		// An object that still exists is not deleted, even with a
		// deletionTimestamp; one of the same name with a different uid is
		// a new object and the deleted one is gone.
		Observe: func(_ context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			if uid := string(obj.GetUID()); t.uid != "" && uid != "" && uid != t.uid {
				return engine.WaitDone, nil
			}
			return engine.WaitContinue, nil
		},
		OnDeleted: func(context.Context) (engine.WaitOutcome, error) { return engine.WaitDone, nil },
		StopOn:    ReadRefused,
		// A request cut by the wait's own deadline or by a cancel, with no
		// response, says nothing about the server.
		RecordErr: func(ctx context.Context, err error) bool {
			_, responded := clierr.HTTPStatus(err)
			return ctx.Err() == nil || responded
		},
		OnAbort: func(err error) error {
			if !ReadRefused(err) {
				return t.abortedError(err)
			}
			if !t.readBefore {
				return t.refusedReadError(err)
			}
			warning = fmt.Sprintf("your access to %s %s ended after the delete request; the deletion was requested and not confirmed", t.kind, t.name)
			return nil
		},
		OnCancel: func(context.Context, *unstructured.Unstructured, *clierr.Error) error {
			return t.committedError(fmt.Sprintf("stopped waiting for %s %s to be deleted; the deletion was requested", t.kind, t.name))
		},
		OnTimeout: func(last *unstructured.Unstructured, lastErr *clierr.Error) error {
			msg := fmt.Sprintf("timed out after %s waiting for %s %s to be deleted; the deletion was requested; last state: %s",
				timeoutText, t.kind, t.name, t.state(last))
			if lastErr != nil {
				msg += fmt.Sprintf("; last error: %s: %s", lastErr.Code, lastErr.Message)
			}
			return t.committedError(msg)
		},
	})
	if clierr.IsNotFound(err) {
		return "", nil
	}
	return warning, err
}

// ReadRefused reports a 403: the server responded and refused the read.
func ReadRefused(err error) bool {
	if status, ok := clierr.HTTPStatus(err); ok && status == http.StatusForbidden {
		return true
	}
	ce := clierr.From(err)
	return ce != nil && ce.Code == clierr.CodeRBAC
}

// state is the text that describes the last object the wait saw, as the
// timeout error prints it.
func (t deleteTarget) state(obj *unstructured.Unstructured) string {
	if obj == nil {
		return "<no state observed>"
	}
	ts := "<none>"
	if dt := obj.GetDeletionTimestamp(); dt != nil {
		ts = dt.UTC().Format(time.RFC3339)
	}
	finalizers := "<none>"
	if f := obj.GetFinalizers(); len(f) > 0 {
		finalizers = output.Printable(strings.Join(f, ", "))
	}
	s := fmt.Sprintf("deletionTimestamp %s, finalizers: %s", ts, finalizers)
	if t.gvr.Group == "" && t.gvr.Resource == "namespaces" {
		// A namespace says in its conditions what keeps it Terminating.
		engine.EachCondition(obj.Object, func(typ, status, _, message string) bool {
			if status != "True" {
				return true
			}
			s += ", condition " + output.Printable(typ) + "=True"
			if message != "" {
				s += " (" + output.Printable(message) + ")"
			}
			return true
		})
	}
	return s
}

func (t deleteTarget) committed() clierr.CommittedContextDetails {
	kind, name := t.kind, t.name
	if t.subjectKind != "" {
		kind, name = t.subjectKind, t.subjectName
	}
	return clierr.CommittedContextDetails{Committed: true, ResourceKind: kind, ResourceName: name, Recovery: t.recovery.Field()}
}

// committedError is the error that ends the wait with code GENERIC: the
// deletion was requested, a second delete is not the next step, and the get
// command is the hint.
func (t deleteTarget) committedError(msg string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, msg).WithRetryable(false).WithHintCommand(t.recovery).WithDetails(t.committed())
}

// abortedError is an authentication or certificate failure during the
// wait: its code and hint are kept, and the get command is the hint when
// it has none.
func (t deleteTarget) abortedError(err error) *clierr.Error {
	out := engine.CommittedPartialSuccess(err, t.committed()).WithRetryable(false)
	if out.Hint == "" {
		return out.WithHintCommand(t.recovery)
	}
	if !t.recovery.OK() {
		out = out.WithMessage(clierr.WithLeftOutNote(out.Message))
	}
	return out
}

// refusedReadError is a refused read of an object the command did not read
// before the delete: the caller may never have been able to read it, and
// the error keeps the response's code.
func (t deleteTarget) refusedReadError(err error) *clierr.Error {
	ce := clierr.From(err)
	return clierr.New(ce.Code, fmt.Sprintf("could not read %s %s after the delete request: %s; the deletion was requested and not confirmed", t.kind, t.name, ce.Message)).
		WithRetryable(false).WithHintCommand(t.recovery).WithDetails(t.committed()).WithCause(err)
}

// deleteResult is the result of an accepted delete: "deleted" when the
// wait saw every object deleted, else "deletion started".
func deleteResult(k kinds.Kind, name string, end deleteEnd) runtime.Result {
	verb := "deletion started"
	if end.waited {
		verb = "deleted"
	}
	res := runtime.MutationText(k.Name, name, verb)
	res.Kind = resultKind(k) + "Delete"
	res.Data = map[string]any{"deleted": name, "waited": end.waited}
	res.Warnings = end.warnings
	return res
}
