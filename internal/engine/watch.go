package engine

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ErrWatchUnavailable is what a client that offers ObjectWatcher returns from
// ListObject or WatchObject when it cannot serve them at run time. It means
// no request was sent. A wait treats it like an HTTP 405 and polls.
var ErrWatchUnavailable = errors.New("watching an object is not available")

// ErrListUnusable is what ListObject returns, wrapped, for a list a watch
// cannot start from: one with no resourceVersion, or with more than one item
// (a name selector cannot match two objects). The List was sent. A wait polls
// instead, as for a refused List.
var ErrListUnusable = errors.New("the list cannot start a watch")

// ObjectWatcher is implemented by a ClusterClient that can list and watch one
// named object. A wait type-asserts it, as a command does TableReader; a
// client without it is polled with Get.
type ObjectWatcher interface {
	// ListObject lists the objects of the kind whose metadata.name is name, in
	// ns ("" for a cluster-scoped kind). It returns the item, nil when the list
	// is empty, and the list's resourceVersion. A list with no resourceVersion
	// or with more than one item is an error wrapping ErrListUnusable.
	ListObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (
		obj *unstructured.Unstructured, listResourceVersion string, err error)
	// WatchObject watches name from resourceVersion. The server ends the stream
	// after about timeout, which is a whole number of seconds from 1 to 60. The
	// stream also ends when ctx ends or on any read error, and the channel is
	// then closed. An error is returned only when the stream could not be
	// opened.
	WatchObject(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string,
		timeout time.Duration) (<-chan ObjectEvent, error)
}

// ObjectEventType is what an ObjectEvent reports.
type ObjectEventType int

const (
	// ObjectChanged is an object that was added or modified.
	ObjectChanged ObjectEventType = iota
	// ObjectDeleted is an object that was deleted.
	ObjectDeleted
	// ObjectBookmark is a progress marker the server sends; it carries no
	// change.
	ObjectBookmark
	// ObjectFailed is an error the server sent in the stream.
	ObjectFailed
)

// ObjectEvent is one event of a watch.
type ObjectEvent struct {
	Type ObjectEventType
	// Object is set for ObjectChanged and ObjectDeleted.
	Object *unstructured.Unstructured
	// Err is set for ObjectFailed: the error event's Status, classified.
	Err error
}
