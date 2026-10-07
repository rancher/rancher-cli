package complete

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClusterSourceClaimsEverySite asserts the shared source claims any
// site with a management need, no cluster need and one listing.
func TestClusterSourceClaimsEverySite(t *testing.T) {
	t.Parallel()
	for _, req := range []completion.Request{{Flag: "cluster"}, {Index: 0}, {Flag: "cluster", Cluster: "prod cluster"}} {
		reqs, ok := ClusterSource().Claim(req)
		require.True(t, ok)
		assert.Equal(t, completion.ManagementListing(), reqs)
	}
}

// TestClusterSourceCompletes asserts the management list is read once and
// projected to ids plus unique display names.
func TestClusterSourceCompletes(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"c-aaa11"},"spec":{"displayName":"prod"}},
		{"metadata":{"name":"local"},"spec":{"displayName":"local"}},
		{"metadata":{"name":"c-bbb22"},"spec":{"displayName":"dup"}},
		{"metadata":{"name":"c-ccc33"},"spec":{"displayName":"dup"}}
	]}`)
	got := ClusterSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "cluster"})
	assert.Equal(t, []string{"c-aaa11", "prod", "local", "c-bbb22", "c-ccc33"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/clusters"}, s.Requested())

	refused := completiontest.NewStatus(t, 403)
	assert.Empty(t, ClusterSource().Complete(t.Context(), refused.Conn(1), completion.Request{}).Names)
}

// TestProjectsClusterSourceClaimsTheClusterFlagOnly asserts the command's
// source owns --cluster under the page-walk allowance and declines every
// other site.
func TestProjectsClusterSourceClaimsTheClusterFlagOnly(t *testing.T) {
	t.Parallel()
	reqs, ok := ProjectsClusterSource().Claim(completion.Request{Flag: "cluster"})
	require.True(t, ok)
	assert.Equal(t, completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceSteveClusterPages}, reqs)
	for _, req := range []completion.Request{{Flag: "as"}, {Flag: "timeout"}, {Index: 0}} {
		_, ok := ProjectsClusterSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestSteveClusterSourceClaimsTheClusterFlagAndTheFirstPositional asserts
// the shared source owns a --cluster value and a first positional (inspect
// cluster's) under the page-walk allowance, and declines every other site.
func TestSteveClusterSourceClaimsTheClusterFlagAndTheFirstPositional(t *testing.T) {
	t.Parallel()
	want := completion.Requirements{Cluster: completion.ClusterNone, Credential: completion.CredentialManagement, Allowance: completion.AllowanceSteveClusterPages}
	for _, req := range []completion.Request{{Flag: "cluster"}, {Index: 0}, {Flag: "cluster", Cluster: "prod cluster"}} {
		reqs, ok := SteveClusterSource().Claim(req)
		require.True(t, ok, "%+v", req)
		assert.Equal(t, want, reqs)
	}
	for _, req := range []completion.Request{{Flag: "as"}, {Flag: "output"}, {Index: 1}} {
		_, ok := SteveClusterSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// listerConn is a connection whose cluster lister answers list or err and
// counts its calls.
func listerConn(list []completion.ClusterItem, err error, calls *int) *completion.Connection {
	conn := completion.NewConnection(http.DefaultClient, "tok", "https://rancher.invalid", false, 1)
	conn.SetClusterLister(func(context.Context, *completion.Connection) ([]completion.ClusterItem, error) {
		*calls++
		return slices.Clone(list), err
	})
	return conn
}

// TestSteveClusterSourceCompletes asserts the lister is read once and
// projected to ids plus unique display names, in its order, and that no
// lister or a failed read answers nothing.
func TestSteveClusterSourceCompletes(t *testing.T) {
	t.Parallel()
	calls := 0
	list := []completion.ClusterItem{{ID: "c-aaa11", DisplayName: "prod"}, {ID: "local", DisplayName: "local"},
		{ID: "c-bbb22", DisplayName: "dup"}, {ID: "c-ccc33", DisplayName: "dup"}}
	got := SteveClusterSource().Complete(t.Context(), listerConn(list, nil, &calls), completion.Request{Flag: "cluster"})
	assert.Equal(t, []string{"c-aaa11", "prod", "local", "c-bbb22", "c-ccc33"}, got.Names)
	assert.Equal(t, 1, calls)

	got = SteveClusterSource().Complete(t.Context(), listerConn(list, errors.New("moved"), &calls), completion.Request{})
	assert.Empty(t, got.Names)

	bare := completion.NewConnection(http.DefaultClient, "tok", "https://rancher.invalid", false, 1)
	assert.Empty(t, SteveClusterSource().Complete(t.Context(), bare, completion.Request{}).Names, "no lister fails closed")
}

// TestProjectsClusterSourceAddsTheLocalRowInIDOrder asserts the command's
// source adds the management cluster's row when the read left it out,
// keeps a returned one as returned (its display name, equal to its id, is
// offered once), and orders the rows by id.
func TestProjectsClusterSourceAddsTheLocalRowInIDOrder(t *testing.T) {
	t.Parallel()
	calls := 0
	list := []completion.ClusterItem{{ID: "c-m-b", DisplayName: "b"}, {ID: "c-m-a", DisplayName: "a"}, {ID: "c-m-c", DisplayName: "c"}}
	got := ProjectsClusterSource().Complete(t.Context(), listerConn(list, nil, &calls), completion.Request{Flag: "cluster"})
	assert.Equal(t, []string{"c-m-a", "a", "c-m-b", "b", "c-m-c", "c", "local"}, got.Names)

	list = []completion.ClusterItem{{ID: "local", DisplayName: "local"}, {ID: "c-m-a", DisplayName: "a"}}
	got = ProjectsClusterSource().Complete(t.Context(), listerConn(list, nil, &calls), completion.Request{Flag: "cluster"})
	assert.Equal(t, []string{"c-m-a", "a", "local"}, got.Names)

	got = ProjectsClusterSource().Complete(t.Context(), listerConn(nil, errors.New("moved"), &calls), completion.Request{Flag: "cluster"})
	assert.Empty(t, got.Names, "a failed read offers not even the local row")
}
