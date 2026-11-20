package catalog

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// listCluster is the local cluster as extension list sees it: objects by
// resource and name, lists by resource and namespace, and the errors to answer.
type listCluster struct {
	engine.ClusterClient
	objects map[string]*unstructured.Unstructured
	lists   map[string][]*unstructured.Unstructured
	errs    map[string]error
	calls   []string
}

func (c *listCluster) Get(_ context.Context, gvr schema.GroupVersionResource, _, name string) (*unstructured.Unstructured, error) {
	key := gvr.Resource + "/" + name
	c.calls = append(c.calls, "get "+key)
	if err, ok := c.errs[key]; ok {
		return nil, err
	}
	if obj, ok := c.objects[key]; ok {
		return obj, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, key+" not found")
}

func (c *listCluster) List(_ context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	key := gvr.Resource + "@" + ns
	c.calls = append(c.calls, "list "+key)
	if err, ok := c.errs[key]; ok {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	for _, it := range c.lists[key] {
		list.Items = append(list.Items, *it)
	}
	return list, nil
}

// listClients is the factory of the list command: the local cluster, the
// server's version and its edition.
type listClients struct {
	engine.ClusterClientFactory
	cluster *listCluster
	version string
	edition engine.Edition
}

func (f *listClients) Cluster(context.Context, string) (engine.ClusterClient, error) {
	return f.cluster, nil
}
func (f *listClients) ServerVersion(context.Context) (string, error)         { return f.version, nil }
func (f *listClients) ServerEdition(context.Context) (engine.Edition, error) { return f.edition, nil }

const (
	stevePrefix  = "/v1/"
	reposPath    = "/v1/catalog.cattle.io.clusterrepos"
	localCluster = "/v1/management.cattle.io.clusters/local"
)

// extensionRepoIndex is the index of the repository the list tests read.
const extensionRepoIndex = `{"apiVersion":"v1","entries":{
  "alpha":[
    {"name":"alpha","version":"2.0.0","appVersion":"2.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/display-name":"Alpha extension","catalog.cattle.io/ui-extensions-version":">= 3.0.0"}},
    {"name":"alpha","version":"1.0.0","appVersion":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 4.0.0"}}],
  "beta":[
    {"name":"beta","version":"1.0.0","appVersion":"10","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0","catalog.cattle.io/kube-version":">= 1.40.0-0"}}],
  "primeonly":[
    {"name":"primeonly","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0","catalog.cattle.io/prime-only":"true"}}],
  "hidden":[
    {"name":"hidden","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/hidden":"true","catalog.cattle.io/ui-extensions-version":">= 3.0.0"}}],
  "plain":[
    {"name":"plain","version":"1.0.0"}]}}`

func repoItemBody(name, state string, extra string) string {
	return `{"metadata":{"name":"` + name + `","state":{"name":"` + state + `"}` + extra + `},"spec":{}}`
}

func collectionBody(items ...string) string {
	n := len(items)
	if n == 0 {
		return `{"data":[],"revision":"1"}`
	}
	return `{"data":[` + strings.Join(items, ",") + `],"count":` + string(rune('0'+n)) + `,"revision":"1"}`
}

func clusterBody(info, gitVersion string) string {
	status := `{}`
	switch {
	case info != "" && gitVersion != "":
		status = `{"info":{"kubernetesVersion":"` + info + `"},"version":{"gitVersion":"` + gitVersion + `"}}`
	case info != "":
		status = `{"info":{"kubernetesVersion":"` + info + `"}}`
	case gitVersion != "":
		status = `{"version":{"gitVersion":"` + gitVersion + `"}}`
	}
	return `{"id":"local","status":` + status + `}`
}

func enabledFeature(value bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "uiextension"}, "spec": map[string]any{"value": value}}}
}

func extensionNamespaceObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "cattle-ui-plugin-system"}}}
}

func uiPlugin(ns, name, plugin, version, cacheState string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.cattle.io/v1", "kind": "UIPlugin",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"plugin": map[string]any{"name": plugin, "version": version}},
	}}
	if cacheState != "" {
		obj.Object["status"] = map[string]any{"cacheState": cacheState}
	}
	return obj
}

func extensionApp(name, annotationRepo, labelRepo string) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "cattle-ui-plugin-system"}
	if labelRepo != "" {
		meta["labels"] = map[string]any{"catalog.cattle.io/cluster-repo-name": labelRepo}
	}
	spec := map[string]any{}
	if annotationRepo != "" {
		spec["chart"] = map[string]any{"metadata": map[string]any{"annotations": map[string]any{"catalog.cattle.io/ui-source-repo": annotationRepo}}}
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "App", "metadata": meta, "spec": spec}}
}

// listScenario is the server the list tests run against: an enabled feature,
// the namespace, a community Rancher v2.15.3 on Kubernetes v1.36.5, and one
// active repository with extensionRepoIndex.
type listScenario struct {
	cluster *listCluster
	clients *listClients
	answers map[string]rootAnswer
}

func newListScenario() *listScenario {
	cluster := &listCluster{
		objects: map[string]*unstructured.Unstructured{
			"features/uiextension":               enabledFeature(true),
			"namespaces/cattle-ui-plugin-system": extensionNamespaceObject(),
		},
		lists: map[string][]*unstructured.Unstructured{},
		errs:  map[string]error{},
	}
	return &listScenario{
		cluster: cluster,
		clients: &listClients{cluster: cluster, version: "v2.15.3", edition: engine.EditionCommunity},
		answers: map[string]rootAnswer{
			reposPath:    {status: 200, body: collectionBody(repoItemBody("partner-extensions", "active", ""))},
			localCluster: {status: 200, body: clusterBody("v1.36.5+k3s1", "v1.36.5+k3s1")},
			reposPath + "/partner-extensions?link=index": {status: 200, body: extensionRepoIndex},
		},
	}
}

func (s *listScenario) run(t *testing.T, show string) (runtime.Result, *fakeRoot, error) {
	t.Helper()
	root := newFakeRoot(s.answers)
	cmd, ok := NewExtensionList(s.clients, fakeRoots{root: root}).(runtime.ReadCommand)
	require.True(t, ok)
	res, err := cmd.Execute(t.Context(), input{s: map[string]string{"show": show}}, nil)
	return res, root, err
}

func listRows(t *testing.T, res runtime.Result) extensionList {
	t.Helper()
	l, ok := res.Data.(extensionList)
	require.True(t, ok, "payload is %T", res.Data)
	return l
}

func byName(l extensionList, name string) extensionItem {
	for _, it := range l {
		if it.Name == name {
			return it
		}
	}
	return extensionItem{}
}

// TestExtensionListRows asserts the rows of one repository: hidden extension
// charts and ordinary charts are left out, the newest installable version is
// the first that passes every check, and each version carries its checks and
// the reason of the first failure.
func TestExtensionListRows(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	res, _, err := s.run(t, "")
	require.NoError(t, err)
	assert.Equal(t, "ExtensionList", res.Kind)
	rows := listRows(t, res)
	var names []string
	for _, it := range rows {
		names = append(names, it.Name)
	}
	assert.Equal(t, []string{"alpha", "beta", "primeonly"}, names)

	alpha := byName(rows, "alpha")
	assert.Equal(t, "Alpha extension", alpha.DisplayName)
	assert.Equal(t, "partner-extensions", alpha.Repo)
	assert.Equal(t, "2.0.0", alpha.Installable)
	assert.Empty(t, alpha.Installed)
	assert.Empty(t, alpha.Upgrade)
	require.Len(t, alpha.Versions, 2)
	assert.True(t, alpha.Versions[0].Installable)
	assert.Empty(t, alpha.Versions[0].Reason)
	assert.False(t, alpha.Versions[1].Installable)
	assert.Equal(t, "needs extensions API >= 4.0.0; this Rancher's UI offers 3.0.12", alpha.Versions[1].Reason)
	require.Len(t, alpha.Versions[1].Checks, 1, "the checks stop at the first failure")
	assert.Equal(t, "failed", alpha.Versions[1].Checks[0].Result)

	beta := byName(rows, "beta")
	assert.Equal(t, "beta", beta.DisplayName)
	assert.Empty(t, beta.Installable)
	assert.Equal(t, "needs Kubernetes >= 1.40.0-0; the local cluster runs v1.36.5", beta.Versions[0].Reason)

	primeOnly := byName(rows, "primeonly")
	assert.True(t, primeOnly.PrimeOnly)
	assert.Equal(t, "needs Rancher Prime", primeOnly.Versions[0].Reason)
	assert.Empty(t, res.Warnings[:0])
}

// TestExtensionListColumns asserts the table: its header and the PRIME-ONLY cell.
func TestExtensionListColumns(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	res, _, err := s.run(t, "")
	require.NoError(t, err)
	rows := listRows(t, res)
	assert.Equal(t, []string{"NAME", "DISPLAY-NAME", "REPO", "INSTALLED", "INSTALLABLE", "UPGRADE", "PRIME-ONLY", "STATE"}, rows.Columns())
	for i, it := range rows {
		cells := rows.Rows()[i]
		assert.Equal(t, it.Name, cells[0])
		if it.PrimeOnly {
			assert.Equal(t, "yes", cells[6])
		} else {
			assert.Empty(t, cells[6])
		}
	}
}

// TestExtensionListInstalledAndUpgrade asserts the pairing cases: a UIPlugin
// and its App (installed, upgrade offered by the app version rule), an App
// with no UIPlugin (not installed), a UIPlugin with no App (installed with no
// repository), and --show filtering.
func TestExtensionListInstalledAndUpgrade(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	s.cluster.lists["uiplugins@"] = []*unstructured.Unstructured{
		uiPlugin("cattle-ui-plugin-system", "alpha", "alpha", "1.0.0", "cached"),
		uiPlugin("other", "orphan", "orphan", "0.5.0", ""),
	}
	s.cluster.lists["apps@cattle-ui-plugin-system"] = []*unstructured.Unstructured{
		extensionApp("alpha", "partner-extensions", ""),
		extensionApp("beta", "", "partner-extensions"),
	}
	res, _, err := s.run(t, "all")
	require.NoError(t, err)
	rows := listRows(t, res)

	alpha := byName(rows, "alpha")
	assert.Equal(t, "1.0.0", alpha.Installed)
	assert.Equal(t, "2.0.0", alpha.Upgrade)
	assert.Equal(t, "cached", alpha.State)

	beta := byName(rows, "beta")
	assert.Empty(t, beta.Installed, "an App with no UIPlugin is not installed")
	assert.Equal(t, "app without a UIPlugin", beta.State)
	assert.Empty(t, beta.Upgrade)

	orphan := byName(rows, "orphan")
	assert.Equal(t, "0.5.0", orphan.Installed)
	assert.Empty(t, orphan.Repo)
	assert.Empty(t, orphan.Versions)

	installed, _, err := s.run(t, "installed")
	require.NoError(t, err)
	var got []string
	for _, it := range listRows(t, installed) {
		got = append(got, it.Name)
	}
	assert.Equal(t, []string{"alpha", "orphan"}, got)

	available, _, err := s.run(t, "available")
	require.NoError(t, err)
	got = nil
	for _, it := range listRows(t, available) {
		got = append(got, it.Name)
	}
	assert.Equal(t, []string{"beta", "primeonly"}, got)
}

// TestExtensionListUpgradeFollowsTheAppVersion asserts UPGRADE compares the
// UIPlugin's version with the newest installable version's appVersion, and
// shows the version label with the chart version in parentheses.
func TestExtensionListUpgradeFollowsTheAppVersion(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	s.answers[reposPath+"/partner-extensions?link=index"] = rootAnswer{status: 200, body: `{"entries":{"gamma":[
	  {"name":"gamma","version":"1.2.0","appVersion":"3.1.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0"}}]}}`}
	s.cluster.lists["uiplugins@"] = []*unstructured.Unstructured{uiPlugin("cattle-ui-plugin-system", "gamma", "gamma", "3.1.0", "")}
	s.cluster.lists["apps@cattle-ui-plugin-system"] = []*unstructured.Unstructured{extensionApp("gamma", "partner-extensions", "")}
	res, _, err := s.run(t, "")
	require.NoError(t, err)
	gamma := byName(listRows(t, res), "gamma")
	assert.Equal(t, "3.1.0 (1.2.0)", gamma.Installable)
	assert.Empty(t, gamma.Upgrade, "the installed version equals the app version, not the chart version")

	s.cluster.lists["uiplugins@"] = []*unstructured.Unstructured{uiPlugin("cattle-ui-plugin-system", "gamma", "gamma", "1.2.0", "")}
	res, _, err = s.run(t, "")
	require.NoError(t, err)
	assert.Equal(t, "3.1.0 (1.2.0)", byName(listRows(t, res), "gamma").Upgrade, "the chart version is not what the UIPlugin reports")
}

// TestExtensionListTwoRepositoriesOneChart asserts the pairing by server
// version: on Rancher v2.15 the App's repository holds the installed row and
// on Rancher v2.14 the first repository in name order does.
func TestExtensionListTwoRepositoriesOneChart(t *testing.T) {
	t.Parallel()
	body := `{"entries":{"alpha":[{"name":"alpha","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0"}}]}}`
	for _, tc := range []struct{ version, installedRepo string }{{"v2.15.3", "rancher-ui-plugins"}, {"v2.14.7", "partner-extensions"}} {
		s := newListScenario()
		s.clients.version = tc.version
		s.clients.edition = engine.EditionPrime
		s.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemBody("partner-extensions", "active", ""), repoItemBody("rancher-ui-plugins", "active", ""))}
		s.answers[reposPath+"/partner-extensions?link=index"] = rootAnswer{status: 200, body: body}
		s.answers[reposPath+"/rancher-ui-plugins?link=index"] = rootAnswer{status: 200, body: body}
		s.cluster.lists["uiplugins@"] = []*unstructured.Unstructured{uiPlugin("cattle-ui-plugin-system", "alpha", "alpha", "1.0.0", "")}
		s.cluster.lists["apps@cattle-ui-plugin-system"] = []*unstructured.Unstructured{extensionApp("alpha", "rancher-ui-plugins", "")}
		res, _, err := s.run(t, "")
		require.NoError(t, err, tc.version)
		var installed, available []string
		for _, it := range listRows(t, res) {
			if it.Installed != "" {
				installed = append(installed, it.Repo)
			} else {
				available = append(available, it.Repo)
			}
		}
		assert.Equal(t, []string{tc.installedRepo}, installed, tc.version)
		assert.Len(t, available, 1, tc.version)
	}
}

// TestExtensionListSharedChecks asserts the refusals made before anything is
// listed, in order: the feature turned off (first, whatever the namespace
// says), the namespace missing, and a refused read as RBAC. A missing Feature
// proceeds, and spec.value wins over status.default.
func TestExtensionListSharedChecks(t *testing.T) {
	t.Parallel()
	missingNamespace := func(s *listScenario) { delete(s.cluster.objects, "namespaces/cattle-ui-plugin-system") }
	tests := []struct {
		name    string
		mutate  func(*listScenario)
		code    string
		message string
	}{
		{"feature off", func(s *listScenario) { s.cluster.objects["features/uiextension"] = enabledFeature(false) }, clierr.CodeValidation,
			"UI extensions are turned off on this server (feature uiextension); turning the feature on takes effect after Rancher restarts"},
		{"feature off beats the namespace", func(s *listScenario) {
			s.cluster.objects["features/uiextension"] = enabledFeature(false)
			missingNamespace(s)
		}, clierr.CodeValidation, "UI extensions are turned off on this server (feature uiextension); turning the feature on takes effect after Rancher restarts"},
		{"status default false", func(s *listScenario) {
			s.cluster.objects["features/uiextension"] = &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"default": false}}}
		}, clierr.CodeValidation, "UI extensions are turned off on this server (feature uiextension); turning the feature on takes effect after Rancher restarts"},
		{"namespace missing", missingNamespace, clierr.CodeNotFound,
			"namespace cattle-ui-plugin-system not found; Rancher creates it when the features uiextension and MCM are on"},
		{"namespace forbidden", func(s *listScenario) {
			s.cluster.errs["namespaces/cattle-ui-plugin-system"] = clierr.New(clierr.CodeRBAC, "forbidden")
		}, clierr.CodeRBAC, "forbidden"},
		{"feature forbidden", func(s *listScenario) {
			s.cluster.errs["features/uiextension"] = clierr.New(clierr.CodeRBAC, "forbidden")
		}, clierr.CodeRBAC, "forbidden"},
	}
	for _, tt := range tests {
		s := newListScenario()
		tt.mutate(s)
		_, root, err := s.run(t, "")
		require.Error(t, err, tt.name)
		ce := codeOf(t, err)
		assert.Equal(t, tt.code, ce.Code, tt.name)
		assert.Equal(t, tt.message, ce.Message, tt.name)
		assert.Empty(t, root.requests(), tt.name+": nothing is read after a refusal")
	}

	s := newListScenario()
	delete(s.cluster.objects, "features/uiextension")
	_, _, err := s.run(t, "")
	require.NoError(t, err, "a missing Feature counts as on")

	s = newListScenario()
	s.cluster.objects["features/uiextension"] = &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"value": true}, "status": map[string]any{"default": false}}}
	_, _, err = s.run(t, "")
	require.NoError(t, err, "spec.value true wins over status.default false")
}

// TestExtensionListRepositories asserts which ClusterRepos are read: hidden
// and disabled ones are not read at all, a repository that is not active is
// skipped with a warning naming its state, a 503 index is "still downloading",
// and another index error skips the repository with its message.
func TestExtensionListRepositories(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	s.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
		repoItemBody("hidden-repo", "active", `,"annotations":{"clusterrepo.cattle.io/hidden":"true"}`),
		`{"metadata":{"name":"disabled-repo","state":{"name":"active"}},"spec":{"enabled":false}}`,
		repoItemBody("broken", "error", ""),
		repoItemBody("inactive", "inactive", ""),
		repoItemBody("loading", "active", ""),
		repoItemBody("failing", "active", ""),
		repoItemBody("partner-extensions", "active", ""),
	)}
	s.answers[reposPath+"/loading?link=index"] = rootAnswer{status: http.StatusServiceUnavailable}
	s.answers[reposPath+"/failing?link=index"] = rootAnswer{status: http.StatusInternalServerError, body: "boom"}
	res, root, err := s.run(t, "")
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, "skipped ClusterRepo 'broken': its state is error")
	assert.Contains(t, res.Warnings, "skipped ClusterRepo 'inactive': its state is inactive")
	assert.Contains(t, res.Warnings, "skipped ClusterRepo 'loading': Rancher is still downloading it")
	var failing bool
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "skipped ") && strings.Contains(w, "failing") {
			failing = true
		}
	}
	assert.True(t, failing, "an index error skips its repository with a warning: %v", res.Warnings)
	for _, p := range root.requests() {
		assert.NotContains(t, p, "hidden-repo")
		assert.NotContains(t, p, "disabled-repo")
		assert.NotContains(t, p, "broken")
		assert.NotContains(t, p, "inactive?")
	}
	assert.Len(t, listRows(t, res), 3, "the readable repository still lists its extensions")
}

// TestExtensionListRequestCount asserts the requests of a run with N
// repositories, in order: the ClusterRepo collection, the local cluster, then
// one index each, and no index twice.
func TestExtensionListRequestCount(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	s.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemBody("partner-extensions", "active", ""), repoItemBody("rancher-ui-plugins", "active", ""))}
	s.answers[reposPath+"/rancher-ui-plugins?link=index"] = rootAnswer{status: 200, body: `{"entries":{}}`}
	_, root, err := s.run(t, "")
	require.NoError(t, err)
	assert.Equal(t, []string{localCluster, reposPath, reposPath + "/partner-extensions?link=index", reposPath + "/rancher-ui-plugins?link=index"}, root.requests())
}

// TestExtensionListWarnings asserts each warning and the case where it must
// not appear.
func TestExtensionListWarnings(t *testing.T) {
	t.Parallel()
	const kubeWarning = "the Kubernetes version of the local cluster could not be read; versions that set kube-version are not installable"

	s := newListScenario()
	s.answers[localCluster] = rootAnswer{status: 200, body: clusterBody("", "")}
	res, _, err := s.run(t, "")
	require.NoError(t, err)
	assert.Contains(t, res.Warnings, kubeWarning)
	assert.Equal(t, "the Kubernetes version of the local cluster could not be read", byName(listRows(t, res), "beta").Versions[0].Reason)

	s = newListScenario()
	s.answers[localCluster] = rootAnswer{status: 403, body: "{}"}
	res, _, err = s.run(t, "")
	require.NoError(t, err, "a refused read of the cluster is a warning, never a refusal")
	assert.Contains(t, res.Warnings, kubeWarning)

	s = newListScenario()
	s.answers[localCluster] = rootAnswer{status: 200, body: clusterBody("", "")}
	s.answers[reposPath+"/partner-extensions?link=index"] = rootAnswer{status: 200, body: `{"entries":{"x":[{"name":"x","version":"1.0.0","annotations":{"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 3.0.0"}}]}}`}
	res, _, err = s.run(t, "")
	require.NoError(t, err)
	assert.NotContains(t, res.Warnings, kubeWarning, "no version sets kube-version")

	s = newListScenario()
	s.clients.version = "v2.99.0"
	res, _, err = s.run(t, "")
	require.NoError(t, err)
	var notKnown string
	for _, w := range res.Warnings {
		if strings.HasPrefix(w, "the extensions API version of Rancher v2.99.0 is not known") {
			notKnown = w
		}
	}
	assert.Contains(t, notKnown, "ui-extensions-version >= 3.0.0, >= 4.0.0 was not checked")
	assert.Contains(t, notKnown, "ui-offline-preferred")
	assert.Equal(t, "notChecked", byName(listRows(t, res), "alpha").Versions[0].Checks[0].Result)
	assert.Equal(t, "2.0.0", byName(listRows(t, res), "alpha").Installable)

	s = newListScenario()
	res, _, err = s.run(t, "")
	require.NoError(t, err)
	for _, w := range res.Warnings {
		assert.NotContains(t, w, "is not known", "the Rancher version is in the table")
	}
}

// TestExtensionListNoRepository asserts a server with no extension repository
// and nothing installed lists nothing, exits 0 and warns.
func TestExtensionListNoRepository(t *testing.T) {
	t.Parallel()
	s := newListScenario()
	s.answers[reposPath] = rootAnswer{status: 200, body: collectionBody()}
	res, _, err := s.run(t, "")
	require.NoError(t, err)
	assert.Empty(t, listRows(t, res))
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "no Rancher extension repository is added")
}

// TestExtensionListRBAC asserts a refused read is RBAC with the existing
// mapping, for the collection of ClusterRepos, the index, the UIPlugins and
// the Apps.
func TestExtensionListRBAC(t *testing.T) {
	t.Parallel()
	forbidden := clierr.New(clierr.CodeRBAC, "forbidden")
	for _, tt := range []struct {
		name   string
		mutate func(*listScenario)
	}{
		{"clusterrepos", func(s *listScenario) { s.answers[reposPath] = rootAnswer{status: 403, body: "{}"} }},
		{"uiplugins", func(s *listScenario) { s.cluster.errs["uiplugins@"] = forbidden }},
		{"apps", func(s *listScenario) { s.cluster.errs["apps@cattle-ui-plugin-system"] = forbidden }},
	} {
		s := newListScenario()
		tt.mutate(s)
		_, _, err := s.run(t, "")
		require.Error(t, err, tt.name)
		assert.Equal(t, clierr.CodeRBAC, codeOf(t, err).Code, tt.name)
	}
}
