package resources

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefusePatchKeepsTheFlagsAndSaysItIsRefused asserts that the refusal
// command has the generic patch's name, aliases and flags, and a usage line
// and description that say the verb is always refused.
func TestRefusePatchKeepsTheFlagsAndSaysItIsRefused(t *testing.T) {
	t.Parallel()
	k := catalogRow(t, "app")
	want := NewPatch(k, nil, nil).Describe()
	got := NewRefusePatch(k).Describe()
	assert.Equal(t, want.Name, got.Name)
	assert.Equal(t, want.Aliases, got.Aliases)
	assert.Equal(t, want.Flags, got.Flags)
	assert.Equal(t, "Refused: app cannot be patched", got.Usage)
	assert.Equal(t, "Always refused before any request: "+k.NoPatchReason+".", got.Desc)
}

// TestRefusePatchRefusesBeforeAnyRequestAndNamesTheRead asserts that Plan
// returns a VALIDATION error with the row's reason and a read command that uses
// the cluster and namespace the user typed, shell-quoted, and has <cluster> as
// a placeholder when none was typed. For a row whose namespace is a Fleet
// workspace, the read command includes the typed namespace and has no
// --namespace when none was typed. It uses no client.
func TestRefusePatchRefusesBeforeAnyRequestAndNamesTheRead(t *testing.T) {
	t.Parallel()
	app := catalogRow(t, "app")
	repo := catalogRow(t, "clusterrepo")
	snapshot := catalogRow(t, "etcdsnapshot")
	for _, tc := range []struct {
		name string
		cmd  runtime.Command
		in   input
		want string
	}{
		{"typed cluster and namespace", NewRefusePatch(app), input{s: map[string]string{"cluster": "prod", "namespace": "it's"}},
			"app: patch is not supported - " + app.NoPatchReason + "; read it with " + clierr.Rancher + " get 'app' --cluster 'prod' --namespace 'it'\\''s'"},
		{"no cluster", NewRefusePatch(app), input{},
			"app: patch is not supported - " + app.NoPatchReason + "; read it with " + clierr.Rancher + " get 'app' --cluster <cluster>"},
		{"cluster-scoped row", NewRefusePatch(repo), input{s: map[string]string{"cluster": "c-1", "namespace": "ignored"}},
			"clusterrepo: patch is not supported - " + repo.NoPatchReason + "; read it with " + clierr.Rancher + " get 'clusterrepo' --cluster 'c-1'"},
		{"Fleet workspace row, typed namespace", NewRefusePatch(snapshot), input{s: map[string]string{"namespace": "c-m-1"}},
			"etcd-snapshot: patch is not supported - " + snapshot.NoPatchReason + "; read it with " + clierr.Rancher + " get 'etcd-snapshot' --namespace 'c-m-1'"},
		{"Fleet workspace row, no namespace typed", NewRefusePatch(snapshot), input{},
			"etcd-snapshot: patch is not supported - " + snapshot.NoPatchReason + "; read it with " + clierr.Rancher + " get 'etcd-snapshot'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc, ok := tc.cmd.(runtime.MutatingCommand)
			require.True(t, ok)
			_, exec, err := mc.Plan(t.Context(), tc.in)
			require.Nil(t, exec)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
		})
	}
}
