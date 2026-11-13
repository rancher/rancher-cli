package catalog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The errors of the wait. Every one is returned after Rancher accepted the
// request: it is not retryable, since a second request runs Helm again, and
// carries clierr.CommittedContextDetails whose recovery is the command its
// hint names.

// committedError is the GENERIC error of the wait with msg and the remedy
// hint.
func (w *appWait) committedError(msg string, hint clierr.Command) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, msg).WithRetryable(false).WithHintCommand(hint).WithDetails(w.committed(hint.Field()))
}

func (w *appWait) committed(recovery string) clierr.CommittedContextDetails {
	return clierr.CommittedContextDetails{Committed: true, ResourceKind: "app", ResourceName: w.t.release, Recovery: recovery}
}

// passThroughError is err as run returns it: an error a builder made is
// kept, and any other error, one engine.WaitObject or a client returned as
// it is, keeps its code and hint, gains the committed details and is not
// retryable. Its recovery is its hint, or the get app command, which then
// becomes the hint, when it has none.
func (w *appWait) passThroughError(err error) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if ok {
		if _, committed := ce.Details.(clierr.CommittedContextDetails); committed && !ce.Retryable {
			return err
		}
	}
	if ok && ce.Hint != "" {
		return engine.CommittedPartialSuccess(err, w.committed(ce.Hint)).WithRetryable(false)
	}
	getApp := w.t.getApp()
	return engine.CommittedPartialSuccess(err, w.committed(getApp.Field())).WithRetryable(false).WithHintCommand(getApp)
}

// abortedError is an authentication or certificate failure during the
// wait: its code and hint are kept.
func (w *appWait) abortedError(err error) *clierr.Error {
	getApp := w.t.getApp()
	out := engine.CommittedPartialSuccess(err, w.committed(getApp.Field())).WithRetryable(false)
	if !getApp.OK() {
		out = out.WithMessage(clierr.WithLeftOutNote(out.Message))
	}
	return out
}

// cancelledError is the error of a wait the caller ended: the request was
// sent, and the operation may still run Helm.
func (w *appWait) cancelledError() *clierr.Error {
	msg := fmt.Sprintf("the wait for release %s ended before Helm's run did; operation %s in namespace %s may still run Helm",
		clierr.ShellQuote(w.t.release), clierr.ShellQuote(w.op.OperationName), clierr.ShellQuote(w.op.OperationNamespace))
	if w.t.requestID != "" {
		msg += fmt.Sprintf("; this request's id is %s, and an app whose spec.chart.metadata.annotations hold %s: %s is the revision this request created",
			w.t.requestID, annotationRequestID, w.t.requestID)
	}
	return w.committedError(msg, w.t.getApp())
}

// timeoutError is the error of a wait that ran out of time: the last state
// it read, the last error, and when the Operation step was skipped, what
// that hides.
func (w *appWait) timeoutError(lastErr *clierr.Error) *clierr.Error {
	msg := fmt.Sprintf("timed out after %s waiting for release %s; last state: %s",
		waitDuration(w.timeout), clierr.ShellQuote(w.t.release), w.summary())
	if lastErr != nil {
		msg += fmt.Sprintf("; last error: %s: %s", lastErr.Code, w.clean(lastErr.Message))
	}
	if w.skip {
		msg += "; " + w.skipFact()
	}
	return w.committedError(msg, w.t.getApp())
}

// failureError is the error of this request's revision at failed: Helm's
// error from spec.info.description, the status, and the exit code when the
// Operation was read Stalled.
func (w *appWait) failureError(r revision, s *stall) *clierr.Error {
	if w.mode == modeUpgrade {
		return w.upgradeFailureError(r, s)
	}
	msg := "release " + clierr.ShellQuote(w.t.release) + " failed"
	if desc := w.clean(r.description); desc != "" {
		msg += ": " + desc
	}
	msg += fmt.Sprintf(" (status %s%s); the release stays installed", w.clean(r.status), s.listClause(w))
	return w.committedError(clierr.Text(msg, clierr.Lead("; to remove it, add --apply --yes after reading the preview of "), uninstallPreview(w.t)), w.t.getApp())
}

// upgradeFailureError is the error of an upgrade whose revision is at
// failed: Helm stored it and did not roll back.
func (w *appWait) upgradeFailureError(r revision, s *stall) *clierr.Error {
	msg := "the upgrade of release " + clierr.ShellQuote(w.t.release) + " failed"
	if desc := w.clean(r.description); desc != "" {
		msg += ": " + desc
	}
	msg += fmt.Sprintf(" (revision %d, status %s%s); Helm does not roll back: the resources this upgrade already changed stay changed, "+
		"and in the release's history the last deployed revision still has the status deployed", r.version, w.clean(r.status), s.listClause(w))
	if w.cleanupOnFail {
		msg += "; with --cleanup-on-fail, Helm was asked to delete the resources this upgrade created"
	}
	text := clierr.Text(msg,
		clierr.Lead("; to upgrade again with a change, add the change to this preview command, read the preview, then add --apply --yes: "), upgradePreview(w.t),
		fmt.Sprintf("; the next upgrade sends the values of revision %d, the revision that failed", r.version),
		clierr.Lead("; to remove the release, add --apply --yes after reading the preview of "), uninstallPreview(w.t))
	return w.committedError(text, w.t.getApp())
}

// anotherChangeError is the error of a revision past the recorded one that
// carries another request's id or none.
func (w *appWait) anotherChangeError(version int64) *clierr.Error {
	return w.committedError(fmt.Sprintf("the %s ran; another change to %s also ran (revision %d)",
		w.mode.verb(), clierr.ShellQuote(w.t.release), version), w.t.getApp())
}

// anotherChangeAfterFailureError is anotherChangeError after a Stalled
// Operation.
func (w *appWait) anotherChangeAfterFailureError(s stall, version int64) *clierr.Error {
	return w.committedError(fmt.Sprintf("this request failed%s and another change to %s ran (revision %d)",
		s.clause(w), clierr.ShellQuote(w.t.release), version), w.t.getApp())
}

// leftPendingError is the error of this request's revision still pending
// at the end of the margin.
func (w *appWait) leftPendingError(s *stall) *clierr.Error {
	return w.committedError(clierr.Text(fmt.Sprintf("Helm stopped%s and left release %s pending", s.clause(w), clierr.ShellQuote(w.t.release)),
		clierr.Lead("; to remove it, add --apply --yes after reading the preview of "), uninstallPreview(w.t)), w.t.getApp())
}

// atStatusError is the error of this request's revision at a status other
// than deployed, failed or pending at the end of the margin after a done
// Operation.
func (w *appWait) atStatusError(r revision) *clierr.Error {
	return w.committedError(fmt.Sprintf("release %s is at status %s", clierr.ShellQuote(w.t.release), w.clean(r.status)), w.t.getApp())
}

// deployedButFailedError is the error of a Stalled Operation whose revision
// is deployed: a later step of the operation failed.
func (w *appWait) deployedButFailedError(s stall) *clierr.Error {
	return w.committedError(fmt.Sprintf("operation %s failed%s after release %s was deployed",
		clierr.ShellQuote(w.op.OperationName), s.clause(w), clierr.ShellQuote(w.t.release)), w.t.getApp())
}

// failedAtStatusError is the error of a Stalled Operation whose revision is
// at a status other than deployed, failed or pending at the end of the
// margin.
func (w *appWait) failedAtStatusError(s stall, r revision) *clierr.Error {
	return w.committedError(fmt.Sprintf("operation %s failed%s; release %s is at status %s",
		clierr.ShellQuote(w.op.OperationName), s.clause(w), clierr.ShellQuote(w.t.release), w.clean(r.status)), w.t.getApp())
}

// noRevisionError is the error of a Stalled Operation with no revision of
// the release carrying this request's id at the end of the margin; r is the
// App read last, nil when there is none. The Operation holds the run's
// result, so its command is the remedy.
func (w *appWait) noRevisionError(s stall, r *revision) *clierr.Error {
	msg := fmt.Sprintf("the operation failed%s and no revision of %s carries this request's id; "+
		"Helm records no revision when it fails before it stores the release, for example on values that fail the chart's values schema, "+
		"template errors or resources another release owns",
		s.clause(w), clierr.ShellQuote(w.t.release))
	if r != nil {
		msg += fmt.Sprintf("; release %s is at revision %d, status %s", clierr.ShellQuote(w.t.release), r.version, w.clean(r.status))
	}
	getOperation := clierr.NewCommand("get operation").Arg(w.op.OperationName).
		FlagValue("--cluster", w.t.typedCluster).FlagValue("--namespace", w.op.OperationNamespace)
	return w.committedError(clierr.Text(msg, clierr.Lead("; read the operation with "), getOperation), getOperation)
}

// cutOffError is the error of this request's revision pending on the
// App-only path after the Operation was read and is gone.
func (w *appWait) cutOffError() *clierr.Error {
	return w.committedError(clierr.Text(fmt.Sprintf("Helm's run was cut off; release %s stays pending", clierr.ShellQuote(w.t.release)),
		clierr.Lead("; to remove it, add --apply --yes after reading the preview of "), uninstallPreview(w.t)), w.t.getApp())
}

// uninstalledButFailedError is the error of a Stalled uninstall whose App is
// gone or uninstalled: the release was removed and a later step failed.
func (w *appWait) uninstalledButFailedError(s stall, gone bool) *clierr.Error {
	hint := w.t.getApp()
	if gone {
		hint = clierr.NewCommand("get apps").FlagValue("--cluster", w.t.typedCluster).FlagValue("--namespace", w.t.namespace)
	}
	return w.committedError(fmt.Sprintf("release %s was uninstalled; a later step failed%s", clierr.ShellQuote(w.t.release), s.clause(w)), hint)
}

// staysInstalledError is the error of an uninstall whose App still shows
// the release: the exit code when the Operation was read Stalled, the App's
// status and description.
func (w *appWait) staysInstalledError(s *stall, r revision) *clierr.Error {
	msg := fmt.Sprintf("operation %s ended%s; release %s is at status %s", clierr.ShellQuote(w.op.OperationName), s.clause(w),
		clierr.ShellQuote(w.t.release), w.clean(r.status))
	if desc := w.clean(r.description); desc != "" {
		msg += " (" + desc + ")"
	}
	return w.committedError(msg+"; the release stays installed", w.t.getApp())
}

// keptHistoryError is the error of an uninstall without --keep-history
// whose App still reads uninstalled at the end of the margin.
func (w *appWait) keptHistoryError() *clierr.Error {
	return w.committedError(fmt.Sprintf("release %s is at status uninstalled; its kept history was not removed",
		clierr.ShellQuote(w.t.release)), w.t.getApp())
}

// operationGoneWarning is the warning when the Operation is not found.
func operationGoneWarning(name string) string {
	return fmt.Sprintf("operation %s is gone; the wait reads the app only", clierr.ShellQuote(name))
}

// skipWarning is the warning when the caller cannot read the Operation: the
// wait reads the App alone, and what that cannot see. reviewed is false
// when the access review request itself failed.
func skipWarning(mode waitMode, reviewed bool, ns string) string {
	who := "you cannot get operations in namespace " + clierr.ShellQuote(ns)
	if !reviewed {
		who = "rancher-cli could not check whether you can get operations in namespace " + clierr.ShellQuote(ns)
	}
	return fmt.Sprintf("%s; the wait reads the app only, and %s; a shorter --timeout ends that wait sooner", who, skipFactOf(mode))
}

func (w *appWait) skipFact() string { return skipFactOf(w.mode) }

func skipFactOf(mode waitMode) string {
	switch mode {
	case modeInstall:
		return "a chart Helm refuses before it stores a release is seen only as the wait timeout"
	case modeUpgrade:
		return "an upgrade Helm refuses before it stores a revision is seen only as the wait timeout"
	}
	return "an uninstall that fails and leaves the app unchanged is seen only as the wait timeout"
}

// upgradePreview is the preview command of the upgrade of the release.
func upgradePreview(t actionTarget) clierr.Command {
	return clierr.NewCommand("app upgrade").Arg(t.release).
		FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace).Flag("--dry-run")
}

// uninstallPreview is the preview command of the uninstall of the release.
func uninstallPreview(t actionTarget) clierr.Command {
	return clierr.NewCommand("app uninstall").Arg(t.release).
		FlagValue("--cluster", t.typedCluster).FlagValue("--namespace", t.namespace).Flag("--dry-run")
}

// summary is the last state the wait read: the Operation's conditions with
// their status (and a Stalled message), and the App's status.
func (w *appWait) summary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []string
	switch {
	case w.skip:
	case w.opGone:
		parts = append(parts, "operation "+clierr.ShellQuote(w.op.OperationName)+" not found")
	case w.lastOp != nil:
		parts = append(parts, "operation "+clierr.ShellQuote(w.op.OperationName)+" "+w.conditions())
	}
	switch {
	case w.appRead && w.appAbsent:
		parts = append(parts, "app not found")
	case w.appRead:
		parts = append(parts, "app status "+w.clean(statusText(w.appStatus)))
	}
	if len(parts) == 0 {
		return "nothing read"
	}
	return strings.Join(parts, "; ")
}

// conditions lists the last Operation's condition types with their status;
// a Stalled=True condition adds its message. No other condition message is
// shown: a Reconciling message can hold Helm's output. An Operation whose
// status.observedGeneration is behind metadata.generation says so first:
// its conditions are not about this run yet.
func (w *appWait) conditions() string {
	conds, _, _ := unstructured.NestedSlice(w.lastOp.Object, "status", "conditions")
	var out []string
	if observed, _, _ := unstructured.NestedInt64(w.lastOp.Object, "status", "observedGeneration"); observed < w.lastOp.GetGeneration() {
		out = append(out, fmt.Sprintf("observedGeneration %d behind generation %d", observed, w.lastOp.GetGeneration()))
	}
	for _, c := range conds {
		m, _ := c.(map[string]any)
		typ, _ := m["type"].(string)
		status, _ := m["status"].(string)
		entry := w.clean(typ) + "=" + w.clean(status)
		if msg, _ := m["message"].(string); typ == "Stalled" && status == "True" && w.clean(msg) != "" {
			entry += " (" + w.clean(msg) + ")"
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return "with no conditions"
	}
	return strings.Join(out, ", ")
}

// exitCode reads Helm's exit code from a Stalled message: the trimmed
// message is "exit code: " and digits only. Any other message, a sign or a
// number too large included, gives false.
func exitCode(message string) (int, bool) {
	digits, ok := strings.CutPrefix(strings.TrimSpace(message), "exit code: ")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// clause is the Stalled message as a parenthesized clause with a leading
// space: " (exit code <n>)", " (operation message: <message>)" for a message
// in another shape, or "" with no stall or an empty message.
func (s *stall) clause(w *appWait) string {
	if c := s.text(w); c != "" {
		return " (" + c + ")"
	}
	return ""
}

// listClause is the Stalled message as an entry of a parenthesized list:
// ", exit code <n>", ", operation message: <message>" or "".
func (s *stall) listClause(w *appWait) string {
	if c := s.text(w); c != "" {
		return ", " + c
	}
	return ""
}

func (s *stall) text(w *appWait) string {
	if s == nil {
		return ""
	}
	if n, ok := exitCode(s.message); ok {
		return "exit code " + strconv.Itoa(n)
	}
	if msg := w.clean(s.message); msg != "" {
		return "operation message: " + msg
	}
	return ""
}

// waitDuration writes a --timeout value: whole minutes as "10m", anything
// else as Go writes it.
func waitDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.String()
}
