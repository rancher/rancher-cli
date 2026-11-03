package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// steveServer serves one page per collection path; a path listed with a nil
// page answers 500.
func steveServer(t *testing.T, pages map[string]*stevePage) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if page == nil {
			http.Error(w, "steve is unwell", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func page(items ...map[string]any) *stevePage {
	return &stevePage{Data: items, Count: new(len(items)), Revision: "1"}
}

// projectsPages is the caller's steve view: two downstream clusters, four
// projects, and per-cluster namespaces with one unlisted and one unlabeled.
func projectsPages() map[string]*stevePage {
	return map[string]*stevePage{
		steveClustersPath: page(steveItem("c-1", "prod"), steveItem("c-2", "edge")),
		steveProjectsPath: page(
			steveProject("c-1", "p-a", "Payments"), steveProject("c-1", "p-b", "Ops"),
			steveProject("c-2", "p-c", "Edge"), steveProject("local", "p-l", "System"),
		),
		steveNamespacesPath("c-1"):   page(steveNamespace("c-1", "pay2", "p-a"), steveNamespace("c-1", "ops", "p-b"), steveNamespace("c-1", "stray", "p-zz"), steveNamespace("c-1", "plain", ""), steveNamespace("c-1", "pay1", "p-a")),
		steveNamespacesPath("c-2"):   page(steveNamespace("c-2", "edge1", "p-c")),
		steveNamespacesPath("local"): page(steveNamespace("local", "cattle-system", "p-l"), steveNamespace("local", "fleet-default", "")),
	}
}

// projectsFixture: alice (the caller) is admitted everywhere; on c-1 she may
// get pay1 and pay2 (her project-member PRTB on p-a) but not ops; on c-2
// she may not get edge1; on local she may get cattle-system through global
// grants. Reviews are scripted in the order the command asks them, so the
// fan-out limit is one.
func projectsFixture(t *testing.T, pages map[string]*stevePage) (*projectsCmd, *fakeFactory) {
	t.Helper()
	old := fanOutLimit
	fanOutLimit = 1
	t.Cleanup(func() { fanOutLimit = old })
	local := provenanceFixture()
	local.script = []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(true, `RBAC: allowed by ClusterRoleBinding "crb-l" of ClusterRole "p-l-namespaces-edit" to User "u-alice"`)}}
	f := &fakeFactory{
		caller: local,
		clusters: map[string]*fakeClient{
			"c-1": {script: []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(true, `RBAC: allowed by ClusterRoleBinding "crb-a" of ClusterRole "p-a-namespaces-edit" to User "u-alice"`)}, {resp: sar(true, "")}, {resp: sar(false, "")}}},
			"c-2": {script: []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(false, "")}}},
		},
	}
	srv := steveServer(t, pages)
	cmd := &projectsCmd{clustersCmd{clients: f, roots: rootsOver(fakeRootClient{srv}, local), local: local}}
	return cmd, f
}

func projectsInput(cluster, as, typ string) runtime.Input {
	strs := map[string]string{}
	if cluster != "" {
		strs["cluster"] = cluster
	}
	if as != "" {
		strs[flagAs] = as
	}
	if typ != "" {
		strs[flagType] = typ
	}
	return fakeInput{strs: strs}
}

func decodeProjects(t *testing.T, res runtime.Result) projectsResult {
	t.Helper()
	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out projectsResult
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func projectByID(t *testing.T, out projectsResult, cluster, project string) projectRow {
	t.Helper()
	for _, cr := range out.Clusters {
		if cr.Cluster != cluster {
			continue
		}
		for _, pr := range cr.Projects {
			if pr.Project == project {
				return pr
			}
		}
	}
	t.Fatalf("no project %s/%s", cluster, project)
	return projectRow{}
}

func TestProjectsCallerModeVisibilityMembershipAndInheritedAccess(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Equal(t, 0, res.ExitStatus)
	assert.Equal(t, IdentityCaller, out.Identity)
	assert.Equal(t, "", out.Cluster)
	require.Len(t, out.Clusters, 3)
	assert.Equal(t, []string{"c-1", "c-2", "local"}, []string{out.Clusters[0].Cluster, out.Clusters[1].Cluster, out.Clusters[2].Cluster})

	pa := projectByID(t, out, "c-1", "p-a")
	assert.Equal(t, visibilityAll, pa.Visibility)
	assert.Equal(t, SourceAuthorizer, pa.Source)
	assert.True(t, pa.Complete)
	require.Len(t, pa.Namespaces, 2)
	assert.Equal(t, []string{"pay1", "pay2"}, []string{pa.Namespaces[0].Namespace, pa.Namespaces[1].Namespace}, "namespaces are asked and listed in name order")
	assert.Equal(t, nsAllowed, pa.Namespaces[0].Access)
	assert.Contains(t, pa.Namespaces[0].Reason, "p-a-namespaces-edit")
	var kinds []string
	for _, b := range pa.Bindings {
		kinds = append(kinds, b.Kind+"/"+b.Name)
	}
	assert.Equal(t, []string{"ClusterRoleTemplateBinding/crtb-alice", "GlobalRoleBinding/grb-alice-reader", "GlobalRoleBinding/grb-eng-admin", "ProjectRoleTemplateBinding/prtb-alice"}, kinds,
		"a project's bindings: its PRTBs, the cluster's CRTBs, every GRB")

	pb := projectByID(t, out, "c-1", "p-b")
	assert.Equal(t, visibilityNone, pb.Visibility)
	assert.Equal(t, "", pb.Note, "no PRTB on p-b, so a denial is not a disagreement")
	pc := projectByID(t, out, "c-2", "p-c")
	assert.Equal(t, visibilityNone, pc.Visibility)
	pl := projectByID(t, out, "local", "p-l")
	assert.Equal(t, visibilityAll, pl.Visibility)

	assert.Equal(t, []unlistedProject{{Project: "p-zz", Namespaces: []string{"stray"}}}, out.Clusters[0].UnlistedProjectNamespaces)
	assert.Empty(t, out.Clusters[2].UnlistedProjectNamespaces, "an unlabeled namespace belongs to no project and is ignored")
	require.NotNil(t, out.Clusters[0].NamespaceDiscovery)
	assert.Nil(t, out.Clusters[0].NamespaceDiscovery.Issue)

	assert.Equal(t, []visibleProject{
		{Cluster: "c-1", Project: "p-a", Namespaces: 2, AllowedIn: 2, Complete: true},
		{Cluster: "local", Project: "p-l", Namespaces: 1, AllowedIn: 1, Complete: true},
	}, out.VisibleProjects)
	require.NotNil(t, out.MemberOf)
	require.Len(t, out.MemberOf.Projects, 1)
	assert.Equal(t, "p-a", out.MemberOf.Projects[0].Project)
	assert.Equal(t, []grant{{Kind: "RoleTemplate", Name: "project-member"}}, out.MemberOf.Projects[0].Grants)
	assert.True(t, out.MemberOf.Complete)
	require.Len(t, out.InheritedAccess, 1)
	assert.Equal(t, "p-l", out.InheritedAccess[0].Project)
	assert.Contains(t, out.InheritedAccess[0].Grants, grant{Kind: "GlobalRole", Name: "admin"})

	assert.Equal(t, projectsRollup{Clusters: 3, Reachable: 3, Projects: 4, All: 2, None: 2, NamespacesAsked: 5, Members: 1, Complete: true}, out.Rollup)
	assert.Equal(t, SourceMixed, out.ResultSource, "proxy rows and authorizer rows in one healthy answer")
	assert.Len(t, f.clusters["c-1"].Reviews, 4, "one identity review and one review per project namespace; the unlisted and unlabeled namespaces are never asked")
	assert.Contains(t, res.Text, "visibility for you")
	assert.Contains(t, res.Text, "VISIBILITY")
	assert.Contains(t, res.Text, "you are a member of: c-1/p-a (project-member)")
	assert.Contains(t, res.Text, "inherited access (cluster or global grants, not membership): local/p-l")
	assert.Contains(t, res.Text, visibilityDefinition)
	assert.Contains(t, res.WideText, "pay1=allowed pay2=allowed")
}

func TestProjectsClusterFlagResolvesFromTheSteveSet(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	res, err := cmd.Execute(t.Context(), projectsInput("prod", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Equal(t, "c-1", out.Cluster)
	require.Len(t, out.Clusters, 1)
	assert.Len(t, out.Clusters[0].Projects, 2)
	assert.Empty(t, f.clusters["c-2"].Reviews, "the other cluster is never probed")
	assert.Equal(t, 0, f.ResolveCalls, "the proxy-backed resolver is not used")
	assert.Contains(t, res.Text, "projects you can list on c-1")

	// By id, on a fresh fixture (the scripted reviews are consumed per run).
	cmd, _ = projectsFixture(t, projectsPages())
	res, err = cmd.Execute(t.Context(), projectsInput("c-2", "", ""), nil)
	require.NoError(t, err)
	assert.Equal(t, "c-2", decodeProjects(t, res).Cluster)
}

func TestProjectsClusterFlagUnknownAndAmbiguous(t *testing.T) {
	// Not parallel: the fixture shrinks the package-level fan-out limit.
	pages := projectsPages()
	pages[steveClustersPath] = page(steveItem("c-1", "prod"), steveItem("c-2", "prod"))
	cmd, _ := projectsFixture(t, pages)
	_, err := cmd.Execute(t.Context(), projectsInput("prod", "", ""), nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []clierr.Candidate{{ID: "c-1", Name: "prod"}, {ID: "c-2", Name: "prod"}}, ce.Candidates)

	_, err = cmd.Execute(t.Context(), projectsInput("nope", "", ""), nil)
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Len(t, ce.Candidates, 3, "every listed cluster, local included")
	assert.Equal(t, "cluster not found: 'nope'; it is not among the clusters you can read; pass a cluster id or display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}

// TestProjectsClusterFlagIDWinsWithAWarning asserts an id that is also
// another cluster's display name selects the id, and the warning naming
// the other cluster is in the result.
func TestProjectsClusterFlagIDWinsWithAWarning(t *testing.T) {
	// Not parallel: the fixture shrinks the package-level fan-out limit.
	pages := projectsPages()
	pages[steveClustersPath] = page(steveItem("c-1", "prod"), steveItem("c-2", "c-1"))
	cmd, _ := projectsFixture(t, pages)
	res, err := cmd.Execute(t.Context(), projectsInput("c-1", "", ""), nil)
	require.NoError(t, err)
	assert.Equal(t, "c-1", decodeProjects(t, res).Cluster)
	assert.Equal(t, []string{"--cluster 'c-1' is the id of cluster 'c-1' and the display name of cluster 'c-2'; the command uses the one with that id. To use the other one, pass --cluster 'c-2'"}, res.Warnings)
}

// TestResolveClusterArgLocalIsTheManagementRow asserts "local" selects the
// management cluster's row before any match, even when a downstream
// cluster's display name is "local", with no warning; and a match returns
// the row the set holds.
func TestResolveClusterArgLocalIsTheManagementRow(t *testing.T) {
	t.Parallel()
	rows := withLocal([]discoveredCluster{{ID: "c-1", DisplayName: "local", Returned: true}})
	got, warning, err := resolveClusterArg(rows, "local")
	require.NoError(t, err)
	assert.Equal(t, discoveredCluster{ID: "local", DisplayName: "local"}, got)
	assert.Empty(t, warning)

	got, _, err = resolveClusterArg(rows, "c-1")
	require.NoError(t, err)
	assert.Equal(t, discoveredCluster{ID: "c-1", DisplayName: "local", Returned: true}, got)
}

func TestProjectsNamespaceDiscoveryFailureIsTheClustersIssue(t *testing.T) {
	pages := projectsPages()
	pages[steveNamespacesPath("c-1")] = nil
	cmd, f := projectsFixture(t, pages)
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	c1 := out.Clusters[0]
	require.NotNil(t, c1.NamespaceDiscovery)
	require.NotNil(t, c1.NamespaceDiscovery.Issue)
	assert.Equal(t, RowScopeDiscovery, c1.NamespaceDiscovery.Source)
	for _, pr := range c1.Projects {
		assert.Equal(t, visibilityUnknown, pr.Visibility)
		assert.Equal(t, SourceScopeDiscovery, pr.Source)
		assert.False(t, pr.Complete)
		assert.Equal(t, discoveryFailedNote, pr.Note)
		assert.Empty(t, pr.Namespaces)
	}
	assert.Len(t, f.clusters["c-1"].Reviews, 1, "only the identity review; no namespace was asked about")
	assert.Equal(t, 2, out.Rollup.ProjectsUnknown)
	assert.False(t, out.Rollup.Complete)
	assert.Contains(t, res.Text, "namespace discovery failed")
}

func TestProjectsEmptyProjectIsUnknownFromScopeDiscovery(t *testing.T) {
	pages := projectsPages()
	pages[steveNamespacesPath("c-2")] = page(steveNamespace("c-2", "plain", ""))
	cmd, _ := projectsFixture(t, pages)
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	pc := projectByID(t, out, "c-2", "p-c")
	assert.Equal(t, visibilityUnknown, pc.Visibility)
	assert.Equal(t, SourceScopeDiscovery, pc.Source)
	assert.Equal(t, emptyProjectNote, pc.Note)
	assert.False(t, pc.Complete)
	assert.Equal(t, exitUnknown, res.ExitStatus)
	assert.Regexp(t, `(?m)^c-2\s+p-c\s+Edge\s+unknown\s+-\s+`, res.WideText, "an empty project's NAMESPACES cell is the dash every other empty cell uses")
}

func TestProjectsNamedSubjectImpersonatesAndLocalIsNotApplicable(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.impersonatedPer = map[string]*fakeClient{
		"c-1\x00u-bob": {script: []scripted{{resp: whoamiResp("u-bob")}, {resp: sar(false, "")}, {resp: sar(false, "")}, {resp: sar(true, "")}}},
		"c-2\x00u-bob": {script: []scripted{{resp: whoamiResp("u-bob")}, {resp: sar(false, "")}}},
	}
	res, err := cmd.Execute(t.Context(), projectsInput("", "bob", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Equal(t, IdentityImpersonated, out.Identity)
	assert.Equal(t, "u-bob", out.Subject.ID)
	local := out.Clusters[2]
	assert.Equal(t, notApplicable, local.Access)
	require.Len(t, local.Projects, 1)
	assert.Equal(t, visibilityNotApplicable, local.Projects[0].Visibility)
	assert.Equal(t, ResultSource(RowNotApplicable), local.Projects[0].Source)
	assert.True(t, local.Projects[0].Complete)
	assert.Equal(t, localCannotImpersonateNote, local.Projects[0].Note)
	assert.Nil(t, local.NamespaceDiscovery, "no namespace read on a cluster no review answers for")
	pb := projectByID(t, out, "c-1", "p-b")
	assert.Equal(t, visibilityAll, pb.Visibility)
	assert.Empty(t, f.clusters["c-1"].Reviews, "the caller's client is never asked about bob")
	assert.Equal(t, 1, out.Rollup.ClustersNotApplicable)
	assert.Equal(t, 1, out.Rollup.ProjectsNotApplicable)
	assert.Equal(t, 0, out.Rollup.ProjectsUnknown)
	assert.Equal(t, 0, res.ExitStatus, "the management cluster's projects never count against completeness for a named user")
	assert.Contains(t, res.Text, "1 not applicable; ")
}

func TestProjectsRefusedClusterProjectsAreNoneFromTheProxy(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.clusters["c-2"] = scriptedClient(http403(""))
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Equal(t, refused, out.Clusters[1].Access)
	pc := projectByID(t, out, "c-2", "p-c")
	assert.Equal(t, visibilityNone, pc.Visibility)
	assert.Equal(t, ResultSource(RowRancherProxy), pc.Source)
	assert.True(t, pc.Complete)
	assert.Equal(t, refusedClusterNote, pc.Note)
	assert.Empty(t, pc.Namespaces)
	assert.Equal(t, 1, out.Rollup.Refused)
	assert.Equal(t, 0, out.Rollup.ProjectsUnknown)
	assert.Equal(t, 0, res.ExitStatus, "a refusal is an answer, not an unknown")
}

func TestProjectsMembershipNotDeterminedWhenThePRTBReadFails(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.caller.ListErrFor = map[string]error{"projectroletemplatebindings||": clierr.New(clierr.CodeRBAC, "forbidden")}
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	assert.Nil(t, out.MemberOf)
	assert.Equal(t, "partial", out.BindingsDiscovery.Status)
	assert.Contains(t, res.Text, membershipNotDeterminedPrefix+"forbidden")
	assert.Nil(t, out.InheritedAccess)
	assert.Contains(t, res.Text, "inherited access not determined")
	pa := projectByID(t, out, "c-1", "p-a")
	assert.Equal(t, visibilityAll, pa.Visibility, "visibility is the server's answer and does not depend on the bindings")
	assert.Equal(t, "", pa.Note, "an incomplete provenance read never claims a missing binding")
	assert.Equal(t, 0, res.ExitStatus, "a failed provenance read is not an unknown answer")
}

func TestProjectsDisagreementRowIsNoted(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	// alice's PRTB is on p-a; deny pay1 and pay2.
	f.clusters["c-1"].script = []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(false, "")}, {resp: sar(false, "")}, {resp: sar(false, "")}}
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	pa := projectByID(t, out, "c-1", "p-a")
	assert.Equal(t, visibilityNone, pa.Visibility)
	assert.Equal(t, disagreementNote, pa.Note)
	assert.Empty(t, out.MemberOf.Projects, "a denied project is not a membership")
	assert.NotContains(t, res.Text, "c-1/p-a (project-member)")
}

// TestProjectsReachedByAGlobalRoleBindingIsInheritedNotNoBinding asserts a
// project reached only through a GlobalRoleBinding is inherited access
// and carries no note; the no-binding note is asserted by
// TestProjectsVisibleWithoutABindingNamesTheKubernetesBindings.
func TestProjectsReachedByAGlobalRoleBindingIsInheritedNotNoBinding(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	// c-2: allow edge1 while alice's only c-2 binding is a group CRTB; drop
	// the group so nothing names her on c-2.
	f.clusters["c-2"].script = []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(true, "")}}
	f.caller.Objects["u-alice-attr"] = mgmtObj("UserAttribute", "", "u-alice", map[string]any{})
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	pc := projectByID(t, out, "c-2", "p-c")
	assert.Equal(t, visibilityAll, pc.Visibility)
	// The fleet-reader GRB inherits cluster-member on every cluster, so p-c
	// is inherited access, never "no binding".
	assert.Equal(t, "", pc.Note)
	var inherited []string
	for _, ia := range out.InheritedAccess {
		inherited = append(inherited, ia.Cluster+"/"+ia.Project)
	}
	assert.Contains(t, inherited, "c-2/p-c")
}

func TestProjectsGroupAndServiceAccountAreValidation(t *testing.T) {
	t.Parallel()
	cmd := &projectsCmd{clustersCmd{clients: &fakeFactory{caller: &fakeClient{}}, roots: fakeRootFactory{}}}
	for _, as := range []string{"github_org://eng", "system:serviceaccount:ns:sa", "system:anonymous"} {
		_, err := cmd.Execute(t.Context(), projectsInput("", as, ""), nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, as)
		assert.Equal(t, clierr.CodeValidation, ce.Code, as)
	}
}

func TestProjectsDescriptor(t *testing.T) {
	t.Parallel()
	d := NewProjects(nil, nil, nil).Describe()
	assert.Equal(t, "access projects", d.Name)
	var flags []string
	for _, f := range d.Flags {
		flags = append(flags, f.Name)
		if f.Name == flagType {
			assert.Equal(t, []string{typeUser}, f.Enum)
		}
		if f.Name == "cluster" {
			assert.False(t, f.Required, "--cluster is optional: omitted means every cluster")
		}
	}
	assert.Equal(t, []string{"cluster", flagAs, flagType, flagTimeout}, flags)
	assert.Equal(t, []int{0, exitUnknown}, d.SuccessExitCodes)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
}

func TestProjectsDiscoveryFailureIsTerminal(t *testing.T) {
	pages := projectsPages()
	pages[steveProjectsPath] = nil
	cmd, _ := projectsFixture(t, pages)
	_, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
}

func TestProjectsProjectOnAnUnlistedClusterIsAProtocolError(t *testing.T) {
	pages := projectsPages()
	pages[steveProjectsPath] = page(steveProject("c-9", "p-x", "Ghost"))
	cmd, _ := projectsFixture(t, pages)
	_, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.ErrorContains(t, err, "project c-9/p-x belongs to a cluster the cluster collection did not list")
}

var _ engine.RootClientFactory = fakeRootFactory{}

// TestProjectsNamespaceRead401IsTerminal asserts a refused credential on the
// cluster's namespace read ends the run with AUTH instead of a success-shaped
// partial answer, while a 403 stays the cluster's discovery issue.
func TestProjectsNamespaceRead401IsTerminal(t *testing.T) {
	for _, tc := range []struct {
		status   int
		terminal bool
	}{{http.StatusUnauthorized, true}, {http.StatusForbidden, false}} {
		pages := projectsPages()
		delete(pages, steveNamespacesPath("c-1"))
		cmd, _ := projectsFixture(t, pages)
		srv := steveServer(t, pages)
		status := tc.status
		mux := http.NewServeMux()
		mux.HandleFunc(steveNamespacesPath("c-1"), func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		mux.Handle("/", srv.Config.Handler)
		front := httptest.NewServer(mux)
		t.Cleanup(front.Close)
		cmd.roots = rootsOver(fakeRootClient{front}, cmd.local)
		res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
		if tc.terminal {
			require.Error(t, err, "status %d", tc.status)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			continue
		}
		require.NoError(t, err, "status %d", tc.status)
		out := decodeProjects(t, res)
		require.NotNil(t, out.Clusters[0].NamespaceDiscovery.Issue)
		assert.Equal(t, clierr.CodeRBAC, out.Clusters[0].NamespaceDiscovery.Issue.Code)
		assert.Equal(t, exitUnknown, res.ExitStatus)
	}
}

// TestProjectsMembershipCompleteNeedsTheCallerUserRead asserts a failed
// caller User read (the principal ids) leaves memberOf incomplete and
// withholds the exclusive classifications, while a directly proven
// membership stays.
func TestProjectsMembershipCompleteNeedsTheCallerUserRead(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.caller.GetErrFor = map[string]error{"u-alice": clierr.New(clierr.CodeGeneric, "user read failed")}
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	require.NotNil(t, out.MemberOf)
	assert.False(t, out.MemberOf.Complete, "a principal-held PRTB may be hidden")
	assert.Empty(t, out.MemberOf.Projects, "alice's PRTB names her GitHub principal, which the failed User read hid: no membership is proven, and none is denied")
	assert.Nil(t, out.InheritedAccess, "no exclusive classification under incomplete membership evidence")
	assert.Contains(t, res.Text, "inherited access not determined")
	pl := projectByID(t, out, "local", "p-l")
	assert.Equal(t, "", pl.Note, "no binding is never claimed under incomplete evidence")
	assert.Equal(t, 0, res.ExitStatus)
}

// TestProjectsMembershipCompleteIgnoresTheUserReadForAResolvedSubject asserts
// a --as subject, resolved by name with its principal ids, is not made
// incomplete by the user stage (which readProvenance skips).
func TestProjectsMembershipCompleteIgnoresTheUserReadForAResolvedSubject(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.impersonatedPer = map[string]*fakeClient{
		"c-1\x00u-alice": {script: []scripted{{resp: whoamiResp("u-alice")}, {resp: sar(true, "")}, {resp: sar(true, "")}, {resp: sar(false, "")}}},
		"c-2\x00u-alice": {script: []scripted{{resp: whoamiResp("u-alice")}, {resp: sar(false, "")}}},
	}
	f.caller.script = []scripted{{resp: whoamiResp("u-admin")}}
	res, err := cmd.Execute(t.Context(), projectsInput("", "alice", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	require.NotNil(t, out.MemberOf)
	assert.True(t, out.MemberOf.Complete)
	require.Len(t, out.MemberOf.Projects, 1)
	assert.Equal(t, []grant{{Kind: "RoleTemplate", Name: "project-member"}}, out.MemberOf.Projects[0].Grants)
}

// TestProjectsUnrelatedStageFailureKeepsMembershipGrants asserts a failed
// GlobalRole, CRTB or GRB read leaves a proven membership's PRTB grants in
// place, and that inherited access is classified only when the two
// inherited-binding reads succeeded: a failed GlobalRole read withholds the
// grants of entries with a GlobalRoleBinding, a failed CRTB or GRB read
// (a binding of the other kind surviving) withholds the classification
// itself, and both failing does too.
func TestProjectsUnrelatedStageFailureKeepsMembershipGrants(t *testing.T) {
	cases := map[string]struct {
		stages    []string
		inherited bool
	}{
		"globalrole failed":   {[]string{"globalroles||"}, true},
		"crtb failed":         {[]string{"clusterroletemplatebindings||"}, false},
		"grb failed":          {[]string{"globalrolebindings||"}, false},
		"crtb and grb failed": {[]string{"clusterroletemplatebindings||", "globalrolebindings||"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd, f := projectsFixture(t, projectsPages())
			f.caller.ListErrFor = map[string]error{}
			for _, st := range tc.stages {
				f.caller.ListErrFor[st] = clierr.New(clierr.CodeGeneric, "read failed")
			}
			res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
			require.NoError(t, err)
			out := decodeProjects(t, res)
			require.NotNil(t, out.MemberOf)
			assert.True(t, out.MemberOf.Complete, "the membership evidence does not depend on these reads")
			require.Len(t, out.MemberOf.Projects, 1)
			assert.Equal(t, []grant{{Kind: "RoleTemplate", Name: "project-member"}}, out.MemberOf.Projects[0].Grants)
			assert.Equal(t, "partial", out.BindingsDiscovery.Status)
			if !tc.inherited {
				assert.Nil(t, out.InheritedAccess, "a CRTB or GRB may be hidden")
				assert.Contains(t, res.Text, "inherited access not determined")
				return
			}
			require.NotNil(t, out.InheritedAccess)
			require.NotEmpty(t, out.InheritedAccess)
			for _, ia := range out.InheritedAccess {
				assert.Nil(t, ia.Grants, "the entry's GlobalRoleBinding grants come from the GlobalRole read that failed")
			}
		})
	}
}

// TestProjectsRefusedClusterKeepsTheProxyNoteWithAPRTB asserts a refused
// cluster's project keeps the proxy's note even when a PRTB names the
// subject there: no namespace was asked about, so it is no disagreement.
func TestProjectsRefusedClusterKeepsTheProxyNoteWithAPRTB(t *testing.T) {
	cmd, f := projectsFixture(t, projectsPages())
	f.clusters["c-1"] = scriptedClient(http403(""))
	res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
	require.NoError(t, err)
	out := decodeProjects(t, res)
	pa := projectByID(t, out, "c-1", "p-a")
	assert.Equal(t, visibilityNone, pa.Visibility)
	assert.Equal(t, refusedClusterNote, pa.Note)
	require.NotEmpty(t, pa.Bindings)
	assert.NotContains(t, res.Text, disagreementNote)
}

// TestProjectsVisibleWithoutABindingNamesTheKubernetesBindings asserts the
// no-binding row carries the bindings the authorizer's reasons credit when
// every allowed reason parses, none when one does not or is empty.
func TestProjectsVisibleWithoutABindingNamesTheKubernetesBindings(t *testing.T) {
	cases := map[string]struct {
		reasons []string
		want    []string
	}{
		"all parsed": {[]string{
			`RBAC: allowed by ClusterRoleBinding "crb-x" of ClusterRole "view" to User "u-alice"`,
			`RBAC: allowed by RoleBinding "rb-y/pay2" of Role "reader" to User "u-alice"`,
		}, []string{"ClusterRoleBinding crb-x", "RoleBinding pay2/rb-y"}},
		"mixed": {[]string{`RBAC: allowed by ClusterRoleBinding "crb-x" of ClusterRole "view" to User "u-alice"`, "webhook said yes"}, nil},
		"empty": {[]string{"", ""}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd, f := projectsFixture(t, projectsPages())
			// Nothing names alice on c-1: drop her CRTB, her PRTB and her
			// group alias so p-a is visible with no Rancher binding.
			f.caller.Lists["clusterroletemplatebindings||"] = listOf()
			f.caller.Lists["projectroletemplatebindings||"] = listOf()
			f.caller.Lists["globalrolebindings||"] = listOf()
			f.clusters["c-1"].script = []scripted{{resp: whoamiResp("u-alice", "system:authenticated")}, {resp: sar(true, tc.reasons[0])}, {resp: sar(true, tc.reasons[1])}, {resp: sar(false, "")}}
			res, err := cmd.Execute(t.Context(), projectsInput("", "", ""), nil)
			require.NoError(t, err)
			out := decodeProjects(t, res)
			pa := projectByID(t, out, "c-1", "p-a")
			assert.Equal(t, noBindingNote, pa.Note)
			assert.Equal(t, tc.want, pa.KubernetesBindings)
			if tc.want != nil {
				assert.Contains(t, res.Text, "c-1/p-a (ClusterRoleBinding crb-x, RoleBinding pay2/rb-y)")
			}
		})
	}
}
