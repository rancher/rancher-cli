package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testtransport"
)

// clustersServer serves one steve page of clusters.
func clustersServer(t *testing.T, items ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(clustersHandler(items))
	t.Cleanup(srv.Close)
	return srv
}

// clustersTestServer is clustersServer on the in-memory network, for a
// test inside a synctest bubble.
func clustersTestServer(t *testing.T, items ...map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewTestServer(t, clustersHandler(items))
	// An in-memory server's URL is set by its first Client call; the root
	// client reads the URL before it asks for the client.
	_ = srv.Client()
	return srv
}

func clustersHandler(items []map[string]any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != steveClustersPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(stevePage{Data: items, Count: new(len(items)), Revision: "1"})
	})
}

type fakeRootFactory struct{ rc engine.RootClient }

func (f fakeRootFactory) Root() (engine.RootClient, error) { return f.rc, nil }

// scriptedClient answers every review with the same response.
func scriptedClient(resp engine.ReviewResponse) *fakeClient {
	return &fakeClient{script: []scripted{{resp: resp}}}
}

func failingClient(err error) *fakeClient {
	return &fakeClient{script: []scripted{{err: err}}}
}

// clustersInput is the command input with the given --as and --type.
func clustersInput(as, typ string) runtime.Input {
	strs := map[string]string{}
	if as != "" {
		strs[flagAs] = as
	}
	if typ != "" {
		strs[flagType] = typ
	}
	return fakeInput{strs: strs}
}

func decodeClusters(t *testing.T, res runtime.Result) clustersResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out clustersResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// callerFixture: the caller u-me is admitted to prod and refused on edge;
// the management cluster knows u-me with one CRTB on prod.
func callerFixture(t *testing.T) (*clustersCmd, *fakeFactory) {
	t.Helper()
	local := provenanceFixture()
	local.script = []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}}
	f := &fakeFactory{
		caller: local,
		clusters: map[string]*fakeClient{
			"c-prod": scriptedClient(whoamiResp("u-alice", "system:authenticated")),
			"c-edge": scriptedClient(http403("")),
		},
	}
	srv := clustersServer(t, steveItem("c-prod", "prod"), steveItem("c-edge", "edge"))
	cmd := &clustersCmd{clients: f, roots: rootsOver(fakeRootClient{srv}, local), local: local}
	return cmd, f
}

func TestClustersCallerModeRowsAndLocalFromTheIdentityReview(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.NoError(t, err)
	out := decodeClusters(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Equal(t, "u-alice", out.Subject.ID)
	require.Len(t, out.Clusters, 3)
	assert.Equal(t, []string{"c-edge", "c-prod", "local"}, []string{out.Clusters[0].Cluster, out.Clusters[1].Cluster, out.Clusters[2].Cluster})
	edge, prod, local := out.Clusters[0], out.Clusters[1], out.Clusters[2]
	assert.Equal(t, refused, edge.Access)
	assert.Equal(t, RowRancherProxy, edge.Source)
	assert.Nil(t, edge.Identity)
	assert.Contains(t, edge.Note, "has no access to cluster c-edge")
	assert.Equal(t, reachable, prod.Access)
	require.NotNil(t, prod.Identity)
	assert.Equal(t, "u-alice", *prod.Identity)
	assert.Equal(t, reachable, local.Access)
	assert.False(t, local.Returned)
	assert.Len(t, f.caller.Reviews, 1, "the management identity review is the local row's probe")
	assert.Equal(t, []reachableEntry{{Cluster: "c-prod", Grants: nil}, {Cluster: "local", Grants: nil}}, stripGrants(out.Reachable))
	assert.Equal(t, clustersRollup{Clusters: 3, Reachable: 2, Refused: 1, Complete: true}, out.Rollup)
	assert.Equal(t, SourceRancherProxy, out.ResultSource)
	assert.Equal(t, "complete", out.BindingsDiscovery.Status)
	assert.Contains(t, res.Text, "reachability for you")
	assert.Contains(t, res.Text, "REACHABILITY")
	assert.Contains(t, res.Text, reachabilitySentence)
	assert.Contains(t, res.WideText, "IDENTITY")
}

func stripGrants(in []reachableEntry) []reachableEntry {
	out := make([]reachableEntry, len(in))
	for i, e := range in {
		out[i] = reachableEntry{Cluster: e.Cluster}
	}
	return out
}

func TestClustersReachableGrantsComeFromTheBindings(t *testing.T) {
	t.Parallel()
	cmd, _ := callerFixture(t)
	res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.NoError(t, err)
	out := decodeClusters(t, res)
	byCluster := map[string]clusterRow{}
	for _, r := range out.Clusters {
		byCluster[r.Cluster] = r
	}
	// provenanceFixture's bindings are on c-1/c-2; prod carries only the
	// cluster-wide GlobalRoleBindings.
	names := []string{}
	for _, b := range byCluster["c-prod"].Bindings {
		names = append(names, b.Kind+"/"+b.Name)
	}
	assert.Equal(t, []string{"GlobalRoleBinding/grb-alice-reader", "GlobalRoleBinding/grb-eng-admin"}, names)
	for _, e := range out.Reachable {
		if e.Cluster == "c-prod" {
			assert.Equal(t, []grant{{Kind: "GlobalRole", Name: "admin"}, {Kind: "RoleTemplate", Name: "cluster-member"}, {Kind: "RoleTemplate", Name: "fleet-view"}}, e.Grants)
		}
	}
}

func TestClustersUnknownRowExits13(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.clusters["c-edge"] = failingClient(clierr.Retryable(clierr.CodeGeneric, "cluster agent disconnected"))
	res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.NoError(t, err)
	out := decodeClusters(t, res)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	assert.Equal(t, unknown, out.Clusters[0].Access)
	assert.Equal(t, RowRequest, out.Clusters[0].Source)
	require.NotNil(t, out.Clusters[0].Issue)
	assert.True(t, out.Clusters[0].Issue.Retryable)
	assert.False(t, out.Rollup.Complete)
	assert.Equal(t, SourceMixed, out.ResultSource)
	assert.Contains(t, res.Text, "(retryable)")
}

func TestClustersNamedSubjectImpersonatesAndLocalIsNotApplicable(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: whoamiResp("u-admin", "system:authenticated")}}
	f.impersonatedPer = map[string]*fakeClient{
		"c-prod\x00u-alice": scriptedClient(whoamiResp("u-alice", "system:authenticated")),
		"c-edge\x00u-alice": scriptedClient(http403("")),
	}
	res, err := cmd.Execute(t.Context(), clustersInput("alice", ""), nil)
	require.NoError(t, err)
	out := decodeClusters(t, res)
	assert.Equal(t, 0, res.ExitStatus, "a not applicable local row never makes the run incomplete")
	assert.Equal(t, IdentityImpersonated, out.Identity)
	assert.Equal(t, "Alice", out.Subject.DisplayName)
	assert.ElementsMatch(t, []string{"u-alice", "u-alice"}, f.Impersonated)
	local := out.Clusters[2]
	assert.Equal(t, notApplicable, local.Access)
	assert.Equal(t, RowNotApplicable, local.Source)
	assert.Equal(t, localCannotImpersonateNote, local.Note)
	assert.Equal(t, clustersRollup{Clusters: 3, Reachable: 1, Refused: 1, NotApplicable: 1, Complete: true}, out.Rollup)
	assert.Equal(t, SourceRancherProxy, out.ResultSource)
	assert.Len(t, out.Reachable, 1)
	assert.Contains(t, res.Text, "Alice (u-alice)")
	assert.Contains(t, res.Text, "1 not applicable")
}

func TestClustersNamedSubjectThatIsTheCallerTakesTheCallerPath(t *testing.T) {
	t.Parallel()
	for _, as := range []string{"u-alice", "alice", "github_user://1"} {
		t.Run(as, func(t *testing.T) {
			t.Parallel()
			cmd, f := callerFixture(t)
			res, err := cmd.Execute(t.Context(), clustersInput(as, ""), nil)
			require.NoError(t, err)
			out := decodeClusters(t, res)
			assert.Equal(t, IdentityCaller, out.Identity)
			assert.Empty(t, f.Impersonated)
			assert.Equal(t, reachable, out.Clusters[2].Access, "local is answered by the identity review")
		})
	}
}

func TestClustersUsernameMismatchIsTerminal(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: whoamiResp("u-admin")}}
	f.impersonatedPer = map[string]*fakeClient{
		"c-prod\x00u-alice": scriptedClient(whoamiResp("u-admin")),
		"c-edge\x00u-alice": scriptedClient(whoamiResp("u-alice")),
	}
	_, err := cmd.Execute(t.Context(), clustersInput("u-alice", ""), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "answered as u-admin, not u-alice")
}

func TestClustersImpersonationRefusedIsTerminal(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: whoamiResp("u-admin")}}
	f.impersonatedPer = map[string]*fakeClient{
		"c-prod\x00u-alice": scriptedClient(http403(rancherImpersonateUserRefusal)),
		"c-edge\x00u-alice": scriptedClient(whoamiResp("u-alice")),
	}
	_, err := cmd.Execute(t.Context(), clustersInput("u-alice", ""), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "of 2 clusters")
}

func TestClustersGroupAndServiceAccountAreValidation(t *testing.T) {
	t.Parallel()
	cmd, _ := callerFixture(t)
	for _, tc := range []struct{ as, typ, want string }{
		{"github_org://eng", "", "no server operation lists a group's clusters"},
		{"eng", "group", "no server operation lists a group's clusters"},
		{"system:serviceaccount:default:bot", "", "belongs to one cluster"},
		{"system:anonymous", "", "cannot be impersonated"},
	} {
		_, err := cmd.Execute(t.Context(), clustersInput(tc.as, tc.typ), nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, tc.as)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, tc.want)
	}
}

func TestClustersCallerWithoutManagementAccessIsRBAC(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: http403("")}}
	_, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

func TestClustersDiscoveryFailureIsTerminal(t *testing.T) {
	t.Parallel()
	cmd, _ := callerFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	t.Cleanup(srv.Close)
	cmd.roots = rootsOver(fakeRootClient{srv}, cmd.local)
	_, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "rancher root GET /v1/management.cattle.io.clusters: an error on the server (\"Internal Server Error\") has prevented the request from succeeding - retrying may succeed")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.True(t, ce.Retryable, "a 5xx is retryable, and the collection is read once")
}

func TestClustersProvenanceFailureDegradesNotAborts(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.ListErrFor = map[string]error{"globalrolebindings||": clierr.New(clierr.CodeRBAC, "forbidden")}
	res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.NoError(t, err)
	out := decodeClusters(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, "partial", out.BindingsDiscovery.Status)
	for _, e := range out.Reachable {
		assert.Nil(t, e.Grants)
	}
	assert.Contains(t, res.Text, "bindings: partial (grb: forbidden)")
	assert.Contains(t, res.Text, "?")
}

func TestClustersCancellationIsTerminal(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	f.clusters["c-prod"].Before = func(context.Context) error { cancel(); return ctx.Err() }
	_, err := cmd.Execute(ctx, clustersInput("", ""), nil)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "cancel") || errors.Is(err, context.Canceled), err.Error())
}

func TestClustersDescriptorOffersOnlyUsers(t *testing.T) {
	t.Parallel()
	d := NewClusters(nil, nil, nil).Describe()
	assert.Equal(t, "access clusters", d.Name)
	assert.Equal(t, []int{0, exitUnknown}, d.SuccessExitCodes)
	for _, f := range d.Flags {
		if f.Name == flagType {
			assert.Equal(t, []string{typeUser}, f.Enum)
		}
	}
	assert.Equal(t, runtime.ShapeText, d.Shape)
}

func TestClustersIdentityIsNullOnEveryNonReachableRow(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: whoamiResp("u-admin")}}
	f.impersonatedPer = map[string]*fakeClient{
		"c-prod\x00u-alice": failingClient(clierr.Retryable(clierr.CodeGeneric, "503")),
		"c-edge\x00u-alice": scriptedClient(http403("")),
	}
	res, err := cmd.Execute(t.Context(), clustersInput("u-alice", ""), nil)
	require.NoError(t, err)
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out struct {
		Clusters []map[string]any `json:"clusters"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Clusters, 3)
	for _, row := range out.Clusters {
		v, present := row["identity"]
		assert.True(t, present, "%s carries identity", row["cluster"])
		assert.Nil(t, v, "%s (%s) has no identity", row["cluster"], row["access"])
	}
	assert.Equal(t, []any{"refused", "unknown", "notApplicable"}, []any{out.Clusters[0]["access"], out.Clusters[1]["access"], out.Clusters[2]["access"]})
}

// blockingClient answers a review only after release is closed (or the
// context ends), counting how many reviews are in flight at once.
type blockingClient struct {
	*fakeClient
	release  <-chan struct{}
	inFlight *atomic.Int32
	peak     *atomic.Int32
}

func (b *blockingClient) PostReview(ctx context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	n := b.inFlight.Add(1)
	defer b.inFlight.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return engine.ReviewResponse{}, ctx.Err()
	}
	return b.fakeClient.PostReview(ctx, req)
}

func TestClustersProbesRespectTheFanOutLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// NOT parallel: shrinks the package-level fan-out limit.
		old := fanOutLimit
		fanOutLimit = 2
		t.Cleanup(func() { fanOutLimit = old })
		release := make(chan struct{})
		var inFlight, peak atomic.Int32
		local := provenanceFixture()
		local.script = []scripted{{resp: whoamiResp("u-alice")}}
		f := &fakeFactory{caller: local, clusterClients: map[string]engine.ClusterClient{}}
		var items []map[string]any
		for i := range 5 {
			id := fmt.Sprintf("c-%d", i)
			items = append(items, steveItem(id, id))
			f.clusterClients[id] = &blockingClient{fakeClient: scriptedClient(whoamiResp("u-alice")), release: release, inFlight: &inFlight, peak: &peak}
		}
		srv := clustersTestServer(t, items...)
		cmd := &clustersCmd{clients: f, roots: rootsOver(fakeRootClient{srv}, local), local: local}
		done := make(chan struct{})
		go func() {
			defer close(done)
			res, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
			assert.NoError(t, err)
			assert.Equal(t, 0, res.ExitStatus)
		}()
		// Every goroutine in the bubble is durably blocked once two probes wait
		// on the release channel and the others wait on the semaphore.
		synctest.Wait()
		assert.Equal(t, int32(2), inFlight.Load(), "two probes in flight, no more")
		assert.Equal(t, int32(2), peak.Load(), "never more reviews in flight than the fan-out limit")
		close(release)
		<-done
		assert.Equal(t, int32(2), peak.Load())
	})
}

func TestClustersTerminalAnswerWinsOverABlockedSibling(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	f.caller.script = []scripted{{resp: whoamiResp("u-admin")}}
	release := make(chan struct{})
	var inFlight, peak atomic.Int32
	f.impersonatedClients = map[string]engine.ClusterClient{
		"c-edge\x00u-alice": &blockingClient{fakeClient: scriptedClient(whoamiResp("u-alice")), release: release, inFlight: &inFlight, peak: &peak},
		"c-prod\x00u-alice": scriptedClient(http403(rancherImpersonateUserRefusal)),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := cmd.Execute(ctx, clustersInput("u-alice", ""), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code, "the refusal, not the cancelled sibling, is the answer")
	require.ErrorContains(t, err, "not allowed to impersonate user")
	assert.Equal(t, int32(0), inFlight.Load(), "the blocked sibling was cancelled")
}

func TestClustersBudgetExpiryDuringDiscoveryIsTerminal(t *testing.T) {
	// NOT parallel: shrinks the package-level fan-out budget.
	old := fanOutBudget
	fanOutBudget = 50 * time.Millisecond
	t.Cleanup(func() { fanOutBudget = old })
	cmd, _ := callerFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) }))
	t.Cleanup(srv.Close)
	cmd.roots = rootsOver(fakeRootClient{srv}, cmd.local)
	_, err := cmd.Execute(t.Context(), clustersInput("", ""), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "fan-out budget")
	require.ErrorContains(t, err, "cluster discovery")
}

// TestClustersTimeoutFlagIsTheBudget asserts --timeout replaces the default
// budget for the call and the terminal error names the value given.
func TestClustersTimeoutFlagIsTheBudget(t *testing.T) {
	t.Parallel()
	cmd, _ := callerFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { testtransport.HangAndAbort(r) }))
	t.Cleanup(srv.Close)
	cmd.roots = rootsOver(fakeRootClient{srv}, cmd.local)
	_, err := cmd.Execute(t.Context(), fakeInput{strs: map[string]string{flagTimeout: "50ms"}}, nil)
	require.ErrorContains(t, err, "fan-out budget of 50ms exhausted during cluster discovery")
}

// TestClustersTimeoutFlagInvalidIsValidation asserts a malformed --timeout
// is refused before any request.
func TestClustersTimeoutFlagInvalidIsValidation(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	_, err := cmd.Execute(t.Context(), fakeInput{strs: map[string]string{flagTimeout: "soon"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, `--timeout "soon" is not a valid duration`)
	assert.Empty(t, f.caller.Reviews, "nothing was asked")
}

// TestClustersBudgetExpiryDuringProbesNamesTheStageAndProgress asserts the
// cluster probe runner's exhaustion message has the package's one shape:
// the budget, the stage, and how far the probes got (the management
// cluster's identity review counts as the one evaluated of three).
func TestClustersBudgetExpiryDuringProbesNamesTheStageAndProgress(t *testing.T) {
	t.Parallel()
	cmd, f := callerFixture(t)
	for _, c := range f.clusters {
		c.Before = blockUntilDone
	}
	_, err := cmd.Execute(t.Context(), fakeInput{strs: map[string]string{flagTimeout: "50ms"}}, nil)
	require.ErrorContains(t, err, "fan-out budget of 50ms exhausted during the cluster probes; evaluated 1 of 3 clusters")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
}
