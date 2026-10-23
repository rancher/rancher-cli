package testrancher_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const loginBody = `{"type":"localProvider","username":"e2e-user","password":"e2e-pass","responseType":"json"}`

// writeSpy records the calls that would put an answer on the connection.
// afterWriteHeader, when set, runs after each WriteHeader call is recorded
// and forwarded; a test uses it to end the request's context in the middle of
// an answer.
type writeSpy struct {
	http.ResponseWriter
	calls            []string
	afterWriteHeader func()
}

func (s *writeSpy) WriteHeader(code int) {
	s.calls = append(s.calls, "WriteHeader")
	s.ResponseWriter.WriteHeader(code)
	if s.afterWriteHeader != nil {
		s.afterWriteHeader()
	}
}

func (s *writeSpy) Write(p []byte) (int, error) {
	s.calls = append(s.calls, "Write")
	return s.ResponseWriter.Write(p)
}

func (s *writeSpy) Flush() {
	s.calls = append(s.calls, "Flush")
	s.ResponseWriter.(http.Flusher).Flush()
}

// controlSpy records the optional interfaces of a response writer that the
// wrapper must forward: Flush, Hijack and SetWriteDeadline.
type controlSpy struct {
	http.ResponseWriter
	calls []string
	conn  net.Conn
}

func (s *controlSpy) Flush() { s.calls = append(s.calls, "Flush") }

// Write reports the hijack, as a response writer does once its connection
// was taken.
func (s *controlSpy) Write([]byte) (int, error) {
	s.calls = append(s.calls, "Write")
	return 0, http.ErrHijacked
}

func (s *controlSpy) SetWriteDeadline(time.Time) error {
	s.calls = append(s.calls, "SetWriteDeadline")
	return nil
}

func (s *controlSpy) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.calls = append(s.calls, "Hijack")
	return s.conn, nil, nil
}

// facadeWithHandler returns a Facade whose fall-through is handler, and a
// request that reaches it.
func facadeWithHandler(handler http.HandlerFunc) *testrancher.Facade {
	return testrancher.New(testrancher.Options{Fallthrough: handler})
}

func fallthroughRequest(ctx context.Context) *http.Request {
	return httptest.NewRequestWithContext(ctx, http.MethodGet, "/apis/spy", nil)
}

// TestFacadeAbortsItsAnswerToARequestWhoseContextEnded asserts that a request
// whose context ended before the Facade would answer still changes the
// Facade's state but gets no response: the connection is aborted, and the
// handler's write and implicit 200 never reach the client.
func TestFacadeAbortsItsAnswerToARequestWhoseContextEnded(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1-public/login", strings.NewReader(loginBody))
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder()}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.ServeHTTP(spy, req) })
	assert.Equal(t, 1, f.LoginCount(), "the login ran")
	assert.Equal(t, 1, f.TokenCount(), "the token was created")
	assert.Empty(t, spy.calls, "nothing of the answer reached the connection")
}

// TestFacadeAnswersARequestWhoseContextIsAlive asserts that the abort applies
// only to a request whose context ended: a live request gets the whole
// answer.
func TestFacadeAnswersARequestWhoseContextIsAlive(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1-public/login", strings.NewReader(loginBody))
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"token"`)
}

// TestFacadeSendsNothingToAHalfClosedClient asserts, through a real
// connection, that a client which half-closes its side after sending a
// request reads end of stream or a reset and never a response, while the
// Facade still changes its state: the token the request deletes is gone.
func TestFacadeSendsNothingToAHalfClosedClient(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method == http.MethodDelete {
			testtransport.WaitUntilGone(r)
		}
	}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	login := postJSON(t, srv.Client(), srv.URL+"/v1-public/login", "",
		map[string]any{"type": "localProvider", "username": "e2e-user", "password": "e2e-pass", "responseType": "json"})
	name, bearer := login["id"].(string), login["token"].(string)
	_, owned := f.TokenOwner(name)
	require.True(t, owned)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "DELETE "+v3TokensPath+"/"+name+" HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer "+bearer+"\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))

	got, err := io.ReadAll(conn)
	if err != nil {
		if netErr, ok := errors.AsType[net.Error](err); ok {
			require.False(t, netErr.Timeout(), "the read timed out: the Facade neither answered nor closed the connection")
		}
	}
	assert.Empty(t, string(got), "the connection was aborted: no response is written")
	_, owned = f.TokenOwner(name)
	assert.False(t, owned, "the delete took effect")
	require.Len(t, f.DeleteRequests(), 1)
}

// TestFacadeAbortsAnAnswerThatTheContextCutsBetweenHeaderAndBody asserts that
// a request whose context ends between WriteHeader and the body write gets
// no answer: the body write is dropped, the connection is aborted, and the
// state change still happened. A header with an empty body would let a
// client read a status and fail to parse its body.
func TestFacadeAbortsAnAnswerThatTheContextCutsBetweenHeaderAndBody(t *testing.T) {
	t.Parallel()
	f := testrancher.New(testrancher.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1-public/login", strings.NewReader(loginBody))
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder(), afterWriteHeader: cancel}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.ServeHTTP(spy, req) })
	assert.Equal(t, []string{"WriteHeader"}, spy.calls, "the body write never reached the connection")
	assert.Equal(t, 1, f.LoginCount())
	assert.Equal(t, 1, f.TokenCount(), "the token was created")
}

// TestFacadeAbortsAPartialAnswerWhenTheContextEnds asserts that after part of
// an answer was written and flushed, a write that follows the end of the
// context is dropped, the flush after it does nothing, and the connection is
// aborted.
func TestFacadeAbortsAPartialAnswerWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		cancel()
		_, _ = w.Write([]byte("b"))
		w.(http.Flusher).Flush()
	})
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder()}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.ServeHTTP(spy, fallthroughRequest(ctx)) })
	assert.Equal(t, []string{"Write", "Flush"}, spy.calls, "only the part sent before the context ended, and no flush after the drop")
}

// TestFacadeAbortsAHandlerThatReturnsWithoutAnswering asserts that a handler
// which returns without writing, for a request whose context ended, gets an
// aborted connection and not net/http's implicit 200.
func TestFacadeAbortsAHandlerThatReturnsWithoutAnswering(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := facadeWithHandler(func(http.ResponseWriter, *http.Request) {})
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder()}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.ServeHTTP(spy, fallthroughRequest(ctx)) })
	assert.Empty(t, spy.calls)
}

// TestFacadeLeavesACompleteAnswerAloneWhenTheContextEndsAfterIt asserts that a
// request whose context ends after its whole answer was written is not
// aborted.
func TestFacadeLeavesACompleteAnswerAloneWhenTheContextEndsAfterIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("done"))
		cancel()
	})
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder()}
	require.NotPanics(t, func() { f.ServeHTTP(spy, fallthroughRequest(ctx)) })
	assert.Equal(t, []string{"Write"}, spy.calls)
}

// TestFacadeWrapperForwardsFlush asserts that, with a live context, the
// wrapper satisfies http.Flusher and the call reaches the wrapped writer.
func TestFacadeWrapperForwardsFlush(t *testing.T) {
	t.Parallel()
	spy := &controlSpy{ResponseWriter: httptest.NewRecorder()}
	var isFlusher bool
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		var flusher http.Flusher
		if flusher, isFlusher = w.(http.Flusher); isFlusher {
			flusher.Flush()
		}
	})
	f.ServeHTTP(spy, fallthroughRequest(t.Context()))
	require.True(t, isFlusher, "the wrapper must be an http.Flusher")
	assert.Equal(t, []string{"Flush"}, spy.calls)
}

// TestFacadeWrapperExposesTheWrappedWriterToResponseController asserts that
// an http.ResponseController built on the wrapper reaches an optional
// interface the wrapper does not implement itself.
func TestFacadeWrapperExposesTheWrappedWriterToResponseController(t *testing.T) {
	t.Parallel()
	spy := &controlSpy{ResponseWriter: httptest.NewRecorder()}
	var deadlineErr error
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		deadlineErr = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
	})
	f.ServeHTTP(spy, fallthroughRequest(t.Context()))
	require.NoError(t, deadlineErr)
	assert.Equal(t, []string{"SetWriteDeadline"}, spy.calls)
}

// TestFacadeWrapperHijackReachesTheWriterAndIsNotAborted asserts that a
// ResponseController's Hijack through the wrapper reaches the wrapped writer
// before anything was written, and that a request whose context ends after
// the hijack is not aborted: the handler owns the connection.
func TestFacadeWrapperHijackReachesTheWriterAndIsNotAborted(t *testing.T) {
	t.Parallel()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spy := &controlSpy{ResponseWriter: httptest.NewRecorder(), conn: server}
	var (
		conn     net.Conn
		err      error
		writeErr error
	)
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err = http.NewResponseController(w).Hijack()
		cancel()
		_, writeErr = w.Write([]byte("after the hijack"))
	})
	require.NotPanics(t, func() { f.ServeHTTP(spy, fallthroughRequest(ctx)) })
	require.NoError(t, err)
	assert.Same(t, server, conn)
	require.ErrorIs(t, writeErr, http.ErrHijacked, "a write after the hijack reaches the wrapped writer, which reports it")
	assert.Equal(t, []string{"Hijack", "Write"}, spy.calls)
}

// TestFacadeAbortsAFlushOnAnEndedContextBeforeAnyAnswer asserts that a
// handler which flushes after its request's context ended, before it wrote
// anything, sends nothing and gets an aborted connection: a flush would send
// an implicit 200.
func TestFacadeAbortsAFlushOnAnEndedContextBeforeAnyAnswer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := facadeWithHandler(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.(http.Flusher).Flush()
	})
	spy := &writeSpy{ResponseWriter: httptest.NewRecorder()}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() { f.ServeHTTP(spy, fallthroughRequest(ctx)) })
	assert.Empty(t, spy.calls, "the flush never reached the connection")
}
