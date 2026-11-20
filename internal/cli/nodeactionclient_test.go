package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeActionRequest is a request the node action test server received.
type nodeActionRequest struct {
	method      string
	escapedPath string
	query       string
	authorized  string
	accept      string
	contentType string
	body        string
}

// nodeActionServer records every request and responds to each with its answer function.
type nodeActionServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []nodeActionRequest
	answer func(w http.ResponseWriter, r *http.Request)
}

func newNodeActionServer(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *nodeActionServer {
	t.Helper()
	s := &nodeActionServer{answer: answer}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, nodeActionRequest{
			method: r.Method, escapedPath: r.URL.EscapedPath(), query: r.URL.RawQuery,
			authorized: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"), contentType: r.Header.Get("Content-Type"), body: string(body),
		})
		s.mu.Unlock()
		s.answer(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *nodeActionServer) client() *nodeActionClient {
	return &nodeActionClient{base: s.srv.URL, token: "tok", hc: s.srv.Client()}
}

func (s *nodeActionServer) received() []nodeActionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nodeActionRequest(nil), s.reqs...)
}

func answerWith(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// TestNodeActionClient asserts the requests of the node action client: the
// node GET and the action POST go to /v3/nodes/<cluster>:<node> with the
// bearer, drain with a JSON body and a content type, stop drain with neither,
// no field manager, one send per call even on a 500, a response that is not a
// node read as an error, the actions of a node as sorted names, and an action
// name outside the two refused before anything is sent.
func TestNodeActionClient(t *testing.T) {
	t.Parallel()
	t.Run("the node GET", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, answerWith(200, `{"id":"c-1:m-1","state":"active","actions":{"stopDrain":"x","drain":"y"}}`))
		node, err := s.client().GetNode(t.Context(), "c-1", "m-1")
		require.NoError(t, err)
		assert.Equal(t, engine.NormanNode{State: "active", Actions: []string{"drain", "stopDrain"}, HasActions: true}, node)
		reqs := s.received()
		require.Len(t, reqs, 1)
		assert.Equal(t, nodeActionRequest{method: "GET", escapedPath: "/v3/nodes/c-1:m-1", authorized: "Bearer tok", accept: "application/json"}, reqs[0])
	})
	t.Run("a node with no actions", func(t *testing.T) {
		t.Parallel()
		for name, body := range map[string]string{
			"no key":     `{"state":"cordoned"}`,
			"empty map":  `{"state":"cordoned","actions":{}}`,
			"null value": `{"state":"cordoned","actions":null}`,
		} {
			s := newNodeActionServer(t, answerWith(200, body))
			node, err := s.client().GetNode(t.Context(), "c-1", "m-1")
			require.NoError(t, err, name)
			assert.Equal(t, engine.NormanNode{State: "cordoned"}, node, name)
		}
	})
	t.Run("the ids are escaped as one path segment", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, answerWith(200, `{}`))
		_, err := s.client().GetNode(t.Context(), "c/1", "m 1")
		require.NoError(t, err)
		assert.Equal(t, "/v3/nodes/c%2F1:m%201", s.received()[0].escapedPath)
	})
	t.Run("an answer that is not a node", func(t *testing.T) {
		t.Parallel()
		for name, body := range map[string]string{
			"not JSON":            `<html>`,
			"a JSON array":        `[]`,
			"JSON null":           `null`,
			"a JSON string":       `"x"`,
			"actions is an array": `{"actions":["drain"]}`,
			"actions is a string": `{"actions":"drain"}`,
		} {
			s := newNodeActionServer(t, answerWith(200, body))
			_, err := s.client().GetNode(t.Context(), "c-1", "m-1")
			ce := clierr.From(err)
			require.NotNil(t, ce, name)
			assert.Equal(t, clierr.CodeGeneric, ce.Code, name)
			assert.False(t, ce.Retryable, name)
			require.ErrorContains(t, err, "could not read management node m-1 of cluster c-1 from GET /v3/nodes/c-1:m-1: ", name)
		}
	})
	t.Run("a refused GET keeps the code of the status table", func(t *testing.T) {
		t.Parallel()
		for status, code := range map[int]string{403: clierr.CodeRBAC, 404: clierr.CodeNotFound, 422: clierr.CodeValidation} {
			s := newNodeActionServer(t, answerWith(status, `{"type":"error","message":"refused"}`))
			_, err := s.client().GetNode(t.Context(), "c-1", "m-1")
			ce := clierr.From(err)
			require.NotNil(t, ce)
			assert.Equal(t, code, ce.Code, status)
			got, ok := clierr.HTTPStatus(err)
			assert.True(t, ok)
			assert.Equal(t, status, got)
			assert.Len(t, s.received(), 1)
		}
	})
	t.Run("drain sends the body and the content type", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, answerWith(200, `{}`))
		require.NoError(t, s.client().PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionDrain, []byte(`{"force":true}`)))
		reqs := s.received()
		require.Len(t, reqs, 1)
		assert.Equal(t, nodeActionRequest{method: "POST", escapedPath: "/v3/nodes/c-1:m-1", query: "action=drain", authorized: "Bearer tok",
			accept: "application/json", contentType: "application/json", body: `{"force":true}`}, reqs[0])
		assert.NotContains(t, reqs[0].query, "fieldManager")
	})
	t.Run("stop drain sends no body and no content type", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, answerWith(200, `{}`))
		require.NoError(t, s.client().PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionStopDrain, nil))
		reqs := s.received()
		require.Len(t, reqs, 1)
		assert.Equal(t, nodeActionRequest{method: "POST", escapedPath: "/v3/nodes/c-1:m-1", query: "action=stopDrain", authorized: "Bearer tok", accept: "application/json"}, reqs[0])
	})
	t.Run("one send on a 500, 502 and 504", func(t *testing.T) {
		t.Parallel()
		for _, status := range []int{500, 502, 504} {
			s := newNodeActionServer(t, answerWith(status, `{"message":"boom"}`))
			err := s.client().PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionDrain, []byte(`{}`))
			var unknown *engine.ActionOutcomeUnknown
			require.ErrorAs(t, err, &unknown, status)
			got, ok := clierr.HTTPStatus(err)
			assert.True(t, ok)
			assert.Equal(t, status, got)
			assert.Len(t, s.received(), 1, "never retried")
		}
	})
	t.Run("an error after the request was written is an unknown outcome", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		err := s.client().PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionStopDrain, nil)
		var unknown *engine.ActionOutcomeUnknown
		require.ErrorAs(t, err, &unknown)
		assert.Len(t, s.received(), 1)
	})
	t.Run("a refusal carries the status table's error and the request", func(t *testing.T) {
		t.Parallel()
		for status, code := range map[int]string{403: clierr.CodeRBAC, 404: clierr.CodeNotFound, 409: clierr.CodeConflict, 422: clierr.CodeValidation} {
			s := newNodeActionServer(t, answerWith(status, `{"type":"error","message":"refused"}`))
			err := s.client().PostNodeAction(t.Context(), "c-1", "m-1", engine.NodeActionDrain, []byte(`{}`))
			var refused *engine.ActionRefused
			require.ErrorAs(t, err, &refused, status)
			assert.Equal(t, code, refused.Err.Code, status)
			require.NotNil(t, refused.Request)
			assert.Len(t, s.received(), 1)
		}
	})
	t.Run("another action name sends nothing", func(t *testing.T) {
		t.Parallel()
		s := newNodeActionServer(t, answerWith(200, `{}`))
		for _, action := range []engine.NodeAction{"", "cordon", "Drain", "drain&x=1"} {
			err := s.client().PostNodeAction(t.Context(), "c-1", "m-1", action, nil)
			ce := clierr.From(err)
			require.NotNil(t, ce, action)
			assert.Contains(t, ce.Message, "internal error", action)
		}
		assert.Empty(t, s.received())
	})
}
