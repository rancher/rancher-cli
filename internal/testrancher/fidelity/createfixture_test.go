package fidelity

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortFixtureRetryPause sets the pause between fixture create attempts for
// one test. Tests that call it set package state and must not run in
// parallel.
func shortFixtureRetryPause(t *testing.T, pause time.Duration) {
	t.Helper()
	old := fixtureRetryPause
	fixtureRetryPause = pause
	t.Cleanup(func() { fixtureRetryPause = old })
}

const (
	downstreamNamespaces = "/k8s/clusters/{downstream}/api/v1/namespaces"
	namedNamespaceBody   = `{"metadata":{"name":"e2e-fid-ns"}}`
)

// TestCreateFixtureRetriesAFiveHundredOnlyWhenARetryCannotCreateASecondObject
// asserts the rule: a fixture create retries a 5xx answer only on the
// probe's downstream cluster proxy path and only when the body names its
// object, at most fixtureRetryAttempts times; every other create is one
// request.
func TestCreateFixtureRetriesAFiveHundredOnlyWhenARetryCannotCreateASecondObject(t *testing.T) {
	shortFixtureRetryPause(t, time.Millisecond)
	tests := []struct {
		name     string
		path     string
		body     string
		statuses []int
		captured map[string]string
		requests int32
		wantErr  []string
	}{
		{
			name:     "downstream named body 500 500 201 is done",
			path:     downstreamNamespaces,
			body:     namedNamespaceBody,
			statuses: []int{500, 500, 201},
			requests: 3,
		},
		{
			name:     "downstream named body 503 then 200 is done",
			path:     downstreamNamespaces,
			body:     namedNamespaceBody,
			statuses: []int{503, 200},
			requests: 2,
		},
		{
			name:     "downstream named body always 500 stops at the bound",
			path:     downstreamNamespaces,
			body:     namedNamespaceBody,
			statuses: []int{500},
			requests: fixtureRetryAttempts,
			wantErr:  []string{"answered 500", "after 5 attempts"},
		},
		{
			name:     "downstream named body 500 then 409 is done",
			path:     downstreamNamespaces,
			body:     namedNamespaceBody,
			statuses: []int{500, 409},
			requests: 2,
		},
		{
			name:     "downstream named body 403 is one request",
			path:     downstreamNamespaces,
			body:     namedNamespaceBody,
			statuses: []int{403},
			requests: 1,
			wantErr:  []string{"answered 403"},
		},
		{
			name:     "downstream generateName body 500 is one request",
			path:     downstreamNamespaces,
			body:     `{"metadata":{"generateName":"e2e-fid-"}}`,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "downstream body with a name and generateName 500 is one request",
			path:     downstreamNamespaces,
			body:     `{"metadata":{"name":"e2e-fid-ns","generateName":"e2e-fid-"}}`,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "downstream body without a name 500 is one request",
			path:     downstreamNamespaces,
			body:     `{"metadata":{"labels":{"a":"b"}}}`,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "downstream body that is not JSON 500 is one request",
			path:     downstreamNamespaces,
			body:     `metadata: name`,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "local cluster proxy path named body 500 is one request",
			path:     "/k8s/clusters/local/apis/management.cattle.io/v3/projects",
			body:     namedNamespaceBody,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "another cluster proxy path named body 500 is one request",
			path:     "/k8s/clusters/c-m-other/api/v1/namespaces",
			body:     namedNamespaceBody,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "local path named body 500 is one request",
			path:     "/v3/users",
			body:     namedNamespaceBody,
			statuses: []int{500},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "downstream path without a downstream capture 500 is one request",
			path:     "/k8s/clusters/" + testDownstream + "/api/v1/namespaces",
			body:     namedNamespaceBody,
			statuses: []int{500},
			captured: map[string]string{},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
		{
			name:     "downstream path with an empty downstream capture 500 is one request",
			path:     "/k8s/clusters//api/v1/namespaces",
			body:     namedNamespaceBody,
			statuses: []int{500},
			captured: map[string]string{"downstream": ""},
			requests: 1,
			wantErr:  []string{"answered 500"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, n := statusSequence(t, tc.statuses...)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			captured := tc.captured
			if captured == nil {
				captured = map[string]string{"downstream": testDownstream}
			}
			err := createFixture("namespace", tc.path, tc.body).Do(ctx, adminTarget(srv), captured)
			if len(tc.wantErr) == 0 {
				require.NoError(t, err)
			} else {
				for _, want := range tc.wantErr {
					require.ErrorContains(t, err, want)
				}
				if tc.requests == 1 {
					assert.NotContains(t, err.Error(), "attempts")
				}
			}
			assert.Equal(t, tc.requests, n.Load())
		})
	}
}

// TestCreateFixtureRetryEndsWithTheContext asserts that a context that ends
// during the pause between attempts returns the context error without
// another request.
func TestCreateFixtureRetryEndsWithTheContext(t *testing.T) {
	shortFixtureRetryPause(t, time.Hour)
	srv, n := statusSequence(t, http.StatusInternalServerError)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := createFixture("namespace", downstreamNamespaces, namedNamespaceBody).Do(ctx, adminTarget(srv), map[string]string{"downstream": testDownstream})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "answered 500, retry ended")
	assert.Less(t, time.Since(start), 10*time.Second)
	assert.Equal(t, int32(1), n.Load())
}

// TestCreateFixtureTransportErrorIsNotRetried asserts that a create whose
// connection drops after the server read the request fails the step after
// that one request, since the create may have committed before the answer
// was lost.
func TestCreateFixtureTransportErrorIsNotRetried(t *testing.T) {
	shortFixtureRetryPause(t, time.Millisecond)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	err := createFixture("namespace", downstreamNamespaces, namedNamespaceBody).Do(t.Context(), adminTarget(srv), map[string]string{"downstream": testDownstream})
	require.ErrorContains(t, err, "setup namespace")
	assert.Equal(t, int32(1), n.Load())
}

// TestCreateFixtureRetrySendsTheWholeBodyEachTime asserts that every attempt
// of a retried create sends the same, complete body.
func TestCreateFixtureRetrySendsTheWholeBodyEachTime(t *testing.T) {
	shortFixtureRetryPause(t, time.Millisecond)
	var (
		mu     sync.Mutex
		bodies []string
	)
	statuses := []int{http.StatusInternalServerError, http.StatusInternalServerError, http.StatusCreated}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(data))
		i := len(bodies) - 1
		mu.Unlock()
		w.WriteHeader(statuses[min(i, len(statuses)-1)])
	}))
	t.Cleanup(srv.Close)
	err := createFixture("namespace", downstreamNamespaces, namedNamespaceBody).Do(t.Context(), adminTarget(srv), map[string]string{"downstream": testDownstream})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{namedNamespaceBody, namedNamespaceBody, namedNamespaceBody}, bodies)
}
