package cli

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	extChart     = "epinio"
	extRepo      = "partner-extensions"
	extNamespace = "cattle-ui-plugin-system"
	extOpName    = "helm-operation-ext"
	extLocalRoot = localProxyPrefix
)

// extensionIndex is the index of the one ClusterRepo the extension server
// serves: the extension chart extChart in two versions, both compatible with
// the server version the extension server reports.
const extensionIndex = `{"apiVersion":"v1","entries":{"` + extChart + `":[` +
	`{"name":"` + extChart + `","version":"1.14.3","digest":"sha256:aaa","annotations":{` +
	`"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/rancher-version":">= 2.10.0","catalog.cattle.io/ui-extensions-version":">= 3.0.0 < 4.0.0"}},` +
	`{"name":"` + extChart + `","version":"1.14.2","digest":"sha256:bbb","annotations":{` +
	`"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/rancher-version":">= 2.10.0","catalog.cattle.io/ui-extensions-version":">= 3.0.0 < 4.0.0"}}]}}`

// extensionServer is one Rancher responding to what the extension commands read
// from the local cluster and the action requests they send: the feature, the
// namespace, the server version, the local management cluster, the ClusterRepo
// collection and its index, the UIPlugins, the Apps and the Operations, and the
// system default registry setting. A write other than an action fails the
// test, and so does a body field the Rancher API fields golden does not list
// for the edition the server is. Its request hooks apply to every test that
// starts one.
type extensionServer struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	apps map[string]map[string]any
	// operations are the Operations of the namespace.
	operations []map[string]any
	posts      []appPost
	gets       []string
	fields     map[string]map[string][]string
	// editions are the editions the Rancher API fields golden has.
	editions []string
	// plant changes an object response before it is sent, by resource.
	plant func(resource string, obj map[string]any)
}

func newExtensionServer(t *testing.T) *extensionServer {
	t.Helper()
	s := &extensionServer{t: t, apps: map[string]map[string]any{}, fields: appGoldenFields(t), editions: goldenEditions(t)}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// putApp installs the extension's App: the chart at 1.14.2, from the
// ClusterRepo extRepo, at the given status, as the first revision of the
// release.
func (s *extensionServer) putApp(status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[extChart] = map[string]any{
		"apiVersion": "catalog.cattle.io/v1", "kind": "App",
		"metadata": map[string]any{"name": extChart, "namespace": extNamespace, "resourceVersion": "7"},
		"spec": map[string]any{
			"name": extChart, "namespace": extNamespace, "version": int64(1),
			"info": map[string]any{"status": status},
			"chart": map[string]any{"metadata": map[string]any{
				"name": extChart, "version": "1.14.2",
				"annotations": map[string]any{"catalog.cattle.io/ui-source-repo-type": "cluster", "catalog.cattle.io/ui-source-repo": extRepo},
			}},
		},
	}
}

func (s *extensionServer) recorded() []appPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.posts)
}

func (s *extensionServer) serve(w http.ResponseWriter, r *http.Request) {
	t := s.t
	path := r.URL.Path
	if r.Method == http.MethodPost && r.URL.Query().Get("action") != "" {
		s.serveAction(w, r)
		return
	}
	if r.Method != http.MethodGet {
		t.Errorf("an extension command sent %s %s", r.Method, r.URL)
		http.Error(w, "no write", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.gets = append(s.gets, r.URL.RequestURI())
	app := s.apps[extChart]
	s.mu.Unlock()
	switch {
	case path == "/rancherversion":
		writeJSON(w, http.StatusOK, map[string]string{"Version": "v2.15.3", "RancherPrime": "false"})
	case path == extLocalRoot+"/apis/management.cattle.io/v3/features/uiextension":
		kubeNotFound(w, "feature")
	case path == extLocalRoot+"/api/v1/namespaces/"+extNamespace:
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": extNamespace}})
	case path == steveClustersPath+"/local":
		writeJSON(w, http.StatusOK, map[string]any{"id": "local", "metadata": map[string]any{"name": "local"},
			"status": map[string]any{"info": map[string]any{"kubernetesVersion": "v1.36.0"}}})
	case path == "/v1/catalog.cattle.io.clusterrepos":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
			"id": extRepo, "metadata": map[string]any{"name": extRepo, "state": map[string]any{"name": "active"}},
			"spec": map[string]any{"enabled": true, "gitRepo": "https://github.com/rancher/partner-extensions"},
		}}, "count": 1, "revision": "1"})
	case path == "/v1/catalog.cattle.io.clusterrepos/"+extRepo && r.URL.Query().Get("link") == "index":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, extensionIndex)
	case path == extLocalRoot+"/apis/catalog.cattle.io/v1/uiplugins":
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "UIPluginList", "items": []any{}})
	case path == extLocalRoot+"/apis/catalog.cattle.io/v1/namespaces/"+extNamespace+"/operations":
		s.mu.Lock()
		items := []any{}
		for _, op := range s.operations {
			item := deepCopyJSON(op)
			if s.plant != nil {
				s.plant("operations", item)
			}
			items = append(items, item)
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "OperationList",
			"metadata": map[string]any{"resourceVersion": "9"}, "items": items})
	case path == extLocalRoot+"/apis/catalog.cattle.io/v1/namespaces/"+extNamespace+"/apps/"+extChart:
		if app == nil {
			kubeNotFound(w, "app")
			return
		}
		writeJSON(w, http.StatusOK, deepCopyJSON(app))
	case path == extLocalRoot+"/apis/catalog.cattle.io/v1/clusterrepos/"+extRepo:
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "ClusterRepo",
			"metadata": map[string]any{"name": extRepo}, "spec": map[string]any{"gitRepo": "https://github.com/rancher/partner-extensions"}})
	case path == "/v1/management.cattle.io.settings/system-default-registry":
		writeJSON(w, http.StatusOK, map[string]any{"id": "system-default-registry", "value": ""})
	default:
		t.Errorf("an extension command read %s, which the extension server does not serve", r.URL.RequestURI())
		kubeNotFound(w, path)
	}
}

// serveAction checks and records one action request, then responds to it.
func (s *extensionServer) serveAction(w http.ResponseWriter, r *http.Request) {
	t := s.t
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("the action body is not a JSON object: %v", err)
	}
	action := r.URL.Query().Get("action")
	switch {
	case action == "install" && r.URL.Path == "/v1/catalog.cattle.io.clusterrepos/"+extRepo:
		s.checkFields("ChartInstallAction", body)
		charts, _ := body["charts"].([]any)
		if len(charts) == 0 {
			t.Errorf("the action body has no chart")
		}
		for _, c := range charts {
			chart, _ := c.(map[string]any)
			s.checkFields("ChartInstall", chart)
		}
	case action == "upgrade" && r.URL.Path == "/v1/catalog.cattle.io.clusterrepos/"+extRepo:
		s.checkFields("ChartUpgradeAction", body)
		charts, _ := body["charts"].([]any)
		if len(charts) == 0 {
			t.Errorf("the action body has no chart")
		}
		for _, c := range charts {
			chart, _ := c.(map[string]any)
			s.checkFields("ChartUpgrade", chart)
		}
	case action == "uninstall" && r.URL.Path == "/v1/catalog.cattle.io.apps/"+extNamespace+"/"+extChart:
		s.checkFields("ChartUninstallAction", body)
	default:
		t.Errorf("unexpected action request %s", r.URL)
	}
	s.mu.Lock()
	s.posts = append(s.posts, appPost{path: r.URL.Path, query: r.URL.RawQuery, body: body})
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"type": "chartActionOutput", "links": map[string]any{},
		"operationName": extOpName, "operationNamespace": extNamespace})
}

// goldenEditions returns the editions the Rancher API fields golden has.
func goldenEditions(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "rancher-api-fields.golden.json"))
	require.NoError(t, err)
	var g struct {
		Oracles map[string]json.RawMessage `json:"oracles"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	require.NotEmpty(t, g.Oracles)
	return slices.Sorted(maps.Keys(g.Oracles))
}

// checkFields fails the test for a field the Rancher API fields golden does
// not list for every edition it has.
func (s *extensionServer) checkFields(typ string, obj map[string]any) {
	s.t.Helper()
	for field := range obj {
		for _, edition := range s.editions {
			if !slices.Contains(s.fields[typ][field], edition) {
				s.t.Errorf("%s field %q is not a field of %s for the %s edition", typ, field, typ, edition)
			}
		}
	}
}

// TestExtensionInstallBodyFieldsAreRancherFields asserts that every field of
// the install request body is a field of Rancher's install action types, and
// that the request goes to the local cluster with the chart name as the
// release name.
func TestExtensionInstallBodyFieldsAreRancherFields(t *testing.T) {
	t.Parallel()
	s := newExtensionServer(t)
	stdout, stderr, err := extensionRun(t, s, "extension", "install", extChart, "--apply", "-o", "json")
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, extOpName, env.Data["operationName"])
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/v1/catalog.cattle.io.clusterrepos/"+extRepo, posts[0].path)
	assert.Equal(t, extNamespace, posts[0].body["namespace"])
	charts, _ := posts[0].body["charts"].([]any)
	require.Len(t, charts, 1)
	chart, _ := charts[0].(map[string]any)
	assert.Equal(t, extChart, chart["releaseName"])
	assert.Equal(t, "1.14.3", chart["version"])
	assert.Contains(t, posts[0].query, "action=install")
}

// TestExtensionUpgradeBodyFieldsAreRancherFields asserts that every field of
// the upgrade request body is a field of Rancher's upgrade action types, and
// that the request goes to the local cluster with the chart name as the
// release name.
func TestExtensionUpgradeBodyFieldsAreRancherFields(t *testing.T) {
	t.Parallel()
	s := newExtensionServer(t)
	s.putApp("deployed")
	stdout, stderr, err := extensionRun(t, s, "extension", "upgrade", extChart, "--apply", "--yes", "-o", "json")
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, extOpName, env.Data["operationName"])
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/v1/catalog.cattle.io.clusterrepos/"+extRepo, posts[0].path)
	assert.Equal(t, extNamespace, posts[0].body["namespace"])
	charts, _ := posts[0].body["charts"].([]any)
	require.Len(t, charts, 1)
	chart, _ := charts[0].(map[string]any)
	assert.Equal(t, extChart, chart["releaseName"])
	assert.Equal(t, "1.14.3", chart["version"])
	assert.Contains(t, posts[0].query, "action=upgrade")
}

// TestExtensionUninstallBodyFieldsAreRancherFields asserts that every field of
// the uninstall request body is a field of Rancher's uninstall action type
// (the body has none), and that the request goes to the App of the local
// cluster.
func TestExtensionUninstallBodyFieldsAreRancherFields(t *testing.T) {
	t.Parallel()
	s := newExtensionServer(t)
	s.putApp("deployed")
	stdout, stderr, err := extensionRun(t, s, "extension", "uninstall", extChart, "--apply", "--yes", "-o", "json")
	require.NoError(t, err, stderr)
	env := decodeApp(t, stdout)
	assert.Equal(t, extOpName, env.Data["operationName"])
	posts := s.recorded()
	require.Len(t, posts, 1)
	assert.Equal(t, "/v1/catalog.cattle.io.apps/"+extNamespace+"/"+extChart, posts[0].path)
	assert.Empty(t, posts[0].body)
	assert.Contains(t, posts[0].query, "action=uninstall")
}

// extensionRun runs the real root against the extension server, with a config
// directory of its own.
func extensionRun(t *testing.T, s *extensionServer, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dir := grConfigDir(t, s.srv)
	root := grRoot(t)
	root.Reader = strings.NewReader("")
	out, errOut, runErr := grRun(t, root, dir, args...)
	return string(out), string(errOut), runErr
}

// extensionScrubDeps returns the deps of an extension command row: the
// extension server, filled by prepare, and the real client factory over it.
// The server plants the row's values in every Operation response, and the fake
// counts the responses that carry a value the row must not show.
func extensionScrubDeps(prepare func(s *extensionServer)) func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
	return func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
		t.Helper()
		s := newExtensionServer(t)
		prepare(s)
		s.plant = func(resource string, obj map[string]any) {
			if f.kind == nil || resource != f.kind.GVR.Resource {
				return
			}
			mergeMissing(obj, ps.fragment)
			f.scan(&unstructured.Unstructured{Object: obj})
		}
		dir := grConfigDir(t, s.srv)
		factory := newClientFactory(dir, config.NewFileStore(dir))
		return runDeps{clients: factory, roots: factory, actions: factory, serverBase: factory, sessionNamer: factory}
	}
}
