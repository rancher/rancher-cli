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

// TestResolveDownstreamClusterMissingFlagNamesLocalAndHintsClusterList
// asserts the empty --cluster refusal: a VALIDATION error whose message
// tells the reader that Rancher's own namespaces live on the local cluster,
// with the cluster-listing command as the paste-valid hint. Routing stays
// explicit; the error only shortens the round trip to the right value.
func TestResolveDownstreamClusterMissingFlagNamesLocalAndHintsClusterList(t *testing.T) {
	t.Parallel()
	_, err := ResolveDownstreamCluster(t.Context(), nil, flagInput{})
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--cluster local")
	assert.Contains(t, ce.Message, "cattle-tokens")
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}

// TestResolveDownstreamClusterMissingFlagWithProjectIsTheLookupsRefusal
// asserts that with --project typed, an empty --cluster is answered by the
// project lookup's refusal, which names the clusters the project is on,
// in place of the fixed local-cluster advice.
func TestResolveDownstreamClusterMissingFlagWithProjectIsTheLookupsRefusal(t *testing.T) {
	t.Parallel()
	refusal := clierr.New(clierr.CodeValidation, "--cluster is required with --project; candidates")
	ctx := runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) { return runtime.Project{}, refusal })
	_, err := ResolveDownstreamCluster(ctx, nil, flagInput{})
	require.ErrorIs(t, err, refusal)
}
