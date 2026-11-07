package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
)

// defaultStreamGrace is how long a stream may outlive the timeoutSeconds the
// server was asked for before the client ends it.
const defaultStreamGrace = 10 * time.Second

// streamGraceNanos is the grace in nanoseconds, 0 for the default. Atomic, as
// the stream length range of the engine is, so a test that shrinks it does
// not race with a call on another goroutine.
var streamGraceNanos atomic.Int64

func streamGrace() time.Duration {
	if n := streamGraceNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return defaultStreamGrace
}

// maxWatchSeconds is the longest timeoutSeconds a watch asks for.
const maxWatchSeconds = 60

// The client lists and watches one named object, so a wait sees a change
// without polling. A build fails when the client loses either method.
var _ engine.ObjectWatcher = (*dynamicClient)(nil)

// nameSelector is the field selector that names one object.
func nameSelector(name string) string {
	return fields.OneTermEqualSelector("metadata.name", name).String()
}

// ListObject lists the objects of the kind named name with a field selector
// on metadata.name and returns the item, nil for an empty list, and the
// list's resourceVersion.
func (r *dynamicClient) ListObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, string, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	out, err := r.ri(gvr, ns).List(ctx, metav1.ListOptions{FieldSelector: nameSelector(name)})
	if err != nil {
		return nil, "", mapRequestErr(ctx, http.MethodGet, err, r.request("list", gvr, ns, name))
	}
	rv := out.GetResourceVersion()
	if rv == "" {
		// A watch with no resourceVersion starts at the most recent state, not
		// after the list, so a stream must never start from this answer; the
		// wait polls.
		return nil, "", fmt.Errorf("%w: the server listed %s with no resourceVersion", engine.ErrListUnusable, name)
	}
	switch len(out.Items) {
	case 0:
		return nil, rv, nil
	case 1:
		return &out.Items[0], rv, nil
	default:
		return nil, "", fmt.Errorf("%w: the server listed %d objects for the name %s", engine.ErrListUnusable, len(out.Items), name)
	}
}

// WatchObject watches the object named name from resourceVersion.
//
// It is the one call of the client that does not run under opCtx, which ends
// a request after requestTimeout: a watch lasts as long as the server keeps
// the stream open. The open, until the response headers, is bounded by
// requestTimeout and has an answers record of its own, so it has the same 429
// budget as a Get. The stream is bounded by timeout plus streamGrace.
func (r *dynamicClient) WatchObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string,
	timeout time.Duration) (<-chan engine.ObjectEvent, error) {
	gvr, err := r.resolveVersion(kube.WithAnswers(ctx), gvr)
	if err != nil {
		return nil, err
	}
	seconds := min(max(int64(math.Ceil(timeout.Seconds())), 1), maxWatchSeconds)
	streamCtx, cancelStream := context.WithTimeout(ctx, time.Duration(seconds)*time.Second+streamGrace())
	reqCtx, cancelReq := context.WithCancel(kube.WithNewAnswers(streamCtx))

	// The request context is the stream's, so the open cannot run under a
	// deadline. A timer cancels it when the response headers have not come.
	timer := time.AfterFunc(requestTimeout, cancelReq)
	w, err := r.ri(gvr, ns).Watch(reqCtx, metav1.ListOptions{
		FieldSelector:       nameSelector(name),
		ResourceVersion:     resourceVersion,
		AllowWatchBookmarks: true,
		TimeoutSeconds:      &seconds,
	})
	if !timer.Stop() {
		// The timer fired as Watch returned. A stream is never handed out with
		// a cancelled context, so a success is replaced by the deadline error.
		// Only an authentication or certificate failure, which ends a wait,
		// keeps its own error; any other answer is read as a Get reads one
		// that the deadline cut.
		switch {
		case w != nil:
			w.Stop()
			err = context.DeadlineExceeded
		case isCancelWithNoAnswer(err):
			err = context.DeadlineExceeded
			if status, ok := kube.LastAnsweredStatus(reqCtx); ok && status >= http.StatusBadRequest {
				if answered := clierr.ResponseError(http.MethodGet, status, nil); engine.WaitShouldStop(answered) {
					err = answered
				}
			}
		}
	}
	if err != nil {
		mapped := mapRequestErr(reqCtx, http.MethodGet, err, r.request("watch", gvr, ns, name))
		cancelReq()
		cancelStream()
		return nil, mapped
	}

	out := make(chan engine.ObjectEvent)
	go func() {
		defer close(out)
		defer cancelStream()
		defer cancelReq()
		defer w.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case ev, ok := <-w.ResultChan():
				// A cancelled stream makes the read fail, and the watcher reports
				// that failure as an ERROR event: an event that comes after the
				// context ended is the end of the stream, not news from the server.
				if !ok || streamCtx.Err() != nil {
					return
				}
				mapped := r.mapEvent(ev, gvr, ns, name)
				select {
				case out <- mapped:
				case <-streamCtx.Done():
					return
				}
				if mapped.Type == engine.ObjectFailed {
					return
				}
			}
		}
	}()
	return out, nil
}

// isCancelWithNoAnswer reports whether err is the cancellation of a request
// that got no answer, which is what the open timer causes.
func isCancelWithNoAnswer(err error) bool {
	if err == nil || engine.WaitShouldStop(err) {
		return false
	}
	if _, answered := clierr.HTTPStatus(err); answered {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// mapEvent turns a watch event into an engine.ObjectEvent.
func (r *dynamicClient) mapEvent(ev watch.Event, gvr schema.GroupVersionResource, ns, name string) engine.ObjectEvent {
	switch ev.Type {
	case watch.Added, watch.Modified:
		if obj, ok := ev.Object.(*unstructured.Unstructured); ok {
			return engine.ObjectEvent{Type: engine.ObjectChanged, Object: obj}
		}
	case watch.Deleted:
		if obj, ok := ev.Object.(*unstructured.Unstructured); ok {
			return engine.ObjectEvent{Type: engine.ObjectDeleted, Object: obj}
		}
	case watch.Bookmark:
		return engine.ObjectEvent{Type: engine.ObjectBookmark}
	case watch.Error:
		if st := statusOf(ev.Object); st != nil {
			err := clierr.FromStatus(apierrors.FromObject(st), r.request("watch", gvr, ns, name))
			return engine.ObjectEvent{Type: engine.ObjectFailed, Err: err}
		}
	}
	return engine.ObjectEvent{Type: engine.ObjectFailed,
		Err: clierr.New(clierr.CodeGeneric, fmt.Sprintf("the watch of %s sent an event the client cannot read (%s)", name, ev.Type))}
}

// statusOf reads the Status an ERROR event carries, which arrives as a
// *metav1.Status or, from the dynamic client, as an unstructured object.
func statusOf(obj runtime.Object) *metav1.Status {
	switch o := obj.(type) {
	case *metav1.Status:
		return o
	case *unstructured.Unstructured:
		var st metav1.Status
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &st); err != nil {
			return nil
		}
		return &st
	}
	return nil
}
