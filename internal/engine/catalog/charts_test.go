package catalog

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	localIndexPrefix = "/v1/catalog.cattle.io.clusterrepos/"
	downIndexPrefix  = "/k8s/clusters/c-test/v1/catalog.cattle.io.clusterrepos/"
)

// indexOf builds a Helm repository index body from chart versions written
// as name/version/annotation triples.
func indexOf(charts map[string][]string) string {
	var parts []string
	for name, versions := range charts {
		var vs []string
		for _, v := range versions {
			vs = append(vs, `{"name":"`+name+`","version":"`+v+`","appVersion":"app-`+v+`","description":"d-`+name+`"}`)
		}
		parts = append(parts, `"`+name+`":[`+strings.Join(vs, ",")+`]`)
	}
	return `{"apiVersion":"v1","entries":{` + strings.Join(parts, ",") + `}}`
}

func runCharts(t *testing.T, clients *fakeClients, root *fakeRoot, in input) (runtime.Result, error) {
	t.Helper()
	cmd, ok := NewCharts(clients, fakeRoots{root: root}).(runtime.ReadCommand)
	require.True(t, ok)
	return cmd.Execute(t.Context(), in, nil)
}

func resultItems(t *testing.T, res runtime.Result) chartList {
	t.Helper()
	switch d := res.Data.(type) {
	case chartList:
		return d
	case deprecatedChartList:
		return chartList(d)
	}
	require.FailNow(t, "unexpected payload", "Data is %T", res.Data)
	return nil
}

func TestChartsDescribe(t *testing.T) {
	t.Parallel()
	d := NewCharts(nil, nil).Describe()
	assert.Equal(t, "charts", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)
	names := map[string]runtime.ArgSpec{}
	for _, f := range d.Flags {
		names[f.Name] = f
	}
	assert.True(t, names["cluster"].Required)
	assert.Contains(t, names, "repo")
	assert.Contains(t, names, "all-versions")
	assert.Equal(t, "bool", names["include-deprecated"].Type)
}

// TestChartsRepoPaths asserts --repo reads one index at the path the
// cluster picks: /v1/... for the local cluster and the cluster proxy for any
// other, resolved from the name the user typed, after one read of the
// ClusterRepo on that cluster.
func TestChartsRepoPaths(t *testing.T) {
	t.Parallel()
	body := indexOf(map[string][]string{"nginx": {"1.0.0"}})
	for _, tc := range []struct{ name, typed, id, want string }{
		{"local", "local", "local", localIndexPrefix + "r1?link=index"},
		{"downstream by id", "c-test", "c-test", downIndexPrefix + "r1?link=index"},
		{"downstream by name", "testcluster", "c-test", downIndexPrefix + "r1?link=index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(map[string]rootAnswer{tc.want: {status: 200, body: body}})
			clients := &fakeClients{resolve: map[string]string{"testcluster": "c-test"}, cluster: &fakeCluster{}}
			res, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": tc.typed, "repo": "r1"}})
			require.NoError(t, err)
			assert.Equal(t, []string{tc.want}, root.requests())
			assert.Equal(t, "ChartList", res.Kind)
			assert.Equal(t, chartList{{Repository: "r1", Name: "nginx", Version: "1.0.0", AppVersion: "app-1.0.0", Description: "d-nginx"}}, resultItems(t, res))
			assert.Equal(t, []string{tc.id}, clients.asked, "--repo asks once for the client of the resolved cluster")
			assert.Equal(t, []string{"r1"}, clients.cluster.gets)
		})
	}
}

// TestReadIndexEscapesTheRepositoryName asserts a repository name that is not
// a plain name stays one path segment of the index request.
func TestReadIndexEscapesTheRepositoryName(t *testing.T) {
	t.Parallel()
	want := downIndexPrefix + "a%20b%2Fc?link=index"
	root := newFakeRoot(map[string]rootAnswer{want: {status: 200, body: indexOf(nil)}})
	_, err := ReadIndex(t.Context(), root, "c-test", "a b/c")
	require.NoError(t, err)
	assert.Equal(t, []string{want}, root.requests())
}

// TestChartsWithoutRepoReadsEnabledRepositoriesInNameOrder asserts the
// ClusterRepos are listed on the resolved cluster and the index of each one
// that is not disabled is read, one after another in name order; a missing
// spec.enabled counts as enabled.
func TestChartsWithoutRepoReadsEnabledRepositoriesInNameOrder(t *testing.T) {
	t.Parallel()
	cluster := &fakeCluster{items: nil}
	cluster.items = append(cluster.items,
		repoObject("zeta", new(true)), repoObject("off", new(false)), repoObject("alpha", new(true)), repoObject("mid", nil))
	root := newFakeRoot(map[string]rootAnswer{
		downIndexPrefix + "alpha?link=index": {status: 200, body: indexOf(map[string][]string{"b-chart": {"1.0.0"}, "a-chart": {"2.0.0"}})},
		downIndexPrefix + "mid?link=index":   {status: 200, body: indexOf(map[string][]string{"m-chart": {"0.1.0"}})},
		downIndexPrefix + "zeta?link=index":  {status: 200, body: indexOf(map[string][]string{"z-chart": {"3.0.0"}})},
	})
	clients := &fakeClients{resolve: map[string]string{"testcluster": "c-test"}, cluster: cluster}
	res, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": "testcluster"}})
	require.NoError(t, err)

	assert.Equal(t, []string{downIndexPrefix + "alpha?link=index", downIndexPrefix + "mid?link=index", downIndexPrefix + "zeta?link=index"}, root.requests())
	assert.Equal(t, []string{"c-test"}, clients.asked)
	require.Len(t, cluster.gvrs, 1)
	assert.Equal(t, "clusterrepos", cluster.gvrs[0].Resource)
	assert.Equal(t, "catalog.cattle.io", cluster.gvrs[0].Group)
	assert.Equal(t, []string{""}, cluster.nss)
	assert.Empty(t, res.Warnings)

	var got []string
	for _, it := range resultItems(t, res) {
		got = append(got, it.Repository+"/"+it.Name)
	}
	assert.Equal(t, []string{"alpha/a-chart", "alpha/b-chart", "mid/m-chart", "zeta/z-chart"}, got)
}

// TestChartsLocalClusterListsThroughTheLocalIndexPath asserts the local
// cluster lists its ClusterRepos on the id "local" and reads indexes at
// /v1/....
func TestChartsLocalClusterListsThroughTheLocalIndexPath(t *testing.T) {
	t.Parallel()
	cluster := &fakeCluster{items: nil}
	cluster.items = append(cluster.items, repoObject("r1", nil))
	root := newFakeRoot(map[string]rootAnswer{localIndexPrefix + "r1?link=index": {status: 200, body: indexOf(nil)}})
	clients := &fakeClients{cluster: cluster}
	_, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": "local"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"local"}, clients.asked)
	assert.Equal(t, []string{localIndexPrefix + "r1?link=index"}, root.requests())
}

// TestChartsNoRepositories asserts a cluster with no ClusterRepo, and one
// whose ClusterRepos are all disabled, is an empty list with no error, no
// warning and no index read.
func TestChartsNoRepositories(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		items []*fakeRepo
	}{
		{"none", nil},
		{"all disabled", []*fakeRepo{{"r1", new(false)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := &fakeCluster{}
			for _, r := range tc.items {
				cluster.items = append(cluster.items, repoObject(r.name, r.enabled))
			}
			root := newFakeRoot(nil)
			res, err := runCharts(t, &fakeClients{cluster: cluster}, root, input{s: map[string]string{"cluster": "c-test"}})
			require.NoError(t, err)
			assert.Equal(t, chartList{}, resultItems(t, res))
			assert.NotNil(t, resultItems(t, res))
			assert.Empty(t, res.Warnings)
			assert.Empty(t, root.requests())
		})
	}
}

type fakeRepo struct {
	name    string
	enabled *bool
}

// TestChartsDefaultVersionIsTheFirstServedRelease asserts one line per chart
// shows the first version, in the order Rancher serves, that is not a
// prerelease: the one after a leading prerelease, the first one for a chart
// with only prereleases, and a version with a leading zero in a number or
// fewer than three numbers counted as a release.
func TestChartsDefaultVersionIsTheFirstServedRelease(t *testing.T) {
	t.Parallel()
	path := downIndexPrefix + "r1?link=index"
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: indexOf(map[string][]string{
		"lead":   {"2.0.0-rc.1", "1.10.0", "1.9.0"},
		"pre":    {"1.0.0-rc.2", "1.0.0-rc.10"},
		"zeros":  {"5.4.001", "5.4.000", "3.0.601"},
		"short":  {"v3.1908", "v3.1907"},
		"build":  {"1.0.0+up1.2-rc", "0.9.0"},
		"suffix": {"3.1908-rc1", "1.2-beta"},
	})}})
	res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root, input{s: map[string]string{"cluster": "c-test", "repo": "r1"}})
	require.NoError(t, err)
	got := map[string]string{}
	for _, it := range resultItems(t, res) {
		got[it.Name] = it.Version
	}
	assert.Equal(t, map[string]string{
		"lead": "1.10.0", "pre": "1.0.0-rc.2", "zeros": "5.4.001", "short": "v3.1908",
		"build": "1.0.0+up1.2-rc", "suffix": "3.1908-rc1",
	}, got)
}

// TestChartsAllVersionsKeepsTheServedOrder asserts --all-versions lists
// charts in name order and each chart's versions as Rancher serves them,
// whatever the version text, and that no version is dropped or reordered.
func TestChartsAllVersionsKeepsTheServedOrder(t *testing.T) {
	t.Parallel()
	path := downIndexPrefix + "r1?link=index"
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: indexOf(map[string][]string{
		"b": {"1.0.0"},
		"a": {"5.4.001", "v3.1908", "1.0.0-rc.1", "106.0.2+up0.6.0", "106.0.2+up0.5.1", "3.14.005", "1.9.0", "1.10.0"},
	})}})
	res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root,
		input{s: map[string]string{"cluster": "c-test", "repo": "r1"}, b: map[string]bool{"all-versions": true}})
	require.NoError(t, err)
	var got []string
	for _, it := range resultItems(t, res) {
		got = append(got, it.Name+" "+it.Version)
	}
	assert.Equal(t, []string{
		"a 5.4.001", "a v3.1908", "a 1.0.0-rc.1", "a 106.0.2+up0.6.0", "a 106.0.2+up0.5.1", "a 3.14.005", "a 1.9.0", "a 1.10.0", "b 1.0.0",
	}, got)
}

const deprecatedIndex = `{"entries":{
  "flag":[{"version":"2.0.0","deprecated":true},{"version":"1.0.0"}],
  "note":[{"version":"2.0.0","annotations":{"catalog.cattle.io/deprecated":"true"}},{"version":"1.0.0"}],
  "notrue":[{"version":"2.0.0","annotations":{"catalog.cattle.io/deprecated":"false"}}],
  "all":[{"version":"2.0.0","deprecated":true},{"version":"1.0.0","annotations":{"catalog.cattle.io/deprecated":"true"}}],
  "none":[{"version":"1.0.0"}]}}`

// TestChartsLeaveOutDeprecatedVersions asserts a version is deprecated when
// its deprecated field is true or its annotation is "true"; that such
// versions are left out before the default version is picked, so a chart
// shows its newest version that is not deprecated; that a chart with only
// deprecated versions has no line; and that no line is marked deprecated.
func TestChartsLeaveOutDeprecatedVersions(t *testing.T) {
	t.Parallel()
	path := localIndexPrefix + "r1?link=index"
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: deprecatedIndex}})
	for _, all := range []bool{false, true} {
		res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root,
			input{s: map[string]string{"cluster": "local", "repo": "r1"}, b: map[string]bool{"all-versions": all}})
		require.NoError(t, err)
		assert.IsType(t, chartList{}, res.Data)
		var got []string
		for _, it := range resultItems(t, res) {
			assert.False(t, it.Deprecated)
			got = append(got, it.Name+" "+it.Version)
		}
		assert.Equal(t, []string{"flag 1.0.0", "none 1.0.0", "note 1.0.0", "notrue 2.0.0"}, got, "all-versions %v", all)
	}
}

// TestChartsIncludeDeprecatedListsThem asserts --include-deprecated lists the
// deprecated versions, marked, runs the default pick over every version, and
// adds a DEPRECATED column to the table.
func TestChartsIncludeDeprecatedListsThem(t *testing.T) {
	t.Parallel()
	path := localIndexPrefix + "r1?link=index"
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: deprecatedIndex}})
	res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root,
		input{s: map[string]string{"cluster": "local", "repo": "r1"}, b: map[string]bool{"include-deprecated": true}})
	require.NoError(t, err)
	tab, ok := res.Data.(deprecatedChartList)
	require.True(t, ok, "Data is %T", res.Data)
	assert.Equal(t, []string{"REPOSITORY", "NAME", "VERSION", "APP VERSION", "DESCRIPTION", "DEPRECATED"}, tab.Columns())
	var got []string
	for i, it := range resultItems(t, res) {
		got = append(got, it.Name+" "+it.Version+" "+strconv.FormatBool(it.Deprecated))
		assert.Equal(t, strconv.FormatBool(it.Deprecated), tab.Rows()[i][5])
	}
	assert.Equal(t, []string{"all 2.0.0 true", "flag 2.0.0 true", "none 1.0.0 false", "note 2.0.0 true", "notrue 2.0.0 false"}, got)

	res, err = runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root,
		input{s: map[string]string{"cluster": "local", "repo": "r1"}, b: map[string]bool{"include-deprecated": true, "all-versions": true}})
	require.NoError(t, err)
	assert.Len(t, resultItems(t, res), 8)
}

// TestChartsHidden asserts hidden is set when the annotation is present and
// not empty, whatever its value, and unset when it is absent or empty.
func TestChartsHidden(t *testing.T) {
	t.Parallel()
	path := localIndexPrefix + "r1?link=index"
	body := `{"entries":{
	  "yes":[{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":"true"}}],
	  "falsetext":[{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":"false"}}],
	  "empty":[{"version":"1.0.0","annotations":{"catalog.cattle.io/hidden":""}}],
	  "absent":[{"version":"1.0.0","annotations":{"other":"x"}}],
	  "none":[{"version":"1.0.0"}]}}`
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: body}})
	res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root, input{s: map[string]string{"cluster": "local", "repo": "r1"}})
	require.NoError(t, err)
	got := map[string]bool{}
	for _, it := range resultItems(t, res) {
		got[it.Name] = it.Hidden
	}
	assert.Equal(t, map[string]bool{"yes": true, "falsetext": true, "empty": false, "absent": false, "none": false}, got)
}

// TestChartsKeepsServerTextUnchanged asserts the payload carries the
// server's text as decoded: the table writer removes unsafe runes, the
// command does not.
func TestChartsKeepsServerTextUnchanged(t *testing.T) {
	t.Parallel()
	path := localIndexPrefix + "r1?link=index"
	body := `{"entries":{"c":[{"version":"1.0.0","description":"a\u001b[31mred\u0007"}]}}`
	root := newFakeRoot(map[string]rootAnswer{path: {status: 200, body: body}})
	res, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root, input{s: map[string]string{"cluster": "local", "repo": "r1"}})
	require.NoError(t, err)
	assert.Equal(t, "a\x1b[31mred\a", resultItems(t, res)[0].Description)
}

// TestChartsUnreadableRepositoryIsAWarning asserts that without --repo a
// repository that cannot be read is left out with one warning naming it,
// and the others are listed: a 403, a 401, a 503 and one with no stored
// index.
func TestChartsUnreadableRepositoryIsAWarning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer rootAnswer
		want   string
	}{
		{"forbidden", rootAnswer{status: http.StatusForbidden}, "refused"},
		{"unauthorized", rootAnswer{status: http.StatusUnauthorized}, "refused to serve its index"},
		{"service unavailable", rootAnswer{status: http.StatusServiceUnavailable}, "unable to handle the request"},
		{"no stored index", rootAnswer{status: http.StatusInternalServerError, body: `{"type":"error","status":500,"code":"ServerError","message":"configmaps \"\" not found"}`}, "configmaps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := &fakeCluster{}
			cluster.items = append(cluster.items, repoObject("bad", nil), repoObject("good", nil))
			root := newFakeRoot(map[string]rootAnswer{
				downIndexPrefix + "bad?link=index":  tc.answer,
				downIndexPrefix + "good?link=index": {status: 200, body: indexOf(map[string][]string{"nginx": {"1.0.0"}})},
			})
			res, err := runCharts(t, &fakeClients{cluster: cluster}, root, input{s: map[string]string{"cluster": "c-test"}})
			require.NoError(t, err)
			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], "ClusterRepo 'bad'")
			assert.Contains(t, res.Warnings[0], tc.want)
			assert.NotContains(t, res.Warnings[0], "good")
			items := resultItems(t, res)
			require.Len(t, items, 1)
			assert.Equal(t, "good", items[0].Repository)
			assert.Len(t, root.requests(), 2)
		})
	}
}

// TestChartsRepoFailureIsTheError asserts a failed --repo read is the
// command's error with the status table's code, naming the repository.
func TestChartsRepoFailureIsTheError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		answer    rootAnswer
		code      string
		retryable bool
	}{
		{"forbidden", rootAnswer{status: http.StatusForbidden}, clierr.CodeRBAC, false},
		{"unauthorized", rootAnswer{status: http.StatusUnauthorized}, clierr.CodeGeneric, false},
		{"service unavailable", rootAnswer{status: http.StatusServiceUnavailable}, clierr.CodeGeneric, true},
		{"no stored index", rootAnswer{status: http.StatusInternalServerError, body: `{"type":"error","status":500,"code":"ServerError","message":"configmaps \"\" not found"}`}, clierr.CodeGeneric, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(map[string]rootAnswer{downIndexPrefix + "bad?link=index": tc.answer})
			_, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root, input{s: map[string]string{"cluster": "c-test", "repo": "bad"}})
			require.ErrorContains(t, err, "ClusterRepo 'bad'")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.retryable, ce.Retryable)
		})
	}
}

// TestChartsMissingRepoIsNotFoundWithTheListCommand asserts a missing
// ClusterRepo is NOT_FOUND whose remedy lists the repositories of the
// cluster as the user typed it, shell-quoted.
func TestChartsMissingRepoIsNotFoundWithTheListCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ typed, id, hint string }{
		{"testcluster", "c-test", clierr.Rancher + " get clusterrepos --cluster 'testcluster'"},
		{"local", "local", clierr.Rancher + " get clusterrepos --cluster 'local'"},
		{"my 'odd' name", "c-test", clierr.Rancher + " get clusterrepos --cluster 'my '\\''odd'\\'' name'"},
	} {
		t.Run(tc.typed, func(t *testing.T) {
			t.Parallel()
			root := newFakeRoot(nil)
			cluster := &fakeCluster{missing: []string{"nope"}}
			clients := &fakeClients{resolve: map[string]string{tc.typed: tc.id}, cluster: cluster}
			_, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": tc.typed, "repo": "nope"}})
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, tc.hint, ce.Hint)
			assert.Equal(t, "ClusterRepo 'nope' not found", ce.Message)
			assert.Equal(t, []string{"nope"}, cluster.gets)
			assert.Empty(t, root.requests(), "a missing ClusterRepo reads no index")
		})
	}
}

// TestChartsRepoReadsTheClusterRepoFirst asserts --repo reads the ClusterRepo
// before its index: a disabled one is refused with VALIDATION, the command that
// shows it and no index read; one with no spec.enabled and an enabled one are
// read as enabled.
func TestChartsRepoReadsTheClusterRepoFirst(t *testing.T) {
	t.Parallel()
	body := indexOf(map[string][]string{"nginx": {"1.0.0"}})
	for _, tc := range []struct {
		name    string
		enabled *bool
		refused bool
	}{
		{"disabled", new(false), true},
		{"enabled", new(true), false},
		{"no spec.enabled", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := &fakeCluster{items: []*unstructured.Unstructured{repoObject("r1", tc.enabled)}}
			root := newFakeRoot(map[string]rootAnswer{downIndexPrefix + "r1?link=index": {status: 200, body: body}})
			clients := &fakeClients{resolve: map[string]string{"testcluster": "c-test"}, cluster: cluster}
			res, err := runCharts(t, clients, root, input{s: map[string]string{"cluster": "testcluster", "repo": "r1"}})
			assert.Equal(t, []string{"r1"}, cluster.gets)
			if !tc.refused {
				require.NoError(t, err)
				assert.Equal(t, []string{downIndexPrefix + "r1?link=index"}, root.requests())
				assert.Len(t, resultItems(t, res), 1)
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.False(t, ce.Retryable)
			assert.Equal(t, "ClusterRepo 'r1' is disabled", ce.Message)
			assert.Equal(t, clierr.Rancher+" get clusterrepo 'r1' --cluster 'testcluster'", ce.Hint)
			assert.Empty(t, root.requests(), "a disabled ClusterRepo reads no index")
		})
	}
}

// TestChartsRepoReadFailureIsTheError asserts a failed read of the
// ClusterRepo is the command's error, with no index read.
func TestChartsRepoReadFailureIsTheError(t *testing.T) {
	t.Parallel()
	boom := clierr.New(clierr.CodeRBAC, "the server refused get clusterrepos")
	root := newFakeRoot(nil)
	_, err := runCharts(t, &fakeClients{cluster: &fakeCluster{err: boom}}, root, input{s: map[string]string{"cluster": "c-test", "repo": "r1"}})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, root.requests())
}

// TestChartsClusterRepoListFailureIsTheError asserts a failed ClusterRepo
// list is the command's error, with no index read.
func TestChartsClusterRepoListFailureIsTheError(t *testing.T) {
	t.Parallel()
	boom := clierr.New(clierr.CodeRBAC, "the server refused list clusterrepos")
	root := newFakeRoot(nil)
	_, err := runCharts(t, &fakeClients{cluster: &fakeCluster{err: boom}}, root, input{s: map[string]string{"cluster": "c-test"}})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, root.requests())
}

// TestChartsMissingClusterIsRefused asserts a command with no --cluster
// reads nothing.
func TestChartsMissingClusterIsRefused(t *testing.T) {
	t.Parallel()
	root := newFakeRoot(nil)
	_, err := runCharts(t, &fakeClients{cluster: &fakeCluster{}}, root, input{})
	require.Error(t, err)
	assert.Empty(t, root.requests())
}

var _ engine.ClusterClientFactory = (*fakeClients)(nil)
