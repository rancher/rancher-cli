package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testtransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const actionTestPath = "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/repo1?action=install"

// actionServer is a TLS server that counts the requests it reads and
// answers each with handle.
func actionServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = io.ReadAll(r.Body)
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// hangUp closes the connection after the request was read, with no answer.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		if tc, ok := conn.(interface{ SetLinger(int) error }); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	}
}

// TestActionClientOutcome asserts how the action client reports each
// answer and each failure, with exactly one request sent when the server
// read one: an error before the request was written sent nothing and is
// an ordinary transport error; any error after it was written, and a 500,
// 502 or 504, is *engine.ActionOutcomeUnknown; every other status goes
// through the status table. The waiting cases end the wait only once the
// server has read the request, so no wall-clock margin decides them. The
// test asserts the outcome, not the OS's text, so a reset reads the same on
// every OS.
func TestActionClientOutcome(t *testing.T) {
	t.Parallel()
	answer := func(status int, body string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	cases := map[string]struct {
		handle func(http.ResponseWriter, *http.Request)
		// end, when set, ends the client's wait once the server has read
		// the request: it gets the cancel function of the request's
		// context.
		end      func(cancel context.CancelCauseFunc)
		unknown  bool
		status   int
		code     string
		requests int32
	}{
		"201":                     {handle: answer(201, `{"operationName":"op"}`), requests: 1},
		"409":                     {handle: answer(409, `{"message":"exists"}`), code: clierr.CodeConflict, requests: 1},
		"400":                     {handle: answer(400, `{"message":"bad"}`), code: clierr.CodeValidation, requests: 1},
		"429":                     {handle: answer(429, `{"message":"slow down"}`), code: clierr.CodeGeneric, requests: 1},
		"503":                     {handle: answer(503, ``), code: clierr.CodeGeneric, requests: 1},
		"500":                     {handle: answer(500, `{"message":"fetch failed"}`), unknown: true, status: 500, requests: 1},
		"502":                     {handle: answer(502, ``), unknown: true, status: 502, requests: 1},
		"504":                     {handle: answer(504, ``), unknown: true, status: 504, requests: 1},
		"reset":                   {handle: hangUp, unknown: true, requests: 1},
		"cancelled while waiting": {end: func(cancel context.CancelCauseFunc) { cancel(nil) }, unknown: true, requests: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			handle := tc.handle
			if tc.end != nil {
				read := make(chan struct{})
				handle = func(_ http.ResponseWriter, r *http.Request) {
					close(read)
					testtransport.HangAndAbort(r)
				}
				go func() {
					<-read
					tc.end(cancel)
				}()
			}
			srv, n := actionServer(t, handle)
			c := &actionClient{base: srv.URL, token: "token-1", hc: srv.Client()}
			body, err := c.PostAction(ctx, actionTestPath, []byte(`{}`))
			assert.Equal(t, tc.requests, n.Load())
			if tc.code == "" && !tc.unknown {
				require.NoError(t, err)
				assert.JSONEq(t, `{"operationName":"op"}`, string(body))
				return
			}
			require.Error(t, err)
			_, isUnknown := errors.AsType[*engine.ActionOutcomeUnknown](err)
			assert.Equal(t, tc.unknown, isUnknown)
			if tc.unknown {
				status, _ := clierr.HTTPStatus(err)
				assert.Equal(t, tc.status, status)
				return
			}
			assert.Equal(t, tc.code, clierr.From(err).Code)
		})
	}
}

// TestActionClientNothingWritten asserts the failures before the request
// is written are ordinary transport errors, not an unknown outcome: a
// refused connection, a TLS handshake the client does not trust, and a
// context cancelled before the request.
func TestActionClientNothingWritten(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	untrusted, n := actionServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) })
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	trusted, m := actionServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201) })

	cases := map[string]struct {
		c   *actionClient
		ctx context.Context
	}{
		"refused":          {&actionClient{base: "https://" + addr, hc: &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}}, t.Context()},
		"untrusted TLS":    {&actionClient{base: untrusted.URL, hc: &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}}, t.Context()},
		"cancelled before": {&actionClient{base: trusted.URL, hc: trusted.Client()}, cancelled},
	}
	for name, tc := range cases {
		_, err := tc.c.PostAction(tc.ctx, actionTestPath, []byte(`{}`))
		require.Error(t, err, name)
		_, isUnknown := errors.AsType[*engine.ActionOutcomeUnknown](err)
		assert.False(t, isUnknown, name)
	}
	assert.Zero(t, n.Load())
	assert.Zero(t, m.Load())
}

// TestActionClientRequest asserts the request the client sends: one POST
// to the path with fieldManager added to its query, the bearer, and the
// JSON body.
func TestActionClientRequest(t *testing.T) {
	t.Parallel()
	var got *http.Request
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(201)
	}))
	t.Cleanup(srv.Close)
	c := &actionClient{base: srv.URL, token: "token-1", hc: srv.Client()}
	_, err := c.PostAction(t.Context(), actionTestPath, []byte(`{"a":1}`))
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/k8s/clusters/c-1/v1/catalog.cattle.io.clusterrepos/repo1", got.URL.Path)
	assert.Equal(t, "install", got.URL.Query().Get("action"))
	assert.Equal(t, "cli.rancher.io", got.URL.Query().Get("fieldManager"))
	assert.Equal(t, "Bearer token-1", got.Header.Get("Authorization"))
	assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
	assert.JSONEq(t, `{"a":1}`, string(body))
}

// TestActionClientBoundsTheAnswer asserts the action client reads at most
// maxActionAnswer bytes of an answer: a longer body is cut to the bound, a
// short one is returned whole, for an unknown outcome and for a refusal.
func TestActionClientBoundsTheAnswer(t *testing.T) {
	t.Parallel()
	long := `{"message":"` + strings.Repeat("m", maxActionAnswer) + `"}`
	cases := map[string]struct {
		status int
		body   string
		want   int
	}{
		"over the bound, 500": {500, long, maxActionAnswer},
		"short, 500":          {500, `{"message":"short"}`, len(`{"message":"short"}`)},
		"over the bound, 403": {403, long, maxActionAnswer},
		"short, 403":          {403, `{"message":"no"}`, len(`{"message":"no"}`)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := actionServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			c := &actionClient{base: srv.URL, token: "token-1", hc: srv.Client()}
			_, err := c.PostAction(t.Context(), actionTestPath, []byte(`{}`))
			require.Error(t, err)
			var body []byte
			if u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err); ok {
				body = u.Body
			}
			if r, ok := errors.AsType[*engine.ActionRefused](err); ok {
				body = r.Body
			}
			assert.Len(t, body, tc.want)
		})
	}
}

// TestActionClientHandsEveryNonSuccessAnswerOver asserts every answer from
// 300 to 599 comes back as an *engine.ActionOutcomeUnknown (500, 502, 504)
// or an *engine.ActionRefused, with the body the server sent, so no status
// reaches the caller as text the table cut before the values are hidden.
func TestActionClientHandsEveryNonSuccessAnswerOver(t *testing.T) {
	t.Parallel()
	srv, _ := actionServer(t, func(w http.ResponseWriter, r *http.Request) {
		code, _ := strconv.Atoi(r.URL.Query().Get("status"))
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"message":"answer `+strconv.Itoa(code)+`"}`)
	})
	c := &actionClient{base: srv.URL, token: "token-1", hc: srv.Client()}
	for code := 300; code <= 599; code++ {
		_, err := c.PostAction(t.Context(), actionTestPath+"&status="+strconv.Itoa(code), []byte(`{}`))
		require.Error(t, err, code)
		want := `{"message":"answer ` + strconv.Itoa(code) + `"}`
		if code == http.StatusNotModified {
			want = ""
		}
		var body []byte
		switch code {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
			u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err)
			require.True(t, ok, code)
			body = u.Body
		default:
			r, ok := errors.AsType[*engine.ActionRefused](err)
			require.True(t, ok, code)
			body = r.Body
		}
		assert.Equal(t, want, string(body), code)
	}
}
