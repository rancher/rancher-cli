package cli

import (
	"encoding/json"
	"fmt"
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

	"github.com/stretchr/testify/require"
)

// The names the root-run app tests use: --cluster prod is the cluster
// c-aaa11 (steveClustersPages), whose ClusterRepo repo1 serves nginx.
const (
	appDown    = "/k8s/clusters/c-aaa11"
	appRepo    = "repo1"
	appChart   = "nginx"
	appNS      = "web"
	appRelease = "my-nginx"
	appOpName  = "helm-operation-abcde"
)

// appRegistryChart at appRegistryVersion is a chart of the repository whose
// default values declare both global.systemDefaultRegistry and
// global.cattle.systemDefaultRegistry, each with the value "". The index
// lists it, and its info answers like the info of nginx.
const (
	appRegistryChart   = "regchart"
	appRegistryVersion = "3.0.0"
)

// The paths of the reads app install makes for the values it adds, and
// what the server answers at each of them. The cluster is the management
// Cluster c-aaa11, the Settings are read through the Rancher server, never
// through the cluster, and the projects are the steve collection of the
// cluster. The projects list a Project displayed as System but without the
// System project label, first and with the lower name, before the labeled
// one named appSystemProject.
const (
	appClusterPath         = steveClustersPath + "/c-aaa11"
	appServerURLPath       = "/v1/management.cattle.io.settings/server-url"
	appRegistrySettingPath = "/v1/management.cattle.io.settings/system-default-registry"
	appProjectsPath        = "/v1/management.cattle.io.projects/c-aaa11"

	appClusterName     = "prod"
	appServerURL       = "https://rancher.example.test"
	appSettingRegistry = "setting-registry.example.test"
	appSystemProject   = "p-system"
)

const appProjectsBody = `{"data":[` +
	`{"id":"c-aaa11/p-aaaaa","metadata":{"name":"p-aaaaa"},"spec":{"clusterName":"c-aaa11","displayName":"System"}},` +
	`{"id":"c-aaa11/p-system","metadata":{"name":"p-system","labels":{"authz.management.cattle.io/system-project":"true"}},"spec":{"clusterName":"c-aaa11","displayName":"Platform"}}` +
	`],"count":2,"revision":"1"}`

// appAnswer is a status and a body a test sets for one path.
type appAnswer struct {
	status int
	body   string
}

// appPost is one action request the server read.
type appPost struct {
	path  string
	query string
	body  map[string]any
}

// appServer is one Rancher answering every request app install, app
// uninstall and app upgrade send: the cluster lookup, the server version, the
// ClusterRepo, App, Operation and namespace reads, the access review, the
// index and info links, the management cluster and the three actions. Its
// request hooks apply to every test that starts one: a write other than the
// review and the actions fails the test, so does a Secret read that is not a
// Table read (the one exception is the GET of releaseSecret with
// includeHelmData=true, which app upgrade sends for the stored values), an
// install or upgrade whose chart entry lacks the request id or the source
// annotations, an upgrade body with an install field, and a body field the
// Rancher API fields golden does not list for the edition the server is.
type appServer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	edition string
	objects map[string]map[string]any
	index   string
	infos   map[string]string
	allowed bool
	posts   []appPost
	// answers replace the answer to a GET of a path, by path.
	answers map[string]appAnswer
	// gets are the paths of the GET requests, in the order they came.
	gets []string
	// action answers each action request after the hooks; nil answers 201.
	action func(w http.ResponseWriter, r *http.Request)
	// releaseSecret is the path of the release Secret the upgrade tests set,
	// empty elsewhere: the only Secret a request may read other than as a
	// Table, and only with includeHelmData=true.
	releaseSecret string
	// plant changes an object answer before it is sent, by resource.
	plant  func(resource string, obj map[string]any)
	fields map[string]map[string][]string
}

func appRepoPath(name string) string {
	return appDown + "/apis/catalog.cattle.io/v1/clusterrepos/" + name
}

func appAppPath(ns, name string) string {
	return appDown + "/apis/catalog.cattle.io/v1/namespaces/" + ns + "/apps/" + name
}

func appOperationPath(ns, name string) string {
	return appDown + "/apis/catalog.cattle.io/v1/namespaces/" + ns + "/operations/" + name
}

func appNamespacePath(name string) string { return appDown + "/api/v1/namespaces/" + name }

// newAppServer serves the ClusterRepo, its index with nginx 2.0.0 and
// 1.0.0 and appRegistryChart appRegistryVersion, their info, the namespace,
// and the reads for the values app install adds (appClusterPath,
// appServerURLPath, appRegistrySettingPath and appProjectsPath); no App and
// no Operation. edition is community (v2.15.2) or prime (v2.14.5).
func newAppServer(t *testing.T, edition string) *appServer {
	t.Helper()
	s := &appServer{t: t, edition: edition, objects: map[string]map[string]any{}, allowed: true,
		fields: appGoldenFields(t),
		index: `{"apiVersion":"v1","entries":{"nginx":[` +
			`{"name":"nginx","version":"2.0.0","digest":"sha256:aaa"},{"name":"nginx","version":"1.0.0","digest":"sha256:bbb"}],` +
			`"` + appRegistryChart + `":[{"name":"` + appRegistryChart + `","version":"` + appRegistryVersion + `","digest":"sha256:ccc"}]}}`,
		infos: map[string]string{
			"2.0.0": `{"values":{"replicaCount":1},"chart":{"name":"nginx","version":"2.0.0","annotations":{}}}`,
			"1.0.0": `{"values":{"replicaCount":1},"chart":{"name":"nginx","version":"1.0.0","annotations":{}}}`,
		},
		answers: map[string]appAnswer{},
	}
	s.put(appRepoPath(appRepo), map[string]any{
		"apiVersion": "catalog.cattle.io/v1", "kind": "ClusterRepo",
		"metadata": map[string]any{"name": appRepo, "resourceVersion": "1"},
		"spec":     map[string]any{"url": "https://charts.example.test"},
	})
	s.put(appNamespacePath(appNS), map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": appNS, "resourceVersion": "1"},
	})
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// putApp stores the App of the release with a status.
func (s *appServer) putApp(status string) {
	s.put(appAppPath(appNS, appRelease), map[string]any{
		"apiVersion": "catalog.cattle.io/v1", "kind": "App",
		"metadata": map[string]any{"name": appRelease, "namespace": appNS, "resourceVersion": "7"},
		"spec": map[string]any{
			"name": appRelease,
			"info": map[string]any{"status": status},
			"chart": map[string]any{"metadata": map[string]any{
				"name": appChart, "version": "2.0.0",
				"annotations": map[string]any{"catalog.cattle.io/ui-source-repo-type": "cluster", "catalog.cattle.io/ui-source-repo": appRepo},
			}},
		},
	})
}

// The revisions the App fixtures use: the first revision, and a later one
// another request made.
const (
	appFirstRevision = 1
	appLaterRevision = 2
)

// appObject is the App of the release at revision version with status and
// description, whose chart carries the request id id ("" for none).
func appObject(status, id string, version int64, description string) map[string]any {
	ann := map[string]any{"catalog.cattle.io/ui-source-repo-type": "cluster", "catalog.cattle.io/ui-source-repo": appRepo}
	if id != "" {
		ann["cli.rancher.io/request-id"] = id
	}
	return map[string]any{
		"apiVersion": "catalog.cattle.io/v1", "kind": "App",
		"metadata": map[string]any{"name": appRelease, "namespace": appNS, "resourceVersion": "8"},
		"spec": map[string]any{
			"name": appRelease, "version": version,
			"info":  map[string]any{"status": status, "description": description},
			"chart": map[string]any{"metadata": map[string]any{"name": appChart, "version": "2.0.0", "annotations": ann}},
		},
	}
}

// appReleaseSecretName is the name of the Secret Helm keeps revision n of the
// release in.
func appReleaseSecretName(revision int64) string {
	return fmt.Sprintf("sh.helm.release.v1.%s.v%d", appRelease, revision)
}

// appStoredSecrets are the values the release Secret holds beside the user
// values, which an upgrade never prints: the chart's own values, the
// manifest and the hooks.
const (
	appStoredChartValue = "chartValuesS3cr3t"
	appStoredManifest   = "manifestS3cr3t"
	appStoredHook       = "hookS3cr3t"
)

// releaseSecretObject is the Secret steve returns for revision n of the
// release with includeHelmData=true: the decoded release under data.release,
// with config as the user values and the chart, manifest and hooks beside
// them.
func releaseSecretObject(revision int64, status string, config map[string]any) map[string]any {
	name := appReleaseSecretName(revision)
	return map[string]any{
		"id":       appNS + "/" + name,
		"type":     "secret",
		"metadata": map[string]any{"name": name, "namespace": appNS, "labels": map[string]any{"owner": "helm", "name": appRelease, "version": fmt.Sprint(revision)}},
		"data": map[string]any{"release": map[string]any{
			"name": appRelease, "namespace": appNS, "version": revision,
			"info":     map[string]any{"status": status},
			"config":   config,
			"chart":    map[string]any{"metadata": map[string]any{"name": appChart}, "values": map[string]any{"chartKey": appStoredChartValue}},
			"manifest": "kind: ConfigMap\nmetadata:\n  name: " + appStoredManifest + "\n",
			"hooks":    []any{map[string]any{"name": appStoredHook, "manifest": appStoredHook}},
		}},
	}
}

// upgradeApp is the App of the release at revision with status, naming its
// release Secret in an owner reference and carrying no request id.
func upgradeApp(status string, revision int64) map[string]any {
	app := appObject(status, "", revision, "")
	app["metadata"].(map[string]any)["resourceVersion"] = "7"
	app["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
		"apiVersion": "v1", "kind": "Secret", "name": appReleaseSecretName(revision), "uid": "uid-1",
	}}
	return app
}

// putRelease stores the App at revision with status and the release Secret
// of that revision holding config, and lets the server answer the read of
// that Secret with includeHelmData=true.
func (s *appServer) putRelease(status string, revision int64, config map[string]any) {
	s.put(appAppPath(appNS, appRelease), upgradeApp(status, revision))
	path := appDown + "/v1/secrets/" + appNS + "/" + appReleaseSecretName(revision)
	s.put(path, releaseSecretObject(revision, status, config))
	s.mu.Lock()
	s.releaseSecret = path
	s.mu.Unlock()
}

// opStatus is an Operation status at observedGeneration 1 with the
// conditions, each a type, a status and a message; an empty message is left
// out, as Rancher leaves it out.
func opStatus(conds ...[3]string) map[string]any {
	list := []any{}
	for _, c := range conds {
		cond := map[string]any{"type": c[0], "status": c[1]}
		if c[2] != "" {
			cond["message"] = c[2]
		}
		list = append(list, cond)
	}
	return map[string]any{"observedGeneration": 1, "releaseName": appRelease, "namespace": appNS, "conditions": list}
}

// completeOnAction makes the action store the Operation its answer names,
// with status, and then change the App: app gives the App for the request
// id the install sent, and an app that returns nil removes it. A nil app
// leaves the App as it is.
func (s *appServer) completeOnAction(status map[string]any, app func(requestID string) map[string]any) {
	s.action = func(w http.ResponseWriter, _ *http.Request) {
		id := ""
		if posts := s.recorded(); len(posts) > 0 {
			charts, _ := posts[len(posts)-1].body["charts"].([]any)
			if len(charts) > 0 {
				chart, _ := charts[0].(map[string]any)
				ann, _ := chart["annotations"].(map[string]any)
				id, _ = ann["cli.rancher.io/request-id"].(string)
			}
		}
		s.put(appOperationPath(appNS, appOpName), map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "Operation",
			"metadata": map[string]any{"name": appOpName, "namespace": appNS, "generation": 1, "resourceVersion": "1"},
			"status":   status,
		})
		if app != nil {
			if obj := app(id); obj != nil {
				s.put(appAppPath(appNS, appRelease), obj)
			} else {
				s.mu.Lock()
				delete(s.objects, appAppPath(appNS, appRelease))
				s.mu.Unlock()
			}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"type": "chartActionOutput", "links": map[string]any{},
			"operationName": appOpName, "operationNamespace": appNS})
	}
}

func (s *appServer) put(path string, obj map[string]any) {
	s.mu.Lock()
	s.objects[path] = obj
	s.mu.Unlock()
}

// answer makes the server answer a GET of path with status and body, over
// the answer it serves.
func (s *appServer) answer(path string, status int, body string) {
	s.mu.Lock()
	s.answers[path] = appAnswer{status: status, body: body}
	s.mu.Unlock()
}

// deny makes the server answer a GET of path with 403.
func (s *appServer) deny(path string) {
	s.answer(path, http.StatusForbidden, `{"type":"error","status":403,"code":"Forbidden","message":"forbidden"}`)
}

// getCount is the number of GET requests of path so far.
func (s *appServer) getCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, g := range s.gets {
		if g == path {
			n++
		}
	}
	return n
}

func (s *appServer) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func (s *appServer) recorded() []appPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.posts)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func kubeNotFound(w http.ResponseWriter, what string) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404,
		"message": what + " not found",
	})
}

func (s *appServer) serve(w http.ResponseWriter, r *http.Request) {
	t := s.t
	path := r.URL.Path
	s.mu.Lock()
	releaseSecret := s.releaseSecret
	s.mu.Unlock()
	releaseRead := releaseSecret != "" && r.Method == http.MethodGet && path == releaseSecret && r.URL.Query().Get("includeHelmData") == "true"
	if strings.Contains(path, "/secrets") && !releaseRead && !strings.Contains(r.Header.Get("Accept"), "as=Table") {
		t.Errorf("an app command read Secrets with a request that is not a Table read: %s", r.URL)
	}
	switch {
	case r.Method == http.MethodPost && path == appDown+"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		var review map[string]any
		_ = json.NewDecoder(r.Body).Decode(&review)
		s.mu.Lock()
		review["status"] = map[string]any{"allowed": s.allowed}
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, review)
		return
	case r.Method == http.MethodPost && r.URL.Query().Get("action") != "":
		s.serveAction(w, r)
		return
	case r.Method != http.MethodGet:
		t.Errorf("an app command sent %s %s", r.Method, r.URL)
		http.Error(w, "no write", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.gets = append(s.gets, path)
	planted, isPlanted := s.answers[path]
	s.mu.Unlock()
	if isPlanted {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(planted.status)
		_, _ = io.WriteString(w, planted.body)
		return
	}
	switch path {
	case "/rancherversion":
		version, prime := "v2.15.2", "false"
		if s.edition == "prime" {
			version, prime = "v2.14.5", "true"
		}
		writeJSON(w, http.StatusOK, map[string]string{"Version": version, "RancherPrime": prime})
	case steveClustersPath:
		writeJSON(w, http.StatusOK, steveClustersPages[r.URL.Query().Get("continue")])
	case appClusterPath:
		writeJSON(w, http.StatusOK, map[string]any{"id": "c-aaa11", "metadata": map[string]any{"name": "c-aaa11"},
			"spec": map[string]any{"displayName": appClusterName}, "status": map[string]any{"linuxWorkerCount": 2}})
	case appServerURLPath:
		writeJSON(w, http.StatusOK, map[string]any{"id": "server-url", "value": appServerURL, "default": ""})
	case appRegistrySettingPath:
		writeJSON(w, http.StatusOK, map[string]any{"id": "system-default-registry", "value": "", "default": appSettingRegistry})
	case appProjectsPath:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, appProjectsBody)
	case appDown + "/v1/catalog.cattle.io.clusterrepos/" + appRepo:
		s.serveLink(w, r)
	case appDown + "/apis/catalog.cattle.io/v1/namespaces/" + appNS + "/operations":
		s.serveList(w, "operations", appDown+"/apis/catalog.cattle.io/v1/namespaces/"+appNS+"/operations/")
	default:
		s.mu.Lock()
		obj, ok := s.objects[path]
		var out map[string]any
		if ok {
			out = deepCopyJSON(obj)
			if s.plant != nil {
				s.plant(resourceOf(path), out)
			}
		}
		s.mu.Unlock()
		if !ok {
			kubeNotFound(w, path)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// resourceOf is the resource plural of a kube object path.
func resourceOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

func deepCopyJSON(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *appServer) serveList(w http.ResponseWriter, resource, prefix string) {
	s.mu.Lock()
	var items []any
	for _, k := range slices.Sorted(maps.Keys(s.objects)) {
		if strings.HasPrefix(k, prefix) {
			item := deepCopyJSON(s.objects[k])
			if s.plant != nil {
				s.plant(resource, item)
			}
			items = append(items, item)
		}
	}
	s.mu.Unlock()
	if items == nil {
		items = []any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "OperationList",
		"metadata": map[string]any{"resourceVersion": "9"}, "items": items})
}

func (s *appServer) serveLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch q.Get("link") {
	case "index":
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		_, _ = io.WriteString(w, s.index)
		s.mu.Unlock()
	case "info":
		s.mu.Lock()
		body, ok := s.infos[q.Get("version")]
		s.mu.Unlock()
		if q.Get("chartName") == appRegistryChart && q.Get("version") == appRegistryVersion {
			body, ok = `{"values":{"replicaCount":1,"global":{"systemDefaultRegistry":"","cattle":{"systemDefaultRegistry":""}}},`+
				`"chart":{"name":"`+appRegistryChart+`","version":"`+appRegistryVersion+`","annotations":{}}}`, true
		} else if q.Get("chartName") != appChart {
			ok = false
		}
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"type": "error", "status": 404, "code": "NotFound", "message": "chart not found"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	default:
		http.Error(w, "unexpected link", http.StatusBadRequest)
	}
}

// serveAction checks and records one action request, then answers it.
func (s *appServer) serveAction(w http.ResponseWriter, r *http.Request) {
	t := s.t
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("the action body is not a JSON object: %v", err)
	}
	action := r.URL.Query().Get("action")
	switch {
	case action == "install" && r.URL.Path == appDown+"/v1/catalog.cattle.io.clusterrepos/"+appRepo:
		s.checkFields("ChartInstallAction", body)
		charts, _ := body["charts"].([]any)
		if len(charts) == 0 {
			t.Errorf("install body has no chart entry")
		}
		for _, c := range charts {
			chart, _ := c.(map[string]any)
			s.checkFields("ChartInstall", chart)
			ann, _ := chart["annotations"].(map[string]any)
			if id, _ := ann["cli.rancher.io/request-id"].(string); id == "" {
				t.Errorf("install chart entry has no cli.rancher.io/request-id annotation")
			}
			if ann["catalog.cattle.io/ui-source-repo-type"] != "cluster" || ann["catalog.cattle.io/ui-source-repo"] != appRepo {
				t.Errorf("install chart entry lacks the source annotations: %v", ann)
			}
		}
	case action == "upgrade" && r.URL.Path == appDown+"/v1/catalog.cattle.io.clusterrepos/"+appRepo:
		s.checkFields("ChartUpgradeAction", body)
		if _, ok := body["install"]; ok {
			t.Errorf("upgrade body carries install, which Rancher always sets: %v", body)
		}
		charts, _ := body["charts"].([]any)
		if len(charts) != 1 {
			t.Errorf("upgrade body has %d chart entries, want 1", len(charts))
		}
		for _, c := range charts {
			chart, _ := c.(map[string]any)
			s.checkFields("ChartUpgrade", chart)
			ann, _ := chart["annotations"].(map[string]any)
			if id, _ := ann["cli.rancher.io/request-id"].(string); id == "" {
				t.Errorf("upgrade chart entry has no cli.rancher.io/request-id annotation")
			}
			if ann["catalog.cattle.io/ui-source-repo-type"] != "cluster" || ann["catalog.cattle.io/ui-source-repo"] != appRepo {
				t.Errorf("upgrade chart entry lacks the source annotations: %v", ann)
			}
			if chart["releaseName"] != appRelease || body["namespace"] != appNS {
				t.Errorf("upgrade body names release %v in namespace %v, want the App's %s in %s", chart["releaseName"], body["namespace"], appRelease, appNS)
			}
		}
	case action == "uninstall" && r.URL.Path == appDown+"/v1/catalog.cattle.io.apps/"+appNS+"/"+appRelease:
		s.checkFields("ChartUninstallAction", body)
	default:
		t.Errorf("unexpected action request %s", r.URL)
	}
	s.mu.Lock()
	s.posts = append(s.posts, appPost{path: r.URL.Path, query: r.URL.RawQuery, body: body})
	answer := s.action
	s.mu.Unlock()
	if answer != nil {
		answer(w, r)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"type": "chartActionOutput", "links": map[string]any{},
		"operationName": appOpName, "operationNamespace": appNS})
}

func (s *appServer) checkFields(typ string, obj map[string]any) {
	s.t.Helper()
	for field := range obj {
		if !slices.Contains(s.fields[typ][field], s.edition) {
			s.t.Errorf("%s field %q is not a field of %s for the %s edition", typ, field, typ, s.edition)
		}
	}
}

// appGoldenFields reads the catalog action types of the Rancher API fields
// golden: type, then field, then the editions that have it.
func appGoldenFields(t *testing.T) map[string]map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "rancher-api-fields.golden.json"))
	require.NoError(t, err)
	var g struct {
		CatalogActions []struct {
			Type   string              `json:"type"`
			Fields map[string][]string `json:"fields"`
		} `json:"catalogActions"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	out := map[string]map[string][]string{}
	for _, a := range g.CatalogActions {
		out[a.Type] = a.Fields
	}
	return out
}

// appRun runs the real root against the server, with a config directory of
// its own, and returns stdout and stderr.
func appRun(t *testing.T, s *appServer, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	return appRunStdin(t, s, "", args...)
}

// appRunStdin is appRun with stdin.
func appRunStdin(t *testing.T, s *appServer, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dir := grConfigDir(t, s.srv)
	root := grRoot(t)
	root.Reader = strings.NewReader(stdin)
	out, errOut, runErr := grRun(t, root, dir, args...)
	return string(out), string(errOut), runErr
}

// appEnvelope is the decoded JSON envelope of a run.
type appEnvelope struct {
	Kind     string         `json:"kind"`
	Data     map[string]any `json:"data"`
	Warnings []string       `json:"warnings"`
}

func decodeApp(t *testing.T, raw string) appEnvelope {
	t.Helper()
	var env appEnvelope
	require.NoError(t, json.Unmarshal([]byte(raw), &env), raw)
	return env
}
