package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type flagInput map[string]string

func (f flagInput) String(name string) string { return f[name] }
func (flagInput) Bool(string) bool            { return false }
func (flagInput) Strings(string) []string     { return nil }
func (flagInput) IsSet(string) bool           { return false }
func (flagInput) Args() []string              { return nil }

// TestResolveDownstreamClusterMissingFlagFitsTheCommand asserts the empty
// --cluster refusal is a VALIDATION error with the cluster-listing command
// as the paste-valid hint. A command that takes --namespace is told that
// Rancher's own namespaces live on the local cluster; any other command
// gets the plain text, which names no namespace. Routing stays explicit;
// the error only shortens the round trip to the right value.
func TestResolveDownstreamClusterMissingFlagFitsTheCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		namespaced bool
		want       string
	}{
		{"namespaced", true, ClusterRequiredMessage},
		{"plain", false, ClusterRequiredPlainMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveDownstreamCluster(t.Context(), nil, flagInput{}, tc.namespaced)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, tc.want, ce.Message)
			assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
		})
	}
	assert.Contains(t, ClusterRequiredMessage, "--cluster local")
	assert.Contains(t, ClusterRequiredMessage, "cattle-tokens")
	assert.NotContains(t, ClusterRequiredPlainMessage, "cattle")
}

// TestClusterFlagCarriesTheTextThatFits asserts the declaration sets the
// message and hint whether or not the flag is required.
func TestClusterFlagCarriesTheTextThatFits(t *testing.T) {
	t.Parallel()
	for _, required := range []bool{true, false} {
		f := ClusterFlag(required, true)
		assert.Equal(t, required, f.Required)
		assert.Equal(t, FlagCluster, f.Name)
		assert.Equal(t, ClusterRequiredMessage, f.MissingMessage)
		assert.Equal(t, ClusterRequiredHint, f.MissingHint)

		f = ClusterFlag(required, false)
		assert.Equal(t, ClusterRequiredPlainMessage, f.MissingMessage)
		assert.Equal(t, ClusterRequiredHint, f.MissingHint)
	}
}

// TestResolveDownstreamClusterMissingFlagWithProjectIsTheLookupsRefusal
// asserts that with --project typed, an empty --cluster is answered by the
// project lookup's refusal, which names the clusters the project is on,
// in place of the fixed local-cluster advice.
func TestResolveDownstreamClusterMissingFlagWithProjectIsTheLookupsRefusal(t *testing.T) {
	t.Parallel()
	refusal := clierr.New(clierr.CodeValidation, "--cluster is required with --project; candidates")
	ctx := runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) { return runtime.Project{}, refusal })
	_, err := ResolveDownstreamCluster(ctx, nil, flagInput{}, true)
	require.ErrorIs(t, err, refusal)
}

// TestClusterRequiredErrorIsValidationWithTheClusterListingHint asserts the
// builder gives the message it is passed, as VALIDATION, with the hint that
// lists the clusters; and that each message of a command with its own wording
// is the text its refusal shows.
func TestClusterRequiredErrorIsValidationWithTheClusterListingHint(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		ClusterRequiredMessage,
		ClusterRequiredPlainMessage,
		ClusterOrAllClustersRequiredMessage,
		DownstreamClusterRequiredMessage("agent"),
		NamedClusterRequiredMessage("name", "clusterroletemplatebinding"),
		"--cluster is required; a text of the caller's",
	} {
		ce := ClusterRequiredError(msg)
		require.NotNil(t, ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, msg, ce.Message)
		assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
	}

	assert.Equal(t, "--cluster or --all-clusters is required; pass --cluster <id|name> for one cluster, or --all-clusters for every cluster you can list", ClusterOrAllClustersRequiredMessage)
	assert.Equal(t, "--cluster is required; agent runs only on a downstream cluster, pass a downstream cluster id or name", DownstreamClusterRequiredMessage("agent"))
	assert.Equal(t, "--cluster is required when you give the id of one project", NamedClusterRequiredMessage("id", "project"))
}
