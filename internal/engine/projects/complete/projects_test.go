package complete

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectListing is the claim every project source makes: the typed
// cluster resolved, a management token, and one walk of the cluster's
// steve project collection.
func projectListing() completion.Requirements {
	r := completion.ClusterListing()
	r.Allowance = completion.AllowanceSteveProjectPages
	return r
}

// TestProjectSourceClaim asserts the shared --project source and the
// project kind's id source claim any site with the project-pages
// allowance, and inspect project's source claims its first positional
// only.
func TestProjectSourceClaim(t *testing.T) {
	t.Parallel()
	for _, src := range []completion.Source{ProjectSource(), ProjectIDSource()} {
		for _, req := range []completion.Request{{Flag: "project"}, {Index: 0}} {
			reqs, ok := src.Claim(req)
			require.True(t, ok)
			assert.Equal(t, projectListing(), reqs)
		}
	}
	reqs, ok := ProjectPositionalSource().Claim(completion.Request{Index: 0})
	require.True(t, ok)
	assert.Equal(t, projectListing(), reqs)
	for _, req := range []completion.Request{{Flag: "project"}, {Flag: "cluster"}, {Index: 1}} {
		_, ok := ProjectPositionalSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// listerConn is a Connection whose project lister answers items for the
// cluster it is asked about, recording the cluster ids it was asked for.
func listerConn(t *testing.T, items []completion.ProjectItem, err error) (*completion.Connection, *[]string) {
	t.Helper()
	conn := completiontest.NewFixed(t, `{}`).Conn(1)
	var asked []string
	conn.SetProjectLister(func(_ context.Context, c *completion.Connection, clusterID string) ([]completion.ProjectItem, error) {
		assert.Same(t, conn, c, "the lister reads through the source's connection")
		asked = append(asked, clusterID)
		return items, err
	})
	return conn, &asked
}

// TestProjectSourceCompletes asserts the projects the lister reads for the
// resolved cluster are projected to ids plus unique display names (a
// display name two projects share, or one equal to an id, is left out),
// and that a failed read or a connection without a lister answers nothing.
func TestProjectSourceCompletes(t *testing.T) {
	t.Parallel()
	items := []completion.ProjectItem{
		{ID: "p-1", DisplayName: "Default"},
		{ID: "p-2", DisplayName: "System"},
		{ID: "p-3", DisplayName: "System"},
		{ID: "p-4", DisplayName: "p-1"},
	}
	req := completion.Request{Flag: "project", Cluster: "prod", ClusterID: "c-aaa11"}
	conn, asked := listerConn(t, items, nil)
	got := ProjectSource().Complete(t.Context(), conn, req)
	assert.Equal(t, []string{"p-1", "Default", "p-2", "p-3", "p-4"}, got.Names)
	assert.Equal(t, []string{"c-aaa11"}, *asked)

	conn, _ = listerConn(t, nil, errors.New("refused"))
	assert.Empty(t, ProjectSource().Complete(t.Context(), conn, req).Names)

	bare := completiontest.NewFixed(t, `{}`)
	assert.Empty(t, ProjectSource().Complete(t.Context(), bare.Conn(1), req).Names, "no lister: fail closed")
	assert.Empty(t, bare.Requested())
}

// TestProjectIDSourceOffersIDsOnly asserts the generated project verbs'
// source offers every id and no display name: those verbs address a
// project by id only.
func TestProjectIDSourceOffersIDsOnly(t *testing.T) {
	t.Parallel()
	conn, asked := listerConn(t, []completion.ProjectItem{{ID: "p-1", DisplayName: "Default"}, {ID: "p-2", DisplayName: "System"}}, nil)
	got := ProjectIDSource().Complete(t.Context(), conn, completion.Request{Index: 0, ClusterID: "c-aaa11"})
	assert.Equal(t, []string{"p-1", "p-2"}, got.Names)
	assert.Equal(t, []string{"c-aaa11"}, *asked)

	conn, _ = listerConn(t, nil, errors.New("refused"))
	assert.Empty(t, ProjectIDSource().Complete(t.Context(), conn, completion.Request{ClusterID: "c-aaa11"}).Names)
}

// TestProjectPositionalSourceCompletesLikeTheFlag asserts inspect
// project's source offers what the shared --project source offers.
func TestProjectPositionalSourceCompletesLikeTheFlag(t *testing.T) {
	t.Parallel()
	items := []completion.ProjectItem{{ID: "p-1", DisplayName: "Default"}, {ID: "p-2", DisplayName: "Default"}, {ID: "p-3", DisplayName: "Edge"}}
	conn, _ := listerConn(t, items, nil)
	got := ProjectPositionalSource().Complete(t.Context(), conn, completion.Request{Index: 0, ClusterID: "c-aaa11"})
	assert.Equal(t, []string{"p-1", "p-2", "p-3", "Edge"}, got.Names)
}

// TestProjectSourcesDescribeEveryIdByDisplayName asserts every project
// source describes an id by its display name, raw: a project without a
// display name has none, a display name shared by two projects still
// describes both ids, and a display name offered as a value (unique, not
// an id) carries no description.
func TestProjectSourcesDescribeEveryIdByDisplayName(t *testing.T) {
	t.Parallel()
	items := []completion.ProjectItem{
		{ID: "p-1", DisplayName: "Default"},
		{ID: "p-2", DisplayName: "System"},
		{ID: "p-3", DisplayName: "System"},
		{ID: "p-4"},
		{ID: "p-5", DisplayName: "a\nb\u001b[31m"},
	}
	for name, src := range map[string]completion.Source{"shared": ProjectSource(), "positional": ProjectPositionalSource(), "ids": ProjectIDSource()} {
		conn, _ := listerConn(t, items, nil)
		got := src.Complete(t.Context(), conn, completion.Request{Index: 0, ClusterID: "c-aaa11"})
		assert.Equal(t, map[string]string{
			"p-1": "Default",
			"p-2": "System",
			"p-3": "System",
			"p-5": "a\nb\u001b[31m",
		}, got.Descriptions, name)
		for _, display := range []string{"Default", "System", "a\nb\u001b[31m"} {
			assert.NotContains(t, got.Descriptions, display, "%s: a display name offered as a value carries no description", name)
		}
	}
}

// TestProjectRowLookupIsTheDisplayName asserts the project row's
// Handle.Lookup is exactly spec.displayName, the field the project
// sources read from the lister's items.
func TestProjectRowLookupIsTheDisplayName(t *testing.T) {
	t.Parallel()
	k, ok := kinds.ByKey("project")
	require.True(t, ok)
	assert.Equal(t, [][]string{{"spec", "displayName"}}, k.DescriptionPaths())
}
