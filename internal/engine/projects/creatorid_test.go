package projects

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	selfUsersSuffix = "/apis/ext.cattle.io/v1/selfusers"
	projectsSuffix  = "/apis/management.cattle.io/v3/namespaces/c-x/projects"
)

// selfUserAnswer is how the test server answers the SelfUser request.
type selfUserAnswer int

const (
	answerUserID selfUserAnswer = iota
	answerForbidden
	answerNoUserID
	answerInvalidUserID
	answerTransportError
)

type recordedRequest struct {
	method string
	path   string
	query  string
	body   map[string]any
}

func (r recordedRequest) annotations() map[string]any {
	a, _, _ := unstructured.NestedMap(r.body, "metadata", "annotations")
	return a
}

// creatorServer is an httptest Rancher that records every request and
// answers the SelfUser request as the test says; any other POST echoes the
// object back.
type creatorServer struct {
	mu     sync.Mutex
	reqs   []recordedRequest
	answer selfUserAnswer
	userID string
}

// newCreatorClient starts the server and returns the real cluster client
// aimed at it, the way the commands get it for the local cluster.
func newCreatorClient(t *testing.T, answer selfUserAnswer, userID string) (engine.ClusterClient, *creatorServer) {
	t.Helper()
	cs := &creatorServer{answer: answer, userID: userID}
	srv := httptest.NewServer(cs)
	t.Cleanup(srv.Close)
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, engine.LocalCluster)
	require.NoError(t, err)
	rc.Transport = srv.Client().Transport
	client, err := provider.NewClusterClient(rc, engine.LocalCluster)
	require.NoError(t, err)
	return client, cs
}

func (cs *creatorServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	cs.mu.Lock()
	cs.reqs = append(cs.reqs, recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body})
	cs.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, selfUsersSuffix) {
		switch cs.answer {
		case answerForbidden:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"selfusers.ext.cattle.io is forbidden","reason":"Forbidden","code":403}`))
			return
		case answerTransportError:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		case answerUserID, answerInvalidUserID:
			_ = unstructured.SetNestedField(body, cs.userID, "status", "userID")
		}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

func (cs *creatorServer) requests() []recordedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]recordedRequest(nil), cs.reqs...)
}

// selfUserRequests returns the SelfUser POSTs; projectRequests the POSTs to
// the projects collection (the dry-run first, then the create).
func (cs *creatorServer) selfUserRequests() []recordedRequest {
	return cs.filter(selfUsersSuffix)
}

func (cs *creatorServer) projectRequests() []recordedRequest {
	return cs.filter(projectsSuffix)
}

func (cs *creatorServer) filter(suffix string) []recordedRequest {
	var out []recordedRequest
	for _, r := range cs.requests() {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func projectObject(annotations map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "web", "namespace": "c-x"},
		"spec":       map[string]any{"clusterName": "c-x", "displayName": "web"},
	}}
	if annotations != nil {
		_ = unstructured.SetNestedMap(obj.Object, annotations, "metadata", "annotations")
	}
	return obj
}

// TestCallerUserID asserts the lookup returns the user ID for a valid
// answer and its own error for a failed request, no user ID and an invalid
// user ID.
func TestCallerUserID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		answer  selfUserAnswer
		userID  string
		want    string
		wantErr error
		code    string
	}{
		{name: "valid", answer: answerUserID, userID: "u-abc", want: "u-abc"},
		{name: "bootstrap admin", answer: answerUserID, userID: "user-abcde", want: "user-abcde"},
		{name: "no user ID", answer: answerNoUserID, wantErr: errNoUserID},
		{name: "uppercase", answer: answerInvalidUserID, userID: "U-ABC", wantErr: errInvalidUserID},
		{name: "space", answer: answerInvalidUserID, userID: "u abc", wantErr: errInvalidUserID},
		{name: "request refused", answer: answerForbidden, code: clierr.CodeRBAC},
		{name: "transport error", answer: answerTransportError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, tt.answer, tt.userID)
			got, err := callerUserID(t.Context(), client)
			assert.Len(t, cs.selfUserRequests(), 1)
			switch {
			case tt.wantErr != nil:
				require.ErrorIs(t, err, tt.wantErr)
			case tt.code != "":
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, tt.code, ce.Code)
			case tt.want == "":
				require.Error(t, err)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
			if tt.wantErr != nil || tt.code != "" || tt.want == "" {
				assert.Empty(t, got)
			}
		})
	}
}

// TestSetCreatorIDRules asserts the annotation rules for a project object:
// the no-creator-rbac annotation, a creatorId equal to the caller's and one
// naming another user, an empty creatorId, and the annotations that stay.
func TestSetCreatorIDRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		annotations  map[string]any
		wantRequests int
		want         map[string]string
	}{
		{
			name:         "no annotations",
			wantRequests: 1,
			want:         map[string]string{creatorIDAnnotation: "u-abc"},
		},
		{
			name:         "other annotations stay",
			annotations:  map[string]any{"example.com/team": "web"},
			wantRequests: 1,
			want:         map[string]string{"example.com/team": "web", creatorIDAnnotation: "u-abc"},
		},
		{
			name:         "creatorId equal to the caller is kept",
			annotations:  map[string]any{creatorIDAnnotation: "u-abc"},
			wantRequests: 1,
			want:         map[string]string{creatorIDAnnotation: "u-abc"},
		},
		{
			name:         "empty creatorId is set",
			annotations:  map[string]any{creatorIDAnnotation: ""},
			wantRequests: 1,
			want:         map[string]string{creatorIDAnnotation: "u-abc"},
		},
		{
			name:         "creator-principal-name stays",
			annotations:  map[string]any{"field.cattle.io/creator-principal-name": "local://u-abc"},
			wantRequests: 1,
			want: map[string]string{
				"field.cattle.io/creator-principal-name": "local://u-abc",
				creatorIDAnnotation:                      "u-abc",
			},
		},
		{
			name:        "no-creator-rbac adds nothing and sends no request",
			annotations: map[string]any{noCreatorRBACAnnotation: "true"},
			want:        map[string]string{noCreatorRBACAnnotation: "true"},
		},
		{
			name:        "no-creator-rbac as a YAML boolean adds nothing",
			annotations: map[string]any{noCreatorRBACAnnotation: true},
		},
		{
			name:        "no-creator-rbac with creatorId is left for the server",
			annotations: map[string]any{noCreatorRBACAnnotation: "true", creatorIDAnnotation: "u-other"},
			want:        map[string]string{noCreatorRBACAnnotation: "true", creatorIDAnnotation: "u-other"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, answerUserID, "u-abc")
			obj := projectObject(tt.annotations)
			warning, err := SetCreatorID(t.Context(), client, obj)
			require.NoError(t, err)
			assert.Empty(t, warning)
			assert.Len(t, cs.selfUserRequests(), tt.wantRequests)
			if tt.want != nil {
				assert.Equal(t, tt.want, obj.GetAnnotations())
			}
			if _, ok := tt.annotations[noCreatorRBACAnnotation]; ok {
				got, _, _ := unstructured.NestedMap(obj.Object, "metadata", "annotations")
				assert.Equal(t, tt.annotations, got, "the annotations are untouched")
			}
		})
	}
}

// TestSetCreatorIDRefusesAnotherUser asserts a creatorId that differs from
// the caller's is refused with VALIDATION, naming the manifest's value and
// the caller's user ID, with no hint, and leaves the object unchanged.
func TestSetCreatorIDRefusesAnotherUser(t *testing.T) {
	t.Parallel()
	client, cs := newCreatorClient(t, answerUserID, "u-abc")
	obj := projectObject(map[string]any{creatorIDAnnotation: "u-other"})
	_, err := SetCreatorID(t.Context(), client, obj)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Empty(t, ce.Hint)
	assert.Contains(t, ce.Message, "sets the creator of a project to you")
	assert.Contains(t, ce.Message, "u-other")
	assert.Contains(t, ce.Message, "u-abc")
	assert.NotContains(t, ce.Message, ", so")
	assert.Equal(t, map[string]string{creatorIDAnnotation: "u-other"}, obj.GetAnnotations())
	assert.Len(t, cs.selfUserRequests(), 1)
}

// TestSetCreatorIDRendersAnotherUsersIDSafely asserts the manifest's value
// is shown through clierr.IDText, so a control character is quoted.
func TestSetCreatorIDRendersAnotherUsersIDSafely(t *testing.T) {
	t.Parallel()
	client, _ := newCreatorClient(t, answerUserID, "u-abc")
	obj := projectObject(map[string]any{creatorIDAnnotation: "u-x\x1b[31m"})
	_, err := SetCreatorID(t.Context(), client, obj)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.NotContains(t, ce.Message, "\x1b")
}

// TestSetCreatorIDLookupFailure asserts what a failed lookup does: with no
// creatorId the command warns and sets nothing; with a non-empty creatorId
// it refuses before any project request, returning the client's own error
// with one added warning for a failed request, and a GENERIC error carrying
// the cause for no user ID or an invalid user ID.
func TestSetCreatorIDLookupFailure(t *testing.T) {
	t.Parallel()
	const compareFailed = "the manifest sets field.cattle.io/creatorId and the CLI could not get your user ID to compare it; remove the annotation, or log in again and retry"
	tests := []struct {
		name       string
		answer     selfUserAnswer
		userID     string
		cause      string
		clientCode string
	}{
		{name: "request refused", answer: answerForbidden, cause: "selfusers.ext.cattle.io is forbidden", clientCode: clierr.CodeRBAC},
		{name: "no user ID", answer: answerNoUserID, cause: "the SelfUser response has no user ID"},
		{name: "invalid user ID", answer: answerInvalidUserID, userID: "U-ABC", cause: "the SelfUser response has an invalid user ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" without creatorId warns", func(t *testing.T) {
			t.Parallel()
			client, _ := newCreatorClient(t, tt.answer, tt.userID)
			obj := projectObject(nil)
			warning, err := SetCreatorID(t.Context(), client, obj)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(warning, creatorWarningPrefix+" ("), warning)
			assert.Contains(t, warning, tt.cause)
			assert.Contains(t, warning, "Rancher adds no owner binding for you on this project")
			assert.Contains(t, warning, "log in again, or check that you may create selfusers.ext.cattle.io, before you run --apply")
			assert.Contains(t, warning, "a project already created keeps no owner binding")
			assert.NotContains(t, warning, "then retry")
			assert.NotContains(t, warning, ", so")
			assert.Empty(t, obj.GetAnnotations())
		})
		t.Run(tt.name+" with creatorId refuses", func(t *testing.T) {
			t.Parallel()
			client, _ := newCreatorClient(t, tt.answer, tt.userID)
			obj := projectObject(map[string]any{creatorIDAnnotation: "u-abc"})
			warning, err := SetCreatorID(t.Context(), client, obj)
			require.Error(t, err)
			assert.Empty(t, warning)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, map[string]string{creatorIDAnnotation: "u-abc"}, obj.GetAnnotations())
			if tt.clientCode != "" {
				assert.Equal(t, tt.clientCode, ce.Code, "the client's error is returned unchanged")
				assert.Equal(t, []string{compareFailed}, ce.Warnings())
				require.ErrorContains(t, err, tt.cause)
				return
			}
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.Equal(t, tt.cause+": "+compareFailed, ce.Message)
			assert.Empty(t, ce.Warnings())
		})
	}
	t.Run("transport error with creatorId refuses", func(t *testing.T) {
		t.Parallel()
		client, cs := newCreatorClient(t, answerTransportError, "")
		obj := projectObject(map[string]any{creatorIDAnnotation: "u-abc"})
		_, err := SetCreatorID(t.Context(), client, obj)
		require.Error(t, err)
		assert.Empty(t, cs.projectRequests())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Contains(t, ce.Warnings(), compareFailed)
	})
}

// TestCreateProjectSetsTheCreatorID asserts `create project` sends one
// SelfUser request before the first Project request, that the dry-run body
// and the create body both carry the annotation, that the preview lists it,
// and that the name, display name and namespace do not change.
func TestCreateProjectSetsTheCreatorID(t *testing.T) {
	t.Parallel()
	client, cs := newCreatorClient(t, answerUserID, "u-abc")
	eff, exec, err := planProject(t, NewCreate(stubFactory{c: client}), in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)

	reqs := cs.requests()
	require.Len(t, reqs, 3, "one SelfUser request, the dry-run and the create")
	assert.True(t, strings.HasSuffix(reqs[0].path, selfUsersSuffix), "the SelfUser request is first: %s", reqs[0].path)
	assert.True(t, strings.HasSuffix(reqs[1].path, projectsSuffix))
	assert.Contains(t, reqs[1].query, "dryRun=All")
	assert.True(t, strings.HasSuffix(reqs[2].path, projectsSuffix))
	assert.NotContains(t, reqs[2].query, "dryRun")
	for _, r := range reqs[1:] {
		assert.Equal(t, map[string]any{creatorIDAnnotation: "u-abc"}, r.annotations())
		name, _, _ := unstructured.NestedString(r.body, "metadata", "name")
		ns, _, _ := unstructured.NestedString(r.body, "metadata", "namespace")
		display, _, _ := unstructured.NestedString(r.body, "spec", "displayName")
		assert.Equal(t, "web", name)
		assert.Equal(t, "c-x", ns)
		assert.Equal(t, "web", display)
	}
	assert.Contains(t, eff.Diff, safety.DiffEntry{Path: "/metadata/annotations/field.cattle.io~1creatorId", To: "u-abc"})
	assert.Empty(t, eff.Warnings)
}

// TestCreateProjectLookupFailureWarnsAndCreates asserts a failed SelfUser
// request, no user ID, an invalid user ID or a transport error adds one
// warning to the Effect, leaves the annotation off both Project bodies and
// still creates the project.
func TestCreateProjectLookupFailureWarnsAndCreates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer selfUserAnswer
		userID string
		cause  string
	}{
		{name: "request refused", answer: answerForbidden, cause: "selfusers.ext.cattle.io is forbidden"},
		{name: "no user ID", answer: answerNoUserID, cause: "the SelfUser response has no user ID"},
		{name: "uppercase user ID", answer: answerInvalidUserID, userID: "U-ABC", cause: "the SelfUser response has an invalid user ID"},
		{name: "user ID with a space", answer: answerInvalidUserID, userID: "u abc", cause: "the SelfUser response has an invalid user ID"},
		{name: "transport error", answer: answerTransportError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, tt.answer, tt.userID)
			eff, exec, err := planProject(t, NewCreate(stubFactory{c: client}), in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err, "the create goes on")

			require.Len(t, eff.Warnings, 1)
			assert.True(t, strings.HasPrefix(eff.Warnings[0], creatorWarningPrefix+" ("), eff.Warnings[0])
			assert.Contains(t, eff.Warnings[0], tt.cause)
			projectReqs := cs.projectRequests()
			require.Len(t, projectReqs, 2, "the dry-run and the create")
			for _, r := range projectReqs {
				assert.Empty(t, r.annotations())
				name, _, _ := unstructured.NestedString(r.body, "metadata", "name")
				assert.Equal(t, "web", name)
			}
			for _, d := range eff.Diff {
				assert.NotContains(t, d.Path, "creatorId")
			}
		})
	}
}
