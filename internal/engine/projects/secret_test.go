package projects

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCreateSecretBareFormObjectShape(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	eff, exec, err := planMember(t, t.Context(), cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "k=v1,other=v2"},
		a: []string{"app-secret"},
	})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionCreate, eff.Action)

	assert.Equal(t, "team-a", client.createdNS)
	assert.Equal(t, "v1", client.created.GetAPIVersion())
	assert.Equal(t, "Secret", client.created.GetKind())
	assert.Equal(t, "app-secret", client.created.GetName())
	typ, _, _ := unstructured.NestedString(client.created.Object, "type")
	assert.Equal(t, "Opaque", typ, "default type when --type is not given")
	sd, _, _ := unstructured.NestedStringMap(client.created.Object, "stringData")
	assert.Equal(t, map[string]string{"k": "v1", "other": "v2"}, sd)
}

func TestCreateSecretBareFormCustomType(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, t.Context(), cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "type": "kubernetes.io/tls"},
		a: []string{"tls-secret"},
	})
	require.NoError(t, err)
	typ, _, _ := unstructured.NestedString(client.created.Object, "type")
	assert.Equal(t, "kubernetes.io/tls", typ)
}

// webProjectCtx carries project p-1 (display name web) on cluster c-x, as
// the pipeline resolves a typed --project web.
func webProjectCtx(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-x", ID: "p-1", DisplayName: "web"})
}

// noProjectCtx carries no project, as for an invocation with no --project.
func noProjectCtx(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}

// noProjectReadCtx fails the test when the command reads the project.
func noProjectReadCtx(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
		t.Error("the project was read")
		return runtime.Project{}, errors.New("unexpected project read")
	})
}

// noRequestFactory fails the test on any cluster resolution or client
// request.
type noRequestFactory struct{ t *testing.T }

func (f noRequestFactory) Cluster(context.Context, string) (engine.ClusterClient, error) {
	f.t.Error("a cluster client was requested")
	return nil, errors.New("unexpected request")
}

func (f noRequestFactory) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	f.t.Error("an impersonated cluster client was requested")
	return nil, errors.New("unexpected request")
}

func (f noRequestFactory) ResolveCluster(context.Context, string) (string, error) {
	f.t.Error("the cluster was resolved")
	return "", errors.New("unexpected request")
}

// TestCreateSecretWithoutNamespaceOrProjectIsValidation asserts that with
// neither --namespace nor --project the refusal names both flags, points
// at the caller's projects on the typed cluster, and comes before any
// request.
func TestCreateSecretWithoutNamespaceOrProjectIsValidation(t *testing.T) {
	t.Parallel()
	cmd := NewCreateSecret(noRequestFactory{t: t})

	_, _, err := planMember(t, t.Context(), cmd, memberIn{s: map[string]string{"cluster": "c-x"}, a: []string{"app-secret"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "pass --namespace <namespace> to create a secret in a namespace, or --project <project> to create a project-scoped secret", ce.Message)
	assert.Equal(t, clierr.Rancher+" access projects --cluster 'c-x'", ce.Hint)
}

// TestCreateSecretProjectWithNamespaceIsValidationBeforeReadingTheProject
// asserts --project with --namespace is refused before the project is read
// and before any request.
func TestCreateSecretProjectWithNamespaceIsValidationBeforeReadingTheProject(t *testing.T) {
	t.Parallel()
	cmd := NewCreateSecret(noRequestFactory{t: t})

	_, _, err := planMember(t, noProjectReadCtx(t), cmd, memberIn{
		s: map[string]string{"project": "web", "namespace": "team-a", "cluster": "c-x"},
		a: []string{"shared-cred"},
	})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--project and --namespace cannot be combined: --project creates a project-scoped secret in the project's backing namespace, --namespace creates a secret in that namespace", ce.Message)
}

func TestCreateSecretProjectScopedWithBackingNamespaceSet(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-backing-ns")},
	}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, exec, err := planMember(t, webProjectCtx(t), cmd, memberIn{
		s: map[string]string{"project": "web", "cluster": "c-x", "from-literal": "token=abc"},
		a: []string{"shared-cred"},
	})
	require.NoError(t, err)
	assert.Equal(t, "p-backing-ns", client.createdNS)
	label, _, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectScopedSecretLabel)
	assert.Equal(t, "p-1", label)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Empty(t, res.Warnings, "a set backingNamespace needs no propagation warning")
}

func TestCreateSecretProjectScopedEmptyBackingNamespaceWarns(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, exec, err := planMember(t, webProjectCtx(t), cmd, memberIn{
		s: map[string]string{"project": "web", "cluster": "c-x"},
		a: []string{"shared-cred"},
	})
	require.NoError(t, err)
	assert.Equal(t, "p-1", client.createdNS, "empty status.backingNamespace falls back to the project id")

	res, err := exec(t.Context())
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "the project has no status.backingNamespace yet; Rancher might not copy this project-scoped secret into the project's namespaces (copying needs Rancher 2.14 or later, the oldest version this CLI supports)", res.Warnings[0])
}

func TestCreateSecretMalformedLiteralIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, t.Context(), cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "notapair"},
		a: []string{"app-secret"},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestCreateSecretDryRunEffectNeverLeaksLiteralValue asserts that the diff of
// a create secret preview lists the literal's path with the value hidden:
// rendering the Effect to JSON, the literal never appears and the path does.
func TestCreateSecretDryRunEffectNeverLeaksLiteralValue(t *testing.T) {
	t.Parallel()
	const literal = "sekret-value-xyz"
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	eff, _, err := planMember(t, t.Context(), cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "password=" + literal},
		a: []string{"app-secret"},
	})
	require.NoError(t, err)

	rendered, err := json.Marshal(eff)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), literal)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/namespace", To: "team-a"},
		{Path: "/stringData/password", To: "<redacted>"},
		{Path: "/type", To: "Opaque"},
	}, eff.Diff)
}

// TestCreateSecretIsDestructive asserts the confirmation gate for both routing
// modes: secret is a Sensitive kind, so create secret must need --yes
// exactly like the generic create path (resources/create.go).
func TestCreateSecretIsDestructive(t *testing.T) {
	t.Parallel()

	t.Run("bare", func(t *testing.T) {
		t.Parallel()
		client := &memberClient{}
		cmd := NewCreateSecret(stubFactory{c: client})
		eff, _, err := planMember(t, t.Context(), cmd, memberIn{s: map[string]string{"namespace": "team-a", "cluster": "c-x"}, a: []string{"app-secret"}})
		require.NoError(t, err)
		assert.True(t, eff.Destructive)
	})

	t.Run("project-scoped", func(t *testing.T) {
		t.Parallel()
		client := &memberClient{
			gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-backing-ns")},
		}
		cmd := NewCreateSecret(stubFactory{c: client})
		eff, _, err := planMember(t, webProjectCtx(t), cmd, memberIn{
			s: map[string]string{"project": "web", "cluster": "c-x"},
			a: []string{"shared-cred"},
		})
		require.NoError(t, err)
		assert.True(t, eff.Destructive)
	})
}

// TestCreateSecretProjectScopedExplicitBackingNamespaceEqualToProjectIDDoesNotWarn
// distinguishes the precise statusEmpty signal from the old ns==projectID
// heuristic: status.backingNamespace is explicitly SET, coincidentally to
// the same value as the project id - the old heuristic would misfire a
// warning here since it can't tell that apart from an empty status field.
func TestCreateSecretProjectScopedExplicitBackingNamespaceEqualToProjectIDDoesNotWarn(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-1")},
	}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, exec, err := planMember(t, webProjectCtx(t), cmd, memberIn{
		s: map[string]string{"project": "web", "cluster": "c-x"},
		a: []string{"shared-cred"},
	})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Empty(t, res.Warnings, "status.backingNamespace was explicitly set (even though it equals the project id) - no propagation warning")
}

func TestCreateSecretResultRedactsPayload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		client *memberClient
		ctx    func(t *testing.T) context.Context
		in     memberIn
	}{
		{
			name:   "bare namespace form",
			client: &memberClient{},
			ctx:    noProjectCtx,
			in: memberIn{
				s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "token=abc"},
				a: []string{"app-secret"},
			},
		},
		{
			name: "project-scoped form",
			client: &memberClient{
				gets: map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-backing-ns")},
			},
			ctx: webProjectCtx,
			in: memberIn{
				s: map[string]string{"project": "web", "cluster": "c-x", "from-literal": "token=abc"},
				a: []string{"shared-cred"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := NewCreateSecret(stubFactory{c: tt.client})
			_, exec, err := planMember(t, tt.ctx(t), cmd, tt.in)
			require.NoError(t, err)

			res, err := exec(t.Context())
			require.NoError(t, err)
			obj, ok := res.Data.(map[string]any)
			require.True(t, ok, "committed secret must render as an object")
			assert.NotContains(t, obj, "stringData",
				"the server echoes the submitted payload back on create; it must not reach output")
			assert.NotContains(t, obj, "data")
			assert.Equal(t, "Secret", obj["kind"], "redaction must not drop the rest of the object")
		})
	}
}
