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

// appPost is one action request the server read.
type appPost struct {
	path  string
	query string
	body  map[string]any
}

// appServer is one Rancher answering every request app install and app
// uninstall send: the cluster lookup, the server version, the ClusterRepo,
// App, Operation and namespace reads, the access review, the index and info
// links, the management cluster and the two actions. Its request hooks
// apply to every test that starts one: a write other than the review and
// the actions fails the test, so does a Secret read that is not a Table
// read, an install whose chart entry lacks the request id or the source
// annotations, and a body field the Rancher API fields golden does not list
// for the edition the server is.
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
	// action answers each action request after the hooks; nil answers 201.
	action func(w http.ResponseWriter, r *http.Request)
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
// 1.0.0, their info, and the namespace; no App and no Operation. edition is
// community (v2.15.2) or prime (v2.14.5).
func newAppServer(t *testing.T, edition string) *appServer {
	t.Helper()
	s := &appServer{t: t, edition: edition, objects: map[string]map[string]any{}, allowed: true,
		fields: appGoldenFields(t),
		index: `{"apiVersion":"v1","entries":{"nginx":[` +
			`{"name":"nginx","version":"2.0.0","digest":"sha256:aaa"},{"name":"nginx","version":"1.0.0","digest":"sha256:bbb"}]}}`,
		infos: map[string]string{
			"2.0.0": `{"values":{"replicaCount":1},"chart":{"name":"nginx","annotations":{}}}`,
			"1.0.0": `{"values":{"replicaCount":1},"chart":{"name":"nginx","annotations":{}}}`,
		},
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

func (s *appServer) put(path string, obj map[string]any) {
	s.mu.Lock()
	s.objects[path] = obj
	s.mu.Unlock()
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
	if strings.Contains(path, "/secrets") && !strings.Contains(r.Header.Get("Accept"), "as=Table") {
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
	switch path {
	case "/rancherversion":
		version, prime := "v2.15.2", "false"
		if s.edition == "prime" {
			version, prime = "v2.14.5", "true"
		}
		writeJSON(w, http.StatusOK, map[string]string{"Version": version, "RancherPrime": prime})
	case steveClustersPath:
		writeJSON(w, http.StatusOK, steveClustersPages[r.URL.Query().Get("continue")])
	case steveClustersPath + "/c-aaa11":
		writeJSON(w, http.StatusOK, map[string]any{"id": "c-aaa11", "status": map[string]any{"linuxWorkerCount": 2}})
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
		if q.Get("chartName") != appChart || !ok {
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
