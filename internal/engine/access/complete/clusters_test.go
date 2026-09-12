package complete

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
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

// page is one steve page as the fixture serves it.
type page struct {
	Data     []map[string]any `json:"data"`
	Count    *int             `json:"count,omitempty"`
	Continue string           `json:"continue,omitempty"`
	Revision string           `json:"revision"`
}

func item(id, display string) map[string]any {
	return map[string]any{"id": id, "metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
}

// pagedServer serves pages keyed by the continue token the previous page
// announced and records every request.
func pagedServer(t *testing.T, pages map[string]page) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Path != "/v1/management.cattle.io.clusters" {
			http.NotFound(w, r)
			return
		}
		pg, ok := pages[r.URL.Query().Get("continue")]
		if !ok {
			http.Error(w, "no such page", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(pg)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
}

// TestProjectsClusterSourceWalksThePages asserts the source follows the
// continuation tokens, encodes them, inserts the management cluster row,
// orders by id and projects ids plus unique display names.
func TestProjectsClusterSourceWalksThePages(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServer(t, map[string]page{
		"":       {Data: []map[string]any{item("c-m-b", "b")}, Count: new(3), Continue: "2 of 3", Revision: "9"},
		"2 of 3": {Data: []map[string]any{item("c-m-a", "a")}, Count: new(3), Continue: "x&y", Revision: "9"},
		"x&y":    {Data: []map[string]any{item("c-m-c", "c")}, Count: new(3), Revision: "9"},
	})
	conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, 100)
	got := ProjectsClusterSource().Complete(t.Context(), conn, completion.Request{Flag: "cluster"})
	assert.Equal(t, []string{"c-m-a", "a", "c-m-b", "b", "c-m-c", "c", "local"}, got.Names)
	assert.Equal(t, []string{"/v1/management.cattle.io.clusters", "/v1/management.cattle.io.clusters?continue=2+of+3", "/v1/management.cattle.io.clusters?continue=x%26y"}, paths())
	assert.Equal(t, 3, conn.Used())
}

// TestProjectsClusterSourceKeepsAReturnedLocalRow asserts a listed local
// row is kept as returned and its display name, equal to its id, is not
// offered twice.
func TestProjectsClusterSourceKeepsAReturnedLocalRow(t *testing.T) {
	t.Parallel()
	srv, _ := pagedServer(t, map[string]page{
		"": {Data: []map[string]any{item("local", "local"), item("c-m-a", "a")}, Count: new(2), Revision: "1"},
	})
	got := ProjectsClusterSource().Complete(t.Context(), completion.NewConnection(srv.Client(), "tok", srv.URL, false, 100), completion.Request{})
	assert.Equal(t, []string{"c-m-a", "a", "local"}, got.Names)
}

// TestProjectsClusterSourceAnswersNothingOnProtocolFaults asserts every
// disagreement the command treats as an error answers nothing: a moved
// revision, a moved count, a missing revision or count, an item without
// an id, an item whose name disagrees with its id, a repeated item that
// disagrees with itself, a repeated continue token, a final count that
// does not match what was read, and a page past the allowance.
func TestProjectsClusterSourceAnswersNothingOnProtocolFaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		pages map[string]page
		limit int
	}{
		{name: "revision moved", limit: 100, pages: map[string]page{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Revision: "2"}}},
		{name: "count moved", limit: 100, pages: map[string]page{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(3), Revision: "1"}}},
		{name: "no revision", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Count: new(1)}}},
		{name: "no count", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Revision: "1"}}},
		{name: "item without id", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{{"metadata": map[string]any{"name": "x"}}}, Count: new(1), Revision: "1"}}},
		{name: "item names itself otherwise", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{{"id": "c-m-a", "metadata": map[string]any{"name": "c-m-b"}}}, Count: new(1), Revision: "1"}}},
		{name: "repeated item disagrees", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{item("c-m-a", "a"), item("c-m-a", "b")}, Count: new(2), Revision: "1"}}},
		{name: "continue token repeats", limit: 100, pages: map[string]page{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Continue: "n", Revision: "1"}}},
		{name: "count does not match what was read", limit: 100, pages: map[string]page{
			"": {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Revision: "1"}}},
		{name: "page past the allowance", limit: 1, pages: map[string]page{
			"":  {Data: []map[string]any{item("c-m-a", "a")}, Count: new(2), Continue: "n", Revision: "1"},
			"n": {Data: []map[string]any{item("c-m-b", "b")}, Count: new(2), Revision: "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv, _ := pagedServer(t, tt.pages)
			conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, tt.limit)
			got := ProjectsClusterSource().Complete(t.Context(), conn, completion.Request{Flag: "cluster"})
			assert.Empty(t, got.Names)
		})
	}
}
