package projects

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCreateSecretBareFormObjectShape(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	eff, exec, err := planMember(t, cmd, memberIn{
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

	_, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "type": "kubernetes.io/tls"},
		a: []string{"tls-secret"},
	})
	require.NoError(t, err)
	typ, _, _ := unstructured.NestedString(client.created.Object, "type")
	assert.Equal(t, "kubernetes.io/tls", typ)
}

func TestCreateSecretBareWithoutNamespaceIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{s: map[string]string{"cluster": "c-x"}, a: []string{"app-secret"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestCreateSecretProjectScopedWithBackingNamespaceSet(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "p-backing-ns")},
	}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, exec, err := planMember(t, cmd, memberIn{
		s: map[string]string{"project": "p-1", "cluster": "c-x", "from-literal": "token=abc"},
		b: map[string]bool{"project-scoped": true},
		a: []string{"shared-cred"},
	})
	require.NoError(t, err)
	assert.Equal(t, "p-backing-ns", client.createdNS)
	label, _, _ := unstructured.NestedString(client.created.Object, "metadata", "labels", projectScopedSecretLabel)
	assert.Equal(t, "p-1", label)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Empty(t, res.Warnings, "a set backingNamespace needs no propagation warning")
}

func TestCreateSecretProjectScopedEmptyBackingNamespaceWarns(t *testing.T) {
	t.Parallel()
	client := &memberClient{
		projects: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{project("p-1", "web")}},
		gets:     map[string]*unstructured.Unstructured{"c-x/p-1": projectWithBackingNS("p-1", "")},
	}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, exec, err := planMember(t, cmd, memberIn{
		s: map[string]string{"project": "p-1", "cluster": "c-x"},
		b: map[string]bool{"project-scoped": true},
		a: []string{"shared-cred"},
	})
	require.NoError(t, err)
	assert.Equal(t, "p-1", client.createdNS, "empty status.backingNamespace falls back to the project id")

	res, err := exec(context.Background())
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "2.12")
}

func TestCreateSecretProjectScopedWithoutProjectIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"cluster": "c-x"},
		b: map[string]bool{"project-scoped": true},
		a: []string{"shared-cred"},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestCreateSecretProjectScopedWithNamespaceIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"project": "p-1", "namespace": "team-a", "cluster": "c-x"},
		b: map[string]bool{"project-scoped": true},
		a: []string{"shared-cred"},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestCreateSecretMalformedLiteralIsValidation(t *testing.T) {
	t.Parallel()
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	_, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "notapair"},
		a: []string{"app-secret"},
	})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestCreateSecretDryRunEffectNeverLeaksLiteralValue pins DR-001 discipline:
// engine.PlanCreate builds Target-only Effects (no diff, no object payload),
// so a dry-run of a create secret leaks nothing — this test proves it by
// rendering the Effect to JSON and checking the literal never appears.
func TestCreateSecretDryRunEffectNeverLeaksLiteralValue(t *testing.T) {
	t.Parallel()
	const literal = "sekret-value-xyz"
	client := &memberClient{}
	cmd := NewCreateSecret(stubFactory{c: client})

	eff, _, err := planMember(t, cmd, memberIn{
		s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "password=" + literal},
		a: []string{"app-secret"},
	})
	require.NoError(t, err)

	rendered, err := json.Marshal(eff)
	require.NoError(t, err)
	assert.NotContains(t, string(rendered), literal)
}
