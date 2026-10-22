package provider

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
)

// TestOwnerNamesTheFieldManager asserts the SelfSubjectReview that auth
// login's owner check creates carries fieldManager=cli.rancher.io. Creating
// it with empty CreateOptions fails it.
func TestOwnerNamesTheFieldManager(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		managers []string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		managers = append(managers, r.URL.Query().Get("fieldManager"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","status":{"userInfo":{"username":"u-abc12"}}}`))
	}))
	t.Cleanup(srv.Close)
	got, err := Owner(t.Context(), config.Effective{URL: srv.URL, CA: serverCA(srv), Token: "id:secret"})
	require.NoError(t, err)
	assert.Equal(t, "u-abc12", got)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{kube.FieldManager}, managers)
}

// TestPostReviewNamesTheFieldManagerOnEveryReviewKind asserts each of the
// four review kinds is posted with fieldManager=cli.rancher.io and the rest
// of the URL unchanged. Building the request with http.NewRequestWithContext
// instead of kube.NewWriteRequest fails it.
func TestPostReviewNamesTheFieldManagerOnEveryReviewKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind engine.ReviewKind
		obj  string
		path string
	}{
		{engine.ReviewSubjectAccess, "SubjectAccessReview", "/apis/authorization.k8s.io/v1/subjectaccessreviews"},
		{engine.ReviewSelfSubjectAccess, "SelfSubjectAccessReview", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"},
		{engine.ReviewSelfSubjectRules, "SelfSubjectRulesReview", "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews"},
		{engine.ReviewSelfSubjectIdentity, "SelfSubjectReview", "/apis/authentication.k8s.io/v1/selfsubjectreviews"},
	} {
		t.Run(tc.obj, func(t *testing.T) {
			t.Parallel()
			var (
				mu      sync.Mutex
				queries []string
				paths   []string
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				queries = append(queries, r.URL.RawQuery)
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(srv.Close)
			c := clientFor(t, srv)
			_, err := c.PostReview(t.Context(), engine.ReviewRequest{Kind: tc.kind, Object: reviewObj(tc.obj)})
			require.NoError(t, err)
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []string{"fieldManager=" + kube.FieldManager}, queries)
			assert.Equal(t, []string{tc.path}, paths)
		})
	}
}
