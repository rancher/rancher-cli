package catalog

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// defaultPollInterval is the spacing of the wait's reads.
const defaultPollInterval = 5 * time.Second

// appMargin is how long the wait reads the App after the Operation ends
// without the App showing the end of Helm's run. It is a margin for the app
// controller, which writes the App apart from the Operation, not a measured
// bound.
const appMargin = 30 * time.Second

// waitMode is the kind of request a wait follows.
type waitMode int

const (
	modeInstall waitMode = iota
	modeUninstall
	modeUpgrade
)

// writesRevision reports whether the request stores a new revision of the
// release: an install and an upgrade do, an uninstall does not.
func (m waitMode) writesRevision() bool { return m != modeUninstall }

// verb is the request's name in the texts of a wait.
func (m waitMode) verb() string {
	switch m {
	case modeUpgrade:
		return "upgrade"
	case modeUninstall:
		return "uninstall"
	}
	return "install"
}

// appWait is the wait of one accepted install, upgrade or uninstall request:
// the Operation Rancher created for it, then the App. Every read goes
// through engine.WaitObject, with one deadline for the whole wait.
type appWait struct {
	client   engine.ClusterClient
	interval time.Duration
	timeout  time.Duration
	deadline time.Time

	t  actionTarget
	op actionAnswer
	// filter hides the values the request sent in server text.
	filter func(string) string
	mode   waitMode
	// recorded is the App's spec.version Plan read before an upgrade, 0 for
	// an install, which found no App: a revision past it is a newer one.
	recorded int64
	// cleanupOnFail is the upgrade's --cleanup-on-fail.
	cleanupOnFail bool
	// keepHistory and planStatus are the uninstall's --keep-history and the
	// App's spec.info.status Plan read.
	keepHistory bool
	planStatus  string
	// skip is true when the caller cannot read the Operation.
	skip bool
	// stepsForTest, when set, replaces the steps run takes, so a test can
	// send any error through run.
	stepsForTest func(ctx context.Context) (waitEnd, error)

	// What the wait read, for the timeout's summary. mu guards the App's
	// fields, which a watch's events update apart from the wait's loop.
	lastOp    *unstructured.Unstructured
	opGone    bool
	mu        sync.Mutex
	appRead   bool
	appAbsent bool
	appStatus string
	warnings  []string
}

// waitEnd is how a wait that succeeded ended. gone is true for an
// uninstall whose App is gone.
type waitEnd struct {
	status   string
	revision int64
	gone     bool
	warnings []string
}

// stall is what the wait read of a Stalled Operation: its message.
type stall struct{ message string }

// revision is what the wait reads of one App.
type revision struct {
	// mine is true when the App's chart carries this request's id.
	mine        bool
	status      string
	version     int64
	description string
}

func (w *appWait) revisionOf(obj *unstructured.Unstructured) revision {
	ann, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "chart", "metadata", "annotations")
	status, _, _ := unstructured.NestedString(obj.Object, "spec", "info", "status")
	desc, _, _ := unstructured.NestedString(obj.Object, "spec", "info", "description")
	version, _, _ := unstructured.NestedInt64(obj.Object, "spec", "version")
	return revision{mine: w.t.requestID != "" && ann[annotationRequestID] == w.t.requestID, status: status, version: version, description: desc}
}

// pastRecorded reports whether an App's spec.version is past the revision
// Plan recorded. Plan of an install found no App, so it recorded none: any
// revision is past it.
func (w *appWait) pastRecorded(version int64) bool { return version > w.recorded }

func pending(status string) bool { return strings.HasPrefix(status, "pending-") }

// clean is server text as a message carries it: the values hidden, the
// whitespace runs made one space, and every character output.Printable
// removes removed.
func (w *appWait) clean(s string) string {
	return output.Printable(strings.Join(strings.Fields(w.filter(s)), " "))
}

// left is the time the wait has left.
func (w *appWait) left() time.Duration { return time.Until(w.deadline) }

// run waits for the end of Helm's run. Every error it returns carries the
// committed details and is not retryable.
func (w *appWait) run(ctx context.Context) (waitEnd, error) {
	steps := w.steps
	if w.stepsForTest != nil {
		steps = w.stepsForTest
	}
	end, err := steps(ctx)
	if err != nil {
		return waitEnd{}, w.passThroughError(err)
	}
	return end, nil
}

func (w *appWait) steps(ctx context.Context) (waitEnd, error) {
	w.deadline = time.Now().Add(w.timeout)
	if w.skip {
		return w.appOnly(ctx, false)
	}
	s, seen, err := w.operationStep(ctx)
	switch {
	case err != nil && notFound(err):
		w.opGone = true
		w.warnings = append(w.warnings, operationGoneWarning(w.op.OperationName))
		return w.appOnly(ctx, seen)
	case err != nil:
		return waitEnd{}, err
	case w.mode.writesRevision() && s != nil:
		return waitEnd{}, w.installStalled(ctx, *s)
	case w.mode.writesRevision():
		return w.installDone(ctx)
	case s != nil:
		return waitEnd{}, w.uninstallStalled(ctx, *s)
	}
	return w.uninstallDone(ctx)
}

// operationStep reads the Operation until Helm's run has ended: done is
// status.observedGeneration current, Reconciling=False and Stalled not
// True; a Stalled=True at the current generation is returned. seen says
// whether an Operation was read, for a NOT_FOUND that ends the step.
func (w *appWait) operationStep(ctx context.Context) (s *stall, seen bool, err error) {
	err = w.read(ctx, readStep{
		gvr: kind("operation").GVR, ns: w.op.OperationNamespace, name: w.op.OperationName, limit: w.left(),
		observe: func(obj *unstructured.Unstructured) (bool, error) {
			seen = true
			w.lastOp = obj
			observed, _, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
			if observed < obj.GetGeneration() {
				return false, nil
			}
			if status, msg, ok := condition(obj, "Stalled"); ok && status == "True" {
				s = &stall{message: msg}
				return true, nil
			}
			if status, _, ok := condition(obj, "Reconciling"); ok && status == "False" {
				return true, nil
			}
			return false, nil
		},
		end: func(_ *unstructured.Unstructured, lastErr *clierr.Error, _ bool) error {
			return w.timeoutError(lastErr)
		},
	})
	return s, seen, err
}

// installDone reads the App after an install's or upgrade's Operation that
// ended without Stalled: done on the revision with this request's id at deployed. A
// status of that revision other than deployed or failed starts a margin
// read; an App that is absent at its end is read again.
func (w *appWait) installDone(ctx context.Context) (waitEnd, error) {
	for {
		var end *waitEnd
		margin := false
		err := w.read(ctx, readStep{
			gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: w.left(), whileAbsent: true,
			observe: func(obj *unstructured.Unstructured) (bool, error) {
				r := w.revisionOf(obj)
				done, err := w.installRule(r, &end)
				if done || err != nil {
					return done, err
				}
				margin = r.mine
				return margin, nil
			},
			end: func(_ *unstructured.Unstructured, lastErr *clierr.Error, _ bool) error {
				return w.timeoutError(lastErr)
			},
		})
		if err != nil || end != nil {
			return derefEnd(end), err
		}
		if !margin {
			return waitEnd{}, w.timeoutError(nil)
		}
		again := false
		err = w.read(ctx, readStep{
			gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: min(appMargin, w.left()), whileAbsent: true,
			observe: func(obj *unstructured.Unstructured) (bool, error) {
				return w.installRule(w.revisionOf(obj), &end)
			},
			end: func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error {
				if !answered {
					return w.timeoutError(lastErr)
				}
				if last == nil {
					again = true
					return nil
				}
				r := w.revisionOf(last)
				switch {
				case r.mine && pending(r.status):
					return w.leftPendingError(nil)
				case r.mine:
					return w.atStatusError(r)
				}
				again = true
				return nil
			},
		})
		if err != nil || end != nil {
			return derefEnd(end), err
		}
		if !again {
			return waitEnd{}, w.timeoutError(nil)
		}
	}
}

// installRule is the rule an install's or upgrade's App read after a done Operation and
// on the App-only path share: done on this revision at deployed, the
// failure error on this revision at failed, the another-change error on a
// revision past the recorded one without this id.
func (w *appWait) installRule(r revision, end **waitEnd) (bool, error) {
	switch {
	case r.mine && r.status == "deployed":
		*end = &waitEnd{status: r.status, revision: r.version, warnings: w.warnings}
		return true, nil
	case r.mine && r.status == "failed":
		return false, w.failureError(r, nil)
	case !r.mine && w.pastRecorded(r.version):
		return false, w.anotherChangeError(r.version)
	}
	return false, nil
}

func derefEnd(end *waitEnd) waitEnd {
	if end == nil {
		return waitEnd{}
	}
	return *end
}

// installStalled reads the App after a Stalled install or upgrade Operation
// for the margin, or
// the time left when that is less, and returns the error that names how
// the run ended. Every path ends in an error: Helm exited non-zero.
func (w *appWait) installStalled(ctx context.Context, s stall) error {
	return w.read(ctx, readStep{
		gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: min(appMargin, w.left()), whileAbsent: true,
		observe: func(obj *unstructured.Unstructured) (bool, error) {
			r := w.revisionOf(obj)
			switch {
			case r.mine && r.status == "failed":
				return false, w.failureError(r, &s)
			case r.mine && r.status == "deployed":
				return false, w.deployedButFailedError(s)
			case !r.mine && w.pastRecorded(r.version):
				return false, w.anotherChangeAfterFailureError(s, r.version)
			}
			return false, nil
		},
		end: func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error {
			if !answered {
				return w.timeoutError(lastErr)
			}
			if last == nil {
				return w.noRevisionError(s, nil)
			}
			r := w.revisionOf(last)
			switch {
			case r.mine && pending(r.status):
				return w.leftPendingError(&s)
			case r.mine:
				return w.failedAtStatusError(s, r)
			}
			return w.noRevisionError(s, &r)
		},
	})
}

// uninstallDone reads the App after an Operation that ended without
// Stalled, for the margin: done when it is gone, or with --keep-history when
// it reads uninstalled.
func (w *appWait) uninstallDone(ctx context.Context) (waitEnd, error) {
	var end *waitEnd
	err := w.read(ctx, readStep{
		gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: min(appMargin, w.left()),
		observe: func(obj *unstructured.Unstructured) (bool, error) {
			if w.uninstallFinished(w.revisionOf(obj)) {
				end = &waitEnd{status: "uninstalled", warnings: w.warnings}
				return true, nil
			}
			return false, nil
		},
		onDeleted: func() (bool, error) {
			end = w.goneEnd()
			return true, nil
		},
		end: func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error {
			switch {
			case !answered:
				return w.timeoutError(lastErr)
			case last == nil:
				end = w.goneEnd()
				return nil
			}
			r := w.revisionOf(last)
			if r.status == "uninstalled" && w.planStatus != "uninstalled" {
				return w.keptHistoryError()
			}
			return w.staysInstalledError(nil, r)
		},
	})
	if err != nil && notFound(err) {
		return *w.goneEnd(), nil
	}
	return derefEnd(end), err
}

// goneEnd is the end of an uninstall whose App is gone.
func (w *appWait) goneEnd() *waitEnd {
	return &waitEnd{status: "uninstalled", gone: true, warnings: w.warnings}
}

// uninstallFinished is the end of an uninstall on an App that still exists:
// with --keep-history, the status uninstalled. Plan refuses --keep-history
// on an App it read uninstalled, so that status is never the one before the
// request here.
func (w *appWait) uninstallFinished(r revision) bool {
	return w.keepHistory && r.status == "uninstalled"
}

// uninstallStalled reads the App after a Stalled Operation for the margin,
// or the time left when that is less. Every path ends in an error. Helm
// stores uninstalling for every uninstall and removes the release after a
// failed later step too, so uninstalling keeps the read going.
func (w *appWait) uninstallStalled(ctx context.Context, s stall) error {
	err := w.read(ctx, readStep{
		gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: min(appMargin, w.left()),
		observe: func(obj *unstructured.Unstructured) (bool, error) {
			if r := w.revisionOf(obj); r.status == "uninstalled" && w.planStatus != "uninstalled" {
				return false, w.uninstalledButFailedError(s, false)
			}
			return false, nil
		},
		onDeleted: func() (bool, error) { return false, w.uninstalledButFailedError(s, true) },
		end: func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error {
			switch {
			case !answered:
				return w.timeoutError(lastErr)
			case last == nil:
				return w.uninstalledButFailedError(s, true)
			}
			return w.staysInstalledError(&s, w.revisionOf(last))
		},
	})
	if err != nil && notFound(err) {
		return w.uninstalledButFailedError(s, true)
	}
	return err
}

// appOnly reads the App alone until the deadline: the Operation step was
// skipped, or the Operation is gone (gone is true when it was read before).
func (w *appWait) appOnly(ctx context.Context, gone bool) (waitEnd, error) {
	var end *waitEnd
	var onDeleted func() (bool, error)
	if !w.mode.writesRevision() {
		onDeleted = func() (bool, error) {
			end = w.goneEnd()
			return true, nil
		}
	}
	err := w.read(ctx, readStep{
		gvr: kind("app").GVR, ns: w.t.namespace, name: w.t.release, limit: w.left(), whileAbsent: w.mode.writesRevision(),
		onDeleted: onDeleted,
		observe: func(obj *unstructured.Unstructured) (bool, error) {
			r := w.revisionOf(obj)
			if !w.mode.writesRevision() {
				if w.uninstallFinished(r) {
					end = &waitEnd{status: "uninstalled", warnings: w.warnings}
					return true, nil
				}
				return false, nil
			}
			if gone && r.mine && pending(r.status) {
				return false, w.cutOffError()
			}
			return w.installRule(r, &end)
		},
		end: func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error {
			if !w.mode.writesRevision() && answered && last == nil {
				end = w.goneEnd()
				return nil
			}
			return w.timeoutError(lastErr)
		},
	})
	if err != nil && !w.mode.writesRevision() && notFound(err) {
		return *w.goneEnd(), nil
	}
	return derefEnd(end), err
}

// readStep is one engine.WaitObject call of the wait: an object, at most
// limit, and the rules of the step. observe judges each object; end is the
// error at the end of limit, told the last object (nil when the last answer
// was NOT_FOUND or a Deleted event), the last error, and whether any read
// was answered (an object, NOT_FOUND or a Deleted event). whileAbsent keeps
// the step going on NOT_FOUND. onDeleted, for a step that ends when the
// object is removed, decides on a watch Deleted event at once.
type readStep struct {
	gvr         schema.GroupVersionResource
	ns, name    string
	limit       time.Duration
	whileAbsent bool
	observe     func(obj *unstructured.Unstructured) (done bool, err error)
	onDeleted   func() (done bool, err error)
	end         func(last *unstructured.Unstructured, lastErr *clierr.Error, answered bool) error
}

func (w *appWait) read(ctx context.Context, s readStep) error {
	answers, absent := 0, false
	isApp := s.gvr == kind("app").GVR
	seen := func(obj *unstructured.Unstructured) {
		w.mu.Lock()
		defer w.mu.Unlock()
		answers++
		absent = obj == nil
		if isApp {
			w.appRead, w.appAbsent = true, absent
			if obj != nil {
				w.appStatus = w.revisionOf(obj).status
			}
		}
	}
	var onDeleted func(context.Context) (engine.WaitOutcome, error)
	if s.onDeleted != nil {
		onDeleted = func(context.Context) (engine.WaitOutcome, error) {
			done, err := s.onDeleted()
			if done {
				return engine.WaitDone, err
			}
			return engine.WaitContinue, err
		}
	}
	return engine.WaitObject(ctx, answering(w.client, seen), engine.WaitParams{
		GVR: s.gvr, Namespace: s.ns, Name: s.name,
		Timeout: s.limit, Interval: w.interval, SpacingFrom: engine.SpacingFromReturn,
		WhileAbsent: s.whileAbsent,
		OnDeleted:   onDeleted,
		Observe: func(_ context.Context, obj *unstructured.Unstructured, _ engine.WaitSource) (engine.WaitOutcome, error) {
			done, err := s.observe(obj)
			switch {
			case err != nil:
				return engine.WaitContinue, err
			case done:
				return engine.WaitDone, nil
			}
			return engine.WaitContinue, nil
		},
		// A request cut by the wait's own deadline or by a cancel, with no
		// answer, says nothing about the server.
		RecordErr: func(ctx context.Context, err error) bool {
			_, answered := clierr.HTTPStatus(err)
			return ctx.Err() == nil || answered
		},
		OnAbort: func(err error) error { return w.abortedError(err) },
		OnCancel: func(context.Context, *unstructured.Unstructured, *clierr.Error) error {
			return w.cancelledError()
		},
		OnTimeout: func(last *unstructured.Unstructured, lastErr *clierr.Error) error {
			w.mu.Lock()
			answered, gone := answers > 0, absent
			w.mu.Unlock()
			if gone {
				last = nil
			}
			return s.end(last, lastErr, answered)
		},
	})
}

// condition returns the status and message of obj's status condition typ.
func condition(obj *unstructured.Unstructured, typ string) (status, message string, ok bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == typ {
			status, _ = m["status"].(string)
			message, _ = m["message"].(string)
			return status, message, true
		}
	}
	return "", "", false
}

// answering is client with every answer reported to seen: an object of a
// Get, a List or a Changed event, or nil for NOT_FOUND and a Deleted event.
// A client that watches still watches.
func answering(client engine.ClusterClient, seen func(*unstructured.Unstructured)) engine.ClusterClient {
	a := &answeringClient{ClusterClient: client, seen: seen}
	if ow, ok := client.(engine.ObjectWatcher); ok {
		return &answeringWatcher{answeringClient: a, watcher: ow}
	}
	return a
}

type answeringClient struct {
	engine.ClusterClient
	seen func(*unstructured.Unstructured)
}

func (a *answeringClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	obj, err := a.ClusterClient.Get(ctx, gvr, ns, name)
	switch {
	case err == nil:
		a.seen(obj)
	case notFound(err):
		a.seen(nil)
	}
	return obj, err
}

type answeringWatcher struct {
	*answeringClient
	watcher engine.ObjectWatcher
}

func (a *answeringWatcher) ListObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, string, error) {
	obj, rv, err := a.watcher.ListObject(ctx, gvr, ns, name)
	if err == nil && obj != nil {
		a.seen(obj)
	}
	return obj, rv, err
}

// WatchObject passes the stream's events on after it reports each one, so the
// wait reads an event only after seen has it. The copy ends with the stream
// or with ctx.
func (a *answeringWatcher) WatchObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, timeout time.Duration) (<-chan engine.ObjectEvent, error) {
	events, err := a.watcher.WatchObject(ctx, gvr, ns, name, resourceVersion, timeout)
	if err != nil {
		return nil, err
	}
	out := make(chan engine.ObjectEvent)
	go func() {
		defer close(out)
		for ev := range events {
			// An event the stream hands over after its context ended belongs
			// to a read that has returned.
			if ctx.Err() != nil {
				return
			}
			switch ev.Type {
			case engine.ObjectChanged:
				a.seen(ev.Object)
			case engine.ObjectDeleted:
				a.seen(nil)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
