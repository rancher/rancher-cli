package testrancher

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync/atomic"
)

// endedRequestWriter wraps the response writer of a request so that the
// Facade never lets a client read an answer to a request whose context has
// ended. The context of a server request ends when the client closes its
// connection, and a client that closed it because its own context ended must
// not read an answer: on TLS the server can wake, write the answer and flush
// it before the client's socket is gone, and a client whose cancellation
// lands in the same instant can read that answer instead of its own error.
//
// The Facade still runs its handler and makes its state change (a token
// created, a revoke recorded), as a real server would: it cannot undo work it
// has done. Once the context has ended the wrapper drops every write and
// every flush that would start or continue an answer; a flush that only
// sends what was already written still goes through. ServeHTTP then
// aborts the connection when the context ended and a write was dropped, or
// when nothing had been answered: an answer cut short is an error to the
// client, never a status with a missing body.
type endedRequestWriter struct {
	http.ResponseWriter
	ctx context.Context
	// answered is set when a header, a body byte or a flush went to the
	// client while the context was alive, or when the connection was
	// hijacked. Every flag here is atomic: the proxy that serves the
	// fall-through flushes from its own goroutine.
	answered atomic.Bool
	// dropped is set when a write or a header was dropped because the
	// context had ended.
	dropped atomic.Bool
	// hijacked is set once the handler owns the connection. Writes then go
	// to the wrapped writer, which reports the hijack, and count as nothing.
	hijacked atomic.Bool
}

func (w *endedRequestWriter) WriteHeader(code int) {
	if !w.hijacked.Load() {
		if w.ctx.Err() != nil {
			w.dropped.Store(true)
			return
		}
		w.answered.Store(true)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *endedRequestWriter) Write(p []byte) (int, error) {
	if !w.hijacked.Load() {
		if w.ctx.Err() != nil {
			w.dropped.Store(true)
			return len(p), nil
		}
		w.answered.Store(true)
	}
	return w.ResponseWriter.Write(p)
}

// Flush forwards to the wrapped writer while the context is alive, and after
// the context ended when an answer was sent and nothing was dropped. Once a
// write was dropped it does nothing: a flush would send a header with a
// missing body. Flushing an unanswered writer would send an implicit 200.
func (w *endedRequestWriter) Flush() {
	if !w.hijacked.Load() {
		if w.dropped.Load() || (w.ctx.Err() != nil && !w.answered.Load()) {
			return
		}
		w.answered.Store(true)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack hands the connection over, for the Drop options and for the proxy
// that serves the fall-through. A hijacked connection counts as answered:
// the handler owns it from then on.
func (w *endedRequestWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked.Store(true)
		w.answered.Store(true)
	}
	return conn, rw, err
}

// Unwrap lets http.ResponseController reach the other optional interfaces of
// the wrapped writer.
func (w *endedRequestWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// abortEndedRequest aborts the connection when the request's context has
// ended and either a write was dropped or nothing was sent to the client. net/http
// closes the connection without writing the rest of a response and without
// logging for this panic value. A hijacked connection is left to its owner.
func (w *endedRequestWriter) abortEndedRequest() {
	if w.ctx.Err() != nil && !w.hijacked.Load() && (w.dropped.Load() || !w.answered.Load()) {
		panic(http.ErrAbortHandler)
	}
}
