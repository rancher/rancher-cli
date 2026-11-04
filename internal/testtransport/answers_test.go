package testtransport

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnswersGiveEveryRequestTheSameAnswerAndCountThem asserts the server
// answers each request with its status, headers and body, on TLS and on
// plain HTTP, reads a request body, counts requests and keeps their paths.
func TestAnswersGiveEveryRequestTheSameAnswerAndCountThem(t *testing.T) {
	t.Parallel()
	for name, useTLS := range map[string]bool{"tls": true, "plain": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := NewAnswers(t, useTLS, http.StatusServiceUnavailable, "120", "application/json", `{"a":1}`)
			assert.Equal(t, useTLS, a.CertPEM() != "")
			for _, path := range []string{"/x", "/y"} {
				resp, err := a.Client().Post(a.URL()+path, "text/plain", strings.NewReader("payload"))
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
				assert.Equal(t, "120", resp.Header.Get("Retry-After"))
				assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
				assert.JSONEq(t, `{"a":1}`, string(body))
			}
			assert.Equal(t, 2, a.Count())
			assert.Equal(t, []string{"/x", "/y"}, a.Paths())
		})
	}
}

// TestAnswersLeaveOutHeadersThatAreEmpty asserts a server given no
// Retry-After and no content type sends neither.
func TestAnswersLeaveOutHeadersThatAreEmpty(t *testing.T) {
	t.Parallel()
	a := NewAnswers(t, false, http.StatusBadGateway, "", "", "")
	resp, err := a.Client().Get(a.URL())
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Empty(t, resp.Header.Get("Retry-After"))
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}
