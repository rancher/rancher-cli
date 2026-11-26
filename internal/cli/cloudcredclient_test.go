package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/secret"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const requestSecret = "fake-request-secret-value-0001"

// sentRequest is what the test server saw of one request.
type sentRequest struct {
	method  string
	path    string
	header  http.Header
	body    []byte
	hasBody bool
}

// norman is a TLS test server that records each request and responds with
// status and response, or drops the connection when lose is set.
type norman struct {
	mu       sync.Mutex
	requests []sentRequest
	status   int
	answer   string
	lose     bool
	srv      *httptest.Server
}

func newNorman(t *testing.T) *norman {
	t.Helper()
	n := &norman{status: http.StatusCreated, answer: `{"id":"cattle-global-data:cc-abcde"}`}
	n.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.requests = append(n.requests, sentRequest{method: r.Method, path: r.URL.EscapedPath(), header: r.Header.Clone(), body: body, hasBody: len(body) > 0})
		status, answer, lose := n.status, n.answer, n.lose
		n.mu.Unlock()
		if lose {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *norman) seen() []sentRequest {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]sentRequest(nil), n.requests...)
}

// credentialClientFor is the cloud credential client of a factory that has a stored
// login for the server.
func credentialClientFor(t *testing.T, n *norman) engine.CloudCredentialClient {
	t.Helper()
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	c, err := seedStoredLogin(t, n.srv, "    provenance: userSupplied\n").CloudCredentials()
	require.NoError(t, err)
	return c
}

func clientBody() engine.CloudCredentialBody {
	return engine.NewCloudCredentialBody(map[string]any{
		"type":                      "cloudCredential",
		"name":                      "prod",
		"amazonec2credentialConfig": map[string]any{"accessKey": "AKIAFAKE", "secretKey": requestSecret},
	})
}

// TestCloudCredentialClientSendsOneRequestPerCall asserts each method sends
// one request with the caller's bearer, the JSON headers and the User-Agent,
// to the collection path (create) or the item path with the id escaped as one
// segment (update and delete), with a JSON body for create and update only,
// and returns the id of Rancher's response.
func TestCloudCredentialClientSendsOneRequestPerCall(t *testing.T) {
	n := newNorman(t)
	n.status = http.StatusOK
	client := credentialClientFor(t, n)

	n.status = http.StatusCreated
	id, err := client.Create(t.Context(), clientBody())
	require.NoError(t, err)
	assert.Equal(t, "cattle-global-data:cc-abcde", id)
	n.status, n.answer = http.StatusOK, `{}`
	require.NoError(t, client.Update(t.Context(), "cattle-global-data:cc-abcde", clientBody()))
	require.NoError(t, client.Delete(t.Context(), "a/b c"))

	got := n.seen()
	require.Len(t, got, 3)
	for _, r := range got {
		assert.Equal(t, "Bearer token-abc:secret", r.header.Get("Authorization"))
		assert.Equal(t, "application/json", r.header.Get("Accept"))
		assert.NotEmpty(t, r.header.Get("User-Agent"))
		assert.NotContains(t, r.path, "fieldManager")
	}
	assert.Equal(t, http.MethodPost, got[0].method)
	assert.Equal(t, "/v3/cloudcredentials", got[0].path)
	assert.Equal(t, http.MethodPut, got[1].method)
	assert.Equal(t, "/v3/cloudCredentials/cattle-global-data:cc-abcde", got[1].path)
	assert.Equal(t, http.MethodDelete, got[2].method)
	assert.Equal(t, "/v3/cloudCredentials/a%2Fb%20c", got[2].path)

	for _, r := range got[:2] {
		assert.Equal(t, "application/json", r.header.Get("Content-Type"))
		var sent map[string]any
		require.NoError(t, json.Unmarshal(r.body, &sent))
		assert.Equal(t, "cloudCredential", sent["type"])
		assert.Equal(t, requestSecret, sent["amazonec2credentialConfig"].(map[string]any)["secretKey"])
	}
	assert.False(t, got[2].hasBody)
	assert.Empty(t, got[2].header.Get("Content-Type"))
}

// TestCloudCredentialClientSendsNothingForAValueItCannotReveal asserts a
// create and an update whose body has a secret.Value at a position the body
// does not reveal fail with an internal error that names no value and send no
// request.
func TestCloudCredentialClientSendsNothingForAValueItCannotReveal(t *testing.T) {
	n := newNorman(t)
	client := credentialClientFor(t, n)
	body := engine.NewCloudCredentialBody(map[string]any{
		"type":                      "cloudCredential",
		"amazonec2credentialConfig": map[string]any{"secretKey": []secret.Value{secret.New(requestSecret)}},
	})
	_, err := client.Create(t.Context(), body)
	require.ErrorContains(t, err, "internal error: the cloud credential request body cannot be encoded")
	require.Error(t, client.Update(t.Context(), "cattle-global-data:cc-abcde", body))
	assert.NotContains(t, err.Error(), requestSecret)
	assert.Empty(t, n.seen())
}

// TestCloudCredentialClientResponseClasses asserts a 500, 502 and 504 are an
// ActionOutcomeUnknown, every other non-2xx response an ActionRefused, each with
// an Err that has the status and a fixed text and none of the response's
// text, the response only in Body, and that one request is sent whatever the
// response.
func TestCloudCredentialClientResponseClasses(t *testing.T) {
	for _, tc := range []struct {
		status  int
		unknown bool
	}{
		{http.StatusBadRequest, false}, {http.StatusUnauthorized, false}, {http.StatusForbidden, false},
		{http.StatusNotFound, false}, {http.StatusConflict, false}, {http.StatusUnprocessableEntity, false},
		{http.StatusInternalServerError, true}, {http.StatusBadGateway, true}, {http.StatusGatewayTimeout, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			n := newNorman(t)
			n.status = tc.status
			n.answer = `{"type":"error","message":"echo ` + requestSecret + `"}`
			client := credentialClientFor(t, n)
			for name, call := range map[string]func() error{
				"create": func() error { _, err := client.Create(t.Context(), clientBody()); return err },
				"update": func() error { return client.Update(t.Context(), "cc-x", clientBody()) },
				"delete": func() error { return client.Delete(t.Context(), "cc-x") },
			} {
				before := len(n.seen())
				err := call()
				require.Error(t, err, name)
				assert.Len(t, n.seen(), before+1, name+": one request, never retried")
				assert.NotContains(t, err.Error(), requestSecret, name)
				if tc.unknown {
					u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err)
					require.True(t, ok, "%s: %T", name, err)
					assert.Contains(t, string(u.Body), requestSecret, name)
					status, ok := clierr.HTTPStatus(u.Err)
					require.True(t, ok, name)
					assert.Equal(t, tc.status, status, name)
					continue
				}
				r, ok := errors.AsType[*engine.ActionRefused](err)
				require.True(t, ok, "%s: %T", name, err)
				assert.Contains(t, string(r.Body), requestSecret, name)
				status, ok := clierr.HTTPStatus(r.Err)
				require.True(t, ok, name)
				assert.Equal(t, tc.status, status, name)
				assert.NotNil(t, r.Request, name)
			}
		})
	}
}

// TestCloudCredentialClientLostResponseIsUnknown asserts a connection that
// drops after the request was written is an ActionOutcomeUnknown with a fixed
// text and no body, and a request that cannot be sent at all is not.
func TestCloudCredentialClientLostResponseIsUnknown(t *testing.T) {
	n := newNorman(t)
	n.lose = true
	client := credentialClientFor(t, n)
	_, err := client.Create(t.Context(), clientBody())
	u, ok := errors.AsType[*engine.ActionOutcomeUnknown](err)
	require.True(t, ok, "%T", err)
	assert.Nil(t, u.Body)
	assert.NotContains(t, err.Error(), requestSecret)
	assert.Len(t, n.seen(), 1, "never retried")

	n.srv.Close()
	_, err = client.Create(t.Context(), clientBody())
	require.Error(t, err)
	_, unknown := errors.AsType[*engine.ActionOutcomeUnknown](err)
	assert.False(t, unknown, "a request that never reached the server sent nothing")
	assert.NotContains(t, err.Error(), requestSecret)
}

// TestCloudCredentialClientCreateNeedsAnID asserts a 2xx response with no id
// is an ActionOutcomeUnknown: the credential may exist.
func TestCloudCredentialClientCreateNeedsAnID(t *testing.T) {
	for _, answer := range []string{`{}`, `not json`, `{"id":""}`} {
		n := newNorman(t)
		n.answer = answer
		_, err := credentialClientFor(t, n).Create(t.Context(), clientBody())
		_, ok := errors.AsType[*engine.ActionOutcomeUnknown](err)
		assert.True(t, ok, answer)
		assert.NotContains(t, err.Error(), requestSecret, answer)
	}
}
