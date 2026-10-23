package testtransport

import "net/http"

// HangAndAbort is a handler body for a server that never answers. It waits
// until the context of r ends, then aborts the connection with no response.
//
// A test server that stands in for a stalled or unreachable server holds a
// request open until the request's own context ends, which happens when the
// client gives up and closes its connection. A handler that then returns
// makes net/http write an implicit 200 response, and a client whose own
// cancellation lands in the same instant can read that response instead of
// its cancellation error. On TLS the window is wide: the client sends
// close_notify before it closes the socket; the server can wake up, return
// and flush the 200 while the socket is still open.
// panic(http.ErrAbortHandler) is the one way out of the handler that
// sends nothing: net/http closes the connection without a response and
// without logging. Every wait on a request's context goes through the
// helpers in this file; a source scan in test/conformance fails any other
// call of Done on a context that comes from a request.
func HangAndAbort(r *http.Request) {
	<-r.Context().Done()
	panic(http.ErrAbortHandler)
}

// HangUntilReleased is a handler body for a server that answers once a test
// releases it. It returns, and the handler then answers as it would for any
// request, when release is closed or receives a value. If the context of r
// ends first it aborts the connection with no response, as HangAndAbort does.
func HangUntilReleased(r *http.Request, release <-chan struct{}) {
	select {
	case <-release:
	case <-r.Context().Done():
		panic(http.ErrAbortHandler)
	}
}

// WaitUntilGone is for a hook that a test server calls before it answers,
// where the server carries on after the hook returns and aborts its answer
// itself when the context of r has ended. It returns once the context of r
// ends. A handler that is not behind such a server must use HangAndAbort.
func WaitUntilGone(r *http.Request) {
	<-r.Context().Done()
}
