package token

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/kube"
)

// writeRecorder is a server that records the method, path and raw query of
// each request and answers with status.
type writeRecorder struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
}

func newWriteRecorder(t *testing.T, status int, body string) *writeRecorder {
	t.Helper()
	rec := &writeRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.requests = append(rec.requests, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (rec *writeRecorder) seen() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.requests...)
}

// TestDerivedTokenCreateNamesTheFieldManager asserts the ext Token create
// that a login and the exec plugin send carries fieldManager=cli.rancher.io.
// Building the request in CreateDerivedToken with http.NewRequestWithContext
// instead of kube.NewWriteRequest fails it.
func TestDerivedTokenCreateNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	rec := newWriteRecorder(t, http.StatusNotFound, "")
	_, err := CreateDerivedToken(t.Context(), rec.srv.Client(), rec.srv.URL, "tok:secret", "c-test", 0, "d")
	require.ErrorIs(t, err, ErrExtGroupAbsent)
	assert.Equal(t, []string{"POST " + ExtTokensPath + "?fieldManager=" + kube.FieldManager}, rec.seen())
}

// TestNormanTokenCreateSendsNoFieldManager asserts the v3 token create goes
// to Rancher's own API with no fieldManager parameter, because it is not a
// Kubernetes write. Building that request with kube.NewWriteRequest fails
// it.
func TestNormanTokenCreateSendsNoFieldManager(t *testing.T) {
	t.Parallel()
	rec := newWriteRecorder(t, http.StatusNotFound, "")
	_, err := createV3ClusterToken(t.Context(), rec.srv.Client(), rec.srv.URL, "tok:secret", "c-test", "d")
	require.Error(t, err)
	assert.Equal(t, []string{"POST " + normanTokensPath + "?"}, rec.seen())
}

// TestOwnerRequestNamesTheFieldManager asserts the sign-in owner check's
// SelfSubjectReview carries fieldManager=cli.rancher.io and keeps the
// escaped cluster id. Building the request with http.NewRequestWithContext
// fails it.
func TestOwnerRequestNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	rec := newWriteRecorder(t, http.StatusCreated, `{"status":{"userInfo":{"username":"u-abc12"}}}`)
	got, err := requestOwner(t.Context(), rec.srv.Client(), rec.srv.URL, "c-1/x", "tok:secret")
	require.NoError(t, err)
	assert.Equal(t, "u-abc12", got)
	assert.Equal(t, []string{"POST /k8s/clusters/c-1%2Fx" + selfSubjectReviewPath + "?fieldManager=" + kube.FieldManager}, rec.seen())
}

// TestProbeDirectNamesTheFieldManager asserts the SelfSubjectReview that
// waits for a token on a direct endpoint carries fieldManager=cli.rancher.io.
// Building the request with http.NewRequestWithContext fails it.
func TestProbeDirectNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	rec := newWriteRecorder(t, http.StatusCreated, `{"status":{"userInfo":{"username":"u-abc12"}}}`)
	assert.Equal(t, probeReady, probeDirect(t.Context(), rec.srv.Client(), rec.srv.URL+"/", "tok:secret"))
	assert.Equal(t, []string{"POST /apis/authentication.k8s.io/v1/selfsubjectreviews?fieldManager=" + kube.FieldManager}, rec.seen())
}
