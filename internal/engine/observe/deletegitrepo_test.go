package observe

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func mustGitRepoKind(t *testing.T) kinds.Kind {
	t.Helper()
	for _, k := range kinds.All() {
		if k.Key == "gitrepo" {
			return k
		}
	}
	t.Fatal("gitrepo kind row missing")
	return kinds.Kind{}
}

// TestDeleteGitRepoCarriesCascadeWarning pins the S-10 exercise fix: a
// gitrepo delete's Effect carries the cascade statement in Warnings — the
// same advisory treatment delete provisioning-cluster gives its blast
// radius — while the generic sequence (Destructive, RV precondition)
// stays untouched.
func TestDeleteGitRepoCarriesCascadeWarning(t *testing.T) {
	t.Parallel()
	k := mustGitRepoKind(t)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "fleet.cattle.io/v1alpha1",
		"kind":       "GitRepo",
		"metadata": map[string]any{
			"name":            "repo-1",
			"namespace":       "fleet-default",
			"resourceVersion": "7",
		},
	}}
	client := newFakeClient().withObject(k.GVR, "fleet-default", "repo-1", obj)
	factory := &fakeFactory{local: client}

	cmd := NewDeleteGitRepo(k, factory)
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)

	eff, exec, err := mc.Plan(context.Background(), input{args: []string{"repo-1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.True(t, eff.Destructive)
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.Equal(t, []string{gitRepoCascadeWarning}, eff.Warnings)
}
