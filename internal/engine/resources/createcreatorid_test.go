package resources

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
	projectsSuffix  = "/apis/management.cattle.io/v3/namespaces/c-abc/projects"
)

// selfUserAnswer is how the test server answers the SelfUser request.
type selfUserAnswer int

const (
	answerUserID selfUserAnswer = iota
	answerForbidden
	answerNoUserID
	answerInvalidUserID
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
	// refuseProjectDryRun answers the Project dry-run with a 400 Status.
	refuseProjectDryRun bool
}

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
	if cs.refuseProjectDryRun && strings.HasSuffix(r.URL.Path, projectsSuffix) && strings.Contains(r.URL.RawQuery, "dryRun=All") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"annotation field.cattle.io/creatorId is required","reason":"BadRequest","code":400}`))
		return
	}
	if strings.HasSuffix(r.URL.Path, selfUsersSuffix) {
		switch cs.answer {
		case answerForbidden:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"selfusers.ext.cattle.io is forbidden","reason":"Forbidden","code":403}`))
			return
		case answerUserID, answerInvalidUserID:
			_ = unstructured.SetNestedField(body, cs.userID, "status", "userID")
		}
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(body)
}

func (cs *creatorServer) postsTo(suffix string) []recordedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []recordedRequest
	for _, r := range cs.reqs {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func (cs *creatorServer) postCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.reqs)
}

func projectManifestWith(annotations string) string {
	return "apiVersion: management.cattle.io/v3\nkind: Project\nmetadata:\n  name: p1\n  namespace: c-abc\n" +
		annotations + "spec:\n  displayName: p1\n  clusterName: c-abc\n"
}

// planProjectManifest runs the create resource Plan and, when it succeeds,
// the create closure, against the real client.
func planProjectManifest(t *testing.T, client engine.ClusterClient, manifest string) (safety.Effect, error) {
	t.Helper()
	fac := &recFactory{client: client}
	cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(manifest)})
	eff, exec, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml"}})
	if err != nil {
		return eff, err
	}
	_, err = exec(t.Context())
	require.NoError(t, err)
	return eff, nil
}

// TestCreateResourceProjectManifestCreatorID asserts each manifest case of
// the creator annotation: set when absent or empty, other annotations and
// creator-principal-name kept, a value equal to the caller's kept with one
// SelfUser request, no-creator-rbac left alone with no SelfUser request. Both
// Project bodies (the dry-run and the create) carry the same annotations, and
// the preview lists the annotation when the Project carries it.
func TestCreateResourceProjectManifestCreatorID(t *testing.T) {
	t.Parallel()
	const creatorKey = "field.cattle.io/creatorId"
	tests := []struct {
		name         string
		annotations  string
		want         map[string]any
		wantRequests int
	}{
		{
			name:         "no annotations",
			want:         map[string]any{creatorKey: "u-abc"},
			wantRequests: 1,
		},
		{
			name:         "other annotations stay",
			annotations:  "  annotations:\n    example.com/team: web\n",
			want:         map[string]any{"example.com/team": "web", creatorKey: "u-abc"},
			wantRequests: 1,
		},
		{
			name:         "creatorId equal to the caller is kept",
			annotations:  "  annotations:\n    field.cattle.io/creatorId: u-abc\n",
			want:         map[string]any{creatorKey: "u-abc"},
			wantRequests: 1,
		},
		{
			name:         "empty creatorId is set",
			annotations:  "  annotations:\n    field.cattle.io/creatorId: \"\"\n",
			want:         map[string]any{creatorKey: "u-abc"},
			wantRequests: 1,
		},
		{
			name:         "creator-principal-name stays",
			annotations:  "  annotations:\n    field.cattle.io/creator-principal-name: local://u-abc\n",
			want:         map[string]any{"field.cattle.io/creator-principal-name": "local://u-abc", creatorKey: "u-abc"},
			wantRequests: 1,
		},
		{
			name:        "no-creator-rbac adds nothing",
			annotations: "  annotations:\n    field.cattle.io/no-creator-rbac: \"true\"\n",
			want:        map[string]any{"field.cattle.io/no-creator-rbac": "true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, answerUserID, "u-abc")
			eff, err := planProjectManifest(t, client, projectManifestWith(tt.annotations))
			require.NoError(t, err)
			assert.Empty(t, eff.Warnings)
			assert.Len(t, cs.postsTo(selfUsersSuffix), tt.wantRequests)
			posts := cs.postsTo(projectsSuffix)
			require.Len(t, posts, 2, "the dry-run and the create")
			assert.Contains(t, posts[0].query, "dryRun=All")
			assert.NotContains(t, posts[1].query, "dryRun")
			for _, p := range posts {
				assert.Equal(t, tt.want, p.annotations())
			}
			entry := safety.DiffEntry{Path: "/metadata/annotations/field.cattle.io~1creatorId", To: "u-abc"}
			if _, ok := tt.want[creatorKey]; ok {
				assert.Contains(t, eff.Diff, entry)
			} else {
				assert.NotContains(t, eff.Diff, entry)
			}
		})
	}
}

// TestCreateResourceProjectManifestRefusesAnotherUser asserts a creatorId
// that differs from the caller's is refused with VALIDATION before any
// Project request, saying the CLI sets the creator to the caller.
func TestCreateResourceProjectManifestRefusesAnotherUser(t *testing.T) {
	t.Parallel()
	client, cs := newCreatorClient(t, answerUserID, "u-abc")
	_, err := planProjectManifest(t, client, projectManifestWith("  annotations:\n    field.cattle.io/creatorId: u-other\n"))
	requireValidation(t, err)
	require.ErrorContains(t, err, "the CLI sets the creator of a project to you")
	require.ErrorContains(t, err, "u-other")
	require.ErrorContains(t, err, "u-abc")
	assert.Empty(t, cs.postsTo(projectsSuffix), "no dry-run is sent")
}

// TestCreateResourceProjectManifestLookupFailure asserts what a failed
// SelfUser request, no user ID or an invalid user ID does to a Project
// manifest: with no creatorId it warns and creates with no annotation; with a
// non-empty creatorId it refuses before any Project request.
func TestCreateResourceProjectManifestLookupFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer selfUserAnswer
		userID string
		cause  string
	}{
		{name: "request refused", answer: answerForbidden, cause: "selfusers.ext.cattle.io is forbidden"},
		{name: "no user ID", answer: answerNoUserID, cause: "the SelfUser response has no user ID"},
		{name: "invalid user ID", answer: answerInvalidUserID, userID: "U-ABC", cause: "the SelfUser response has an invalid user ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name+" without creatorId warns", func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, tt.answer, tt.userID)
			eff, err := planProjectManifest(t, client, projectManifestWith(""))
			require.NoError(t, err)
			require.Len(t, eff.Warnings, 1)
			assert.True(t, strings.HasPrefix(eff.Warnings[0], "creatorId annotation not set ("), eff.Warnings[0])
			assert.Contains(t, eff.Warnings[0], tt.cause)
			posts := cs.postsTo(projectsSuffix)
			require.Len(t, posts, 2)
			for _, p := range posts {
				assert.Empty(t, p.annotations())
			}
		})
		t.Run(tt.name+" with creatorId refuses", func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, tt.answer, tt.userID)
			_, err := planProjectManifest(t, client, projectManifestWith("  annotations:\n    field.cattle.io/creatorId: u-abc\n"))
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Empty(t, ce.Hint)
			require.ErrorContains(t, err, tt.cause)
			assert.Empty(t, cs.postsTo(projectsSuffix), "no dry-run is sent")
		})
	}
}

// TestCreateResourceProjectManifestDryRunRefusalKeepsTheWarning asserts the
// warning for a failed SelfUser request rides the error when the server then
// refuses the dry-run, as it does for a manifest that sets
// creator-principal-name and no creatorId.
func TestCreateResourceProjectManifestDryRunRefusalKeepsTheWarning(t *testing.T) {
	t.Parallel()
	client, cs := newCreatorClient(t, answerForbidden, "")
	cs.refuseProjectDryRun = true
	_, err := planProjectManifest(t, client, projectManifestWith("  annotations:\n    field.cattle.io/creator-principal-name: local://u-abc\n"))
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	require.ErrorContains(t, err, "annotation field.cattle.io/creatorId is required")
	require.Len(t, ce.Warnings(), 1)
	assert.True(t, strings.HasPrefix(ce.Warnings()[0], "creatorId annotation not set ("), ce.Warnings()[0])
	assert.Contains(t, ce.Warnings()[0], "selfusers.ext.cattle.io is forbidden")
}

// TestCreateResourceOtherKindsGetNoCreatorID asserts a kind that does not set
// the creator annotation sends no SelfUser request and no creatorId.
func TestCreateResourceOtherKindsGetNoCreatorID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest string
		suffix   string
	}{
		{
			name:     "ConfigMap",
			manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: ns1\ndata:\n  k: v\n",
			suffix:   "/api/v1/namespaces/ns1/configmaps",
		},
		{
			name: "ProjectRoleTemplateBinding",
			manifest: "apiVersion: management.cattle.io/v3\nkind: ProjectRoleTemplateBinding\nmetadata:\n  name: b1\n  namespace: p-abc\n" +
				"projectName: c-abc:p-abc\nroleTemplateName: project-member\nuserName: u-abc\n",
			suffix: "/apis/management.cattle.io/v3/namespaces/p-abc/projectroletemplatebindings",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, cs := newCreatorClient(t, answerUserID, "u-abc")
			fac := &recFactory{client: client}
			cmd := NewCreate(fac, mapFileSource{"m.yaml": []byte(tt.manifest)})
			_, exec, err := planCreate(t, cmd, input{s: map[string]string{"filename": "m.yaml", "cluster": "c-abc"}})
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err)
			assert.Empty(t, cs.postsTo(selfUsersSuffix))
			posts := cs.postsTo(tt.suffix)
			require.Len(t, posts, 2)
			for _, p := range posts {
				assert.NotContains(t, p.annotations(), "field.cattle.io/creatorId")
			}
			assert.Equal(t, 2, cs.postCount(), "only the dry-run and the create")
		})
	}
}
