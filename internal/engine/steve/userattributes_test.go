package steve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

const (
	uaByIDPath      = "/v1/management.cattle.io.userattributes/u-abc"
	uaCollectPath   = "/v1/management.cattle.io.userattributes"
	uaSchemaPath    = "/v1/schemas/management.cattle.io.userattribute"
	uaRemedyText    = "; a GlobalRole with get and list on userattributes.management.cattle.io allows it; see: "
	uaMissingBody   = `{"type":"error","links":{},"code":"NotFound","message":"userattributes.management.cattle.io \"u-abc\" not found","status":404}`
	uaObjectBody    = `{"id":"u-abc","type":"management.cattle.io.userattribute","links":{},"metadata":{"name":"u-abc"},"GroupPrincipals":{}}`
	uaListCheckPath = "/k8s/clusters/local/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
)

// uaAnswer is what a scripted steve answers for one path.
type uaAnswer struct {
	status int
	body   string
}

// uaSteve is a scripted steve: answers by request path, and records them.
type uaSteve struct {
	mu      sync.Mutex
	answers map[string]uaAnswer
	paths   []string
}

func (s *uaSteve) get(ctx context.Context, r Request) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, r.Path())
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a, ok := s.answers[r.Path()]
	if !ok {
		return nil, clierr.ResponseError(http.MethodGet, http.StatusNotFound, nil)
	}
	if a.status >= 200 && a.status <= 299 {
		return []byte(a.body), nil
	}
	return nil, clierr.ResponseError(http.MethodGet, a.status, []byte(a.body))
}

func (s *uaSteve) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// uaLocal is a scripted local cluster client: it answers the list check
// and records the reviews posted.
type uaLocal struct {
	engine.ClusterClient
	mu      sync.Mutex
	reviews []engine.ReviewRequest
	answer  func(ctx context.Context) (engine.ReviewResponse, error)
}

func (c *uaLocal) PostReview(ctx context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	c.mu.Lock()
	c.reviews = append(c.reviews, req)
	c.mu.Unlock()
	return c.answer(ctx)
}

func (c *uaLocal) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reviews)
}

func uaReview(allowed bool) func(context.Context) (engine.ReviewResponse, error) {
	return func(context.Context) (engine.ReviewResponse, error) {
		return engine.ReviewResponse{Status: http.StatusCreated, Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview",
			"status": map[string]any{"allowed": allowed},
		}}}, nil
	}
}

func uaReviewStatus(status int) func(context.Context) (engine.ReviewResponse, error) {
	return func(context.Context) (engine.ReviewResponse, error) {
		return engine.ReviewResponse{Status: status, Mapped: clierr.FromResponse("", http.MethodPost, status, nil, nil)}, nil
	}
}

func uaCollection(ids ...string) string {
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "metadata": map[string]any{"name": id}})
	}
	body := map[string]any{"type": "collection", "revision": "1", "data": data}
	if len(ids) > 0 {
		body["count"] = len(ids)
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func uaRemedy() string { return uaRemedyText + clierr.Rancher + " get globalroles" }

// TestUserAttributesGet asserts a by-id read tells an object, no object and
// a refusal apart, and asks for steve's schema only after a 404.
func TestUserAttributesGet(t *testing.T) {
	t.Parallel()
	refusedByID := func(status string, clause string) string {
		return "you cannot read userattributes (GET " + uaByIDPath + " answered " + status + ")" + clause + uaRemedy()
	}
	noType := "; Rancher's API shows you no userattributes type"
	tests := []struct {
		name       string
		answers    map[string]uaAnswer
		found      bool
		refusal    string
		code       string
		retryable  bool
		errContain string
		paths      []string
	}{
		{name: "object", answers: map[string]uaAnswer{uaByIDPath: {200, uaObjectBody}}, found: true, paths: []string{uaByIDPath}},
		{
			name:    "missing object, schema served",
			answers: map[string]uaAnswer{uaByIDPath: {404, uaMissingBody}, uaSchemaPath: {200, `{"id":"management.cattle.io.userattribute"}`}},
			paths:   []string{uaByIDPath, uaSchemaPath},
		},
		{
			name:    "empty 404, schema served",
			answers: map[string]uaAnswer{uaByIDPath: {404, ""}, uaSchemaPath: {200, `{"id":"management.cattle.io.userattribute"}`}},
			paths:   []string{uaByIDPath, uaSchemaPath},
		},
		{
			name:    "empty 404, no schema",
			answers: map[string]uaAnswer{uaByIDPath: {404, ""}, uaSchemaPath: {404, ""}},
			refusal: refusedByID("404", noType), code: clierr.CodeRBAC,
			paths: []string{uaByIDPath, uaSchemaPath},
		},
		{
			name:    "missing object body, no schema",
			answers: map[string]uaAnswer{uaByIDPath: {404, uaMissingBody}, uaSchemaPath: {404, ""}},
			refusal: refusedByID("404", noType), code: clierr.CodeRBAC,
			paths: []string{uaByIDPath, uaSchemaPath},
		},
		{
			name:    "403 is refused without a schema read",
			answers: map[string]uaAnswer{uaByIDPath: {403, ""}},
			refusal: refusedByID("403", ""), code: clierr.CodeRBAC,
			paths: []string{uaByIDPath},
		},
		{name: "401 passes through", answers: map[string]uaAnswer{uaByIDPath: {401, ""}}, code: clierr.CodeAuth, paths: []string{uaByIDPath}},
		{name: "500 passes through", answers: map[string]uaAnswer{uaByIDPath: {500, ""}}, code: clierr.CodeGeneric, retryable: true, paths: []string{uaByIDPath}},
		{
			name:    "schema 500 after a 404 is an error, never no object",
			answers: map[string]uaAnswer{uaByIDPath: {404, ""}, uaSchemaPath: {500, ""}},
			code:    clierr.CodeGeneric, retryable: true, paths: []string{uaByIDPath, uaSchemaPath},
		},
		{
			name:    "malformed body",
			answers: map[string]uaAnswer{uaByIDPath: {200, `[]`}},
			code:    clierr.CodeGeneric, errContain: "unexpected response from " + uaByIDPath, paths: []string{uaByIDPath},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &uaSteve{answers: tc.answers}
			attrs := NewUserAttributes(srv.get, &uaLocal{})
			obj, found, err := attrs.Get(t.Context(), "u-abc")
			assert.Equal(t, tc.paths, srv.requested())
			assert.Equal(t, tc.found, found)
			if tc.code == "" {
				require.NoError(t, err)
				assert.Equal(t, tc.found, obj != nil)
				return
			}
			require.Error(t, err)
			assert.Nil(t, obj)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Equal(t, tc.refusal != "", IsUserAttributesRefusal(err))
			if tc.refusal != "" {
				assert.Equal(t, tc.refusal, ce.Message)
			}
			if tc.errContain != "" {
				require.ErrorContains(t, err, tc.errContain)
			}
		})
	}
}

// TestUserAttributesList asserts a collection read is complete only when
// the authorizer says the caller may list, and never asks before a 200.
func TestUserAttributesList(t *testing.T) {
	t.Parallel()
	check := "a SelfSubjectAccessReview for list on userattributes.management.cattle.io answered "
	m3 := func(answer string) string {
		return "you cannot list every userattribute (" + check + answer + "); GET " + uaCollectPath + " may leave items out" + uaRemedy()
	}
	m2 := func(status, clause string) string {
		return "you cannot list userattributes (GET " + uaCollectPath + " answered " + status + ")" + clause + uaRemedy()
	}
	noType := "; Rancher's API shows you no userattributes type"
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name    string
		answers map[string]uaAnswer
		review  func(context.Context) (engine.ReviewResponse, error)
		ctx     context.Context
		ids     []string
		message string
		code    string
		refusal bool
		reviews int
	}{
		{name: "allowed", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a", "u-b")}}, review: uaReview(true), ids: []string{"u-a", "u-b"}, reviews: 1},
		{name: "allowed, none", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection()}}, review: uaReview(true), ids: []string{}, reviews: 1},
		{
			name: "no list right, no items", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection()}}, review: uaReview(false),
			message: m3("denied"), code: clierr.CodeRBAC, refusal: true, reviews: 1,
		},
		{
			name: "no list right, review error", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}}, review: uaReviewStatus(500),
			message: m3(`an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`), code: clierr.CodeRBAC, refusal: true, reviews: 1,
		},
		{
			name: "no list right, review without an answer", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}},
			review: func(context.Context) (engine.ReviewResponse, error) {
				return engine.ReviewResponse{}, errors.New("connection reset")
			},
			message: m3("connection reset"), code: clierr.CodeRBAC, refusal: true, reviews: 1,
		},
		{
			name: "review 401 passes through", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}}, review: uaReviewStatus(401),
			code: clierr.CodeAuth, reviews: 1,
		},
		{
			name: "cancelled context", answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}}, ctx: cancelled,
			review: func(ctx context.Context) (engine.ReviewResponse, error) { return engine.ReviewResponse{}, ctx.Err() },
			code:   clierr.CodeGeneric, reviews: 0,
		},
		{name: "404", answers: map[string]uaAnswer{uaCollectPath: {404, ""}}, review: uaReview(true), message: m2("404", noType), code: clierr.CodeRBAC, refusal: true},
		{name: "403", answers: map[string]uaAnswer{uaCollectPath: {403, ""}}, review: uaReview(true), message: m2("403", ""), code: clierr.CodeRBAC, refusal: true},
		{name: "401 passes through", answers: map[string]uaAnswer{uaCollectPath: {401, ""}}, review: uaReview(true), code: clierr.CodeAuth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &uaSteve{answers: tc.answers}
			local := &uaLocal{answer: tc.review}
			ctx := tc.ctx
			if ctx == nil {
				ctx = t.Context()
			}
			rows, err := NewUserAttributes(srv.get, local).List(ctx)
			assert.Equal(t, []string{uaCollectPath}, srv.requested()[:1], "the collection path")
			assert.Equal(t, tc.reviews, local.count())
			if tc.code == "" {
				require.NoError(t, err)
				ids := []string{}
				for _, r := range rows {
					ids = append(ids, r.GetName())
				}
				assert.Equal(t, tc.ids, ids)
				return
			}
			require.Error(t, err)
			assert.Nil(t, rows, "a refused read never returns rows")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.refusal, IsUserAttributesRefusal(err))
			if tc.message != "" {
				assert.Equal(t, tc.message, ce.Message)
			}
		})
	}
}

// TestUserAttributesListPaged asserts every page is read before the list
// check.
func TestUserAttributesListPaged(t *testing.T) {
	t.Parallel()
	first := `{"type":"collection","revision":"1","count":2,"continue":"1","data":[{"id":"u-a","metadata":{"name":"u-a"}}]}`
	second := `{"type":"collection","revision":"1","count":2,"data":[{"id":"u-b","metadata":{"name":"u-b"}}]}`
	srv := &uaSteve{answers: map[string]uaAnswer{
		uaCollectPath:                 {200, first},
		uaCollectPath + "?continue=1": {200, second},
	}}
	local := &uaLocal{answer: uaReview(true)}
	rows, err := NewUserAttributes(srv.get, local).List(t.Context())
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	assert.Equal(t, []string{uaCollectPath, uaCollectPath + "?continue=1"}, srv.requested())
	assert.Equal(t, 1, local.count())
}

// TestUserAttributesListCheckRequest asserts the review the reader posts: a
// SelfSubjectAccessReview for list on userattributes with no name and no
// namespace.
func TestUserAttributesListCheckRequest(t *testing.T) {
	t.Parallel()
	srv := &uaSteve{answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}}}
	local := &uaLocal{answer: uaReview(true)}
	_, err := NewUserAttributes(srv.get, local).List(t.Context())
	require.NoError(t, err)
	require.Len(t, local.reviews, 1)
	req := local.reviews[0]
	assert.Equal(t, engine.ReviewSelfSubjectAccess, req.Kind)
	gvk, _ := engine.ReviewGVK(engine.ReviewSelfSubjectAccess)
	assert.Equal(t, gvk, req.Object.GroupVersionKind())
	assert.Equal(t, map[string]any{"resourceAttributes": map[string]any{
		"verb": "list", "group": "management.cattle.io", "resource": "userattributes",
	}}, req.Object.Object["spec"])
}

// TestUserAttributesListCheckNoAnswerIsNotStored asserts a list check that
// got no answer is the refusal for that read only, and a list check that
// ends under a cancelled context is the context's error (whatever the review
// reported), not the refusal and not stored: the next read under a live
// context asks again.
func TestUserAttributesListCheckNoAnswerIsNotStored(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"no response", "cancelled context"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := &uaSteve{answers: map[string]uaAnswer{uaCollectPath: {200, uaCollection("u-a")}}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failing := true
			local := &uaLocal{answer: func(context.Context) (engine.ReviewResponse, error) {
				if !failing {
					return uaReview(true)(ctx)
				}
				if name == "cancelled context" {
					cancel()
				}
				return engine.ReviewResponse{}, errors.New("connection reset")
			}}
			attrs := NewUserAttributes(srv.get, local)
			_, err := attrs.List(ctx)
			require.Error(t, err)
			assert.Equal(t, name == "no response", IsUserAttributesRefusal(err))
			assert.Equal(t, name == "cancelled context", errors.Is(err, context.Canceled))
			failing = false
			rows, err := attrs.List(t.Context())
			require.NoError(t, err)
			assert.Len(t, rows, 1)
			assert.Equal(t, 2, local.count(), "the second read sends its own review")
		})
	}
}

// TestUserAttributesMemos asserts the schema
// answer and the list check answer are each computed once per reader, and a
// read cut short by a cancelled context stores nothing.
func TestUserAttributesMemos(t *testing.T) {
	t.Parallel()
	t.Run("one schema request and one review for two reads", func(t *testing.T) {
		t.Parallel()
		srv := &uaSteve{answers: map[string]uaAnswer{
			uaByIDPath: {404, ""}, uaSchemaPath: {200, `{}`}, uaCollectPath: {200, uaCollection("u-a")},
		}}
		local := &uaLocal{answer: uaReview(true)}
		attrs := NewUserAttributes(srv.get, local)
		for range 2 {
			_, found, err := attrs.Get(t.Context(), "u-abc")
			require.NoError(t, err)
			assert.False(t, found)
			_, err = attrs.List(t.Context())
			require.NoError(t, err)
		}
		schemaRequests := 0
		for _, p := range srv.requested() {
			if p == uaSchemaPath {
				schemaRequests++
			}
		}
		assert.Equal(t, 1, schemaRequests)
		assert.Equal(t, 1, local.count())
	})
	t.Run("a cancelled first read stores nothing", func(t *testing.T) {
		t.Parallel()
		srv := &uaSteve{answers: map[string]uaAnswer{uaByIDPath: {404, ""}, uaSchemaPath: {200, `{}`}, uaCollectPath: {200, uaCollection("u-a")}}}
		cancelledReview := true
		local := &uaLocal{answer: func(ctx context.Context) (engine.ReviewResponse, error) {
			if cancelledReview {
				return engine.ReviewResponse{}, context.Canceled
			}
			return uaReview(true)(ctx)
		}}
		attrs := NewUserAttributes(srv.get, local)
		_, err := attrs.List(t.Context())
		require.Error(t, err)
		assert.False(t, IsUserAttributesRefusal(err), "a context error is never the M3 refusal")
		cancelledReview = false
		rows, err := attrs.List(t.Context())
		require.NoError(t, err)
		assert.Len(t, rows, 1)
		assert.Equal(t, 2, local.count(), "the second read sends its own review")

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err = attrs.Get(ctx, "u-abc")
		require.Error(t, err)
		_, found, err := attrs.Get(t.Context(), "u-abc")
		require.NoError(t, err)
		assert.False(t, found)
	})
}
