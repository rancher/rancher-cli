package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The names the app tests use: --cluster prod resolves to the cluster id
// c-1, whose ClusterRepo repo1 serves the chart nginx.
const (
	testTypedCluster = "prod"
	testCluster      = "c-1"
	testRepo         = "repo1"
	testChart        = "nginx"
	testNS           = "web"
	testRelease      = "my-nginx"
	testRequestID    = "req-0001"
)

// appInput is the runtime.Input of an app command test, with the stdin
// flag the pipeline sets.
type appInput struct {
	s        map[string]string
	b        map[string]bool
	strs     map[string][]string
	args     []string
	terminal bool
}

func (i appInput) String(k string) string    { return i.s[k] }
func (i appInput) Bool(k string) bool        { return i.b[k] }
func (i appInput) Strings(k string) []string { return i.strs[k] }
func (i appInput) Args() []string            { return i.args }
func (i appInput) StdinTerminal() bool       { return i.terminal }
func (i appInput) IsSet(k string) bool {
	_, s := i.s[k]
	_, b := i.b[k]
	_, l := i.strs[k]
	return s || b || l
}

var _ runtime.StdinInput = appInput{}

// installInput is the input of `app install my-nginx --cluster prod
// --namespace web --repo repo1 --chart nginx`, changed by edits.
func installInput(edits ...func(*appInput)) appInput {
	in := appInput{
		s: map[string]string{
			engine.FlagCluster: testTypedCluster, engine.FlagNamespace: testNS,
			flagRepo: testRepo, flagChart: testChart,
		},
		b: map[string]bool{}, strs: map[string][]string{}, args: []string{testRelease},
	}
	for _, e := range edits {
		e(&in)
	}
	return in
}

// upgradeInput is the input of `app upgrade my-nginx --cluster prod
// --namespace web`, changed by edits.
func upgradeInput(edits ...func(*appInput)) appInput {
	in := appInput{
		s: map[string]string{engine.FlagCluster: testTypedCluster, engine.FlagNamespace: testNS},
		b: map[string]bool{}, strs: map[string][]string{}, args: []string{testRelease},
	}
	for _, e := range edits {
		e(&in)
	}
	return in
}

// uninstallInput is the input of `app uninstall my-nginx --cluster prod
// --namespace web`, changed by edits.
func uninstallInput(edits ...func(*appInput)) appInput {
	in := appInput{
		s: map[string]string{engine.FlagCluster: testTypedCluster, engine.FlagNamespace: testNS},
		b: map[string]bool{}, strs: map[string][]string{}, args: []string{testRelease},
	}
	for _, e := range edits {
		e(&in)
	}
	return in
}

// files is a FileSource over a table; "-" is stdin.
type files map[string]string

func (f files) Read(p string) ([]byte, error) {
	b, ok := f[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(b), nil
}

// post is one recorded action request.
type post struct {
	path string
	body map[string]any
}

// appFake serves every port an app command uses: the cluster reads, the
// access review and discovery (ClusterClient), the steve link and
// management reads (RootClient) and the action POSTs (ActionClient). Its
// request hooks apply to every test that builds one: a write through the
// Kubernetes client fails the test, so does a read of a Secret (the
// commands read no Secret data through the cluster client), an install or
// upgrade POST whose chart entry lacks the request id or the two source
// annotations, and a POST body with a
// field the golden does not list for the edition the fake serves.
type appFake struct {
	t *testing.T

	mu      sync.Mutex
	objs    map[string]*unstructured.Unstructured
	errs    map[string]error
	reads   []string
	allowed bool
	review  error

	resources    []engine.APIResource
	discoveryErr error

	root    *fakeRoot
	edition string
	fields  map[string]map[string][]string

	posts      []post
	postAnswer func(path string) ([]byte, error)
	// onRead runs before each cluster read, so a test can change the state
	// between Plan and Execute.
	onRead func(key string)
	// after holds, by read key, the answers of the Gets after the first
	// action request, in order; the last one repeats.
	after map[string][]scripted
}

// scripted is one answer of a Get after the action request: an object with
// fields (its top-level fields other than metadata name and namespace), an
// error, or, with neither, NOT_FOUND.
type scripted struct {
	fields map[string]any
	err    error
}

func found(fields map[string]any) scripted { return scripted{fields: fields} }

var absent = scripted{}

func failing(err error) scripted { return scripted{err: err} }

// script sets the answers of the Gets of key after the action request.
func (f *appFake) script(key string, answers ...scripted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.after == nil {
		f.after = map[string][]scripted{}
	}
	f.after[key] = answers
}

// scriptedGet answers a Get of key from the script once an action request
// was sent; ok is false when the script does not apply. The caller holds
// f.mu.
func (f *appFake) scriptedGet(key, ns, name string) (obj *unstructured.Unstructured, ok bool, err error) {
	seq := f.after[key]
	if len(f.posts) == 0 || len(seq) == 0 {
		return nil, false, nil
	}
	a := seq[0]
	if len(seq) > 1 {
		f.after[key] = seq[1:]
	}
	switch {
	case a.err != nil:
		return nil, true, a.err
	case a.fields == nil:
		return nil, true, clierr.New(clierr.CodeNotFound, name+" not found")
	}
	obj = &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range a.fields {
		obj.Object[k] = deepCopyValue(v)
	}
	obj.SetName(name)
	obj.SetNamespace(ns)
	if obj.GetResourceVersion() == "" {
		obj.SetResourceVersion("1")
	}
	return obj, true, nil
}

func deepCopyValue(v any) any {
	return (&unstructured.Unstructured{Object: map[string]any{"v": v}}).DeepCopy().Object["v"]
}

// newAppFake is a cluster with the ClusterRepo, the chart's index and info,
// and the namespace; no App and no Operation.
func newAppFake(t *testing.T) *appFake {
	t.Helper()
	f := &appFake{
		t: t, objs: map[string]*unstructured.Unstructured{}, errs: map[string]error{},
		allowed: true, edition: "prime", fields: goldenActionFields(t),
	}
	f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{"url": "https://charts.example.test"}})
	f.put("namespaces", "", testNS, nil)
	f.root = newFakeRoot(map[string]rootAnswer{
		indexPath(): {status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {
			{Version: "2.0.0", Digest: "sha256:aaa"}, {Version: "1.0.0", Digest: "sha256:bbb"},
		}})},
		infoPath(testChart, "2.0.0"):              {status: 200, body: `{"values":{"replicaCount":1},"chart":{"annotations":{},"version":"2.0.0"}}`},
		infoPath(testChart, "1.0.0"):              {status: 200, body: `{"values":{"replicaCount":1},"chart":{"annotations":{},"version":"1.0.0"}}`},
		clusterPath:                               {status: 200, body: testClusterObject},
		settingPath(settingServerURL):             {status: 200, body: `{"id":"server-url","value":"` + testServerURL + `","default":""}`},
		settingPath(settingSystemDefaultRegistry): {status: 200, body: `{"id":"system-default-registry","value":"","default":"` + testSettingRegistry + `"}`},
		projectsPath:                              {status: 200, body: testProjects},
	})
	f.root.t = t
	f.postAnswer = func(string) ([]byte, error) {
		return []byte(`{"type":"chartActionOutput","links":{},"operationName":"helm-operation-abcde","operationNamespace":"web"}`), nil
	}
	return f
}

// The server reads app install makes for the cluster values: the
// management Cluster (display name "Production", Linux workers), the two
// Settings on the Rancher server and the cluster's projects, where a
// project displayed as System but without the System project label is
// listed before the labeled one.
const (
	clusterPath         = "/v1/management.cattle.io.clusters/" + testCluster
	projectsPath        = "/v1/management.cattle.io.projects/" + testCluster
	testServerURL       = "https://rancher.example.test"
	testSettingRegistry = "setting-registry.example.test"
	testSystemProject   = "p-system"
	testClusterObject   = `{"id":"c-1","metadata":{"name":"c-1"},"spec":{"displayName":"Production"},"status":{"linuxWorkerCount":2}}`
	testProjects        = `{"data":[` +
		`{"id":"c-1/p-aaaaa","metadata":{"name":"p-aaaaa"},"spec":{"clusterName":"c-1","displayName":"System"}},` +
		`{"id":"c-1/p-system","metadata":{"name":"p-system","labels":{"authz.management.cattle.io/system-project":"true"}},"spec":{"clusterName":"c-1","displayName":"Platform"}}` +
		`],"count":2,"revision":"1"}`
)

func settingPath(name string) string { return "/v1/management.cattle.io.settings/" + name }

func indexPath() string {
	return "/k8s/clusters/" + testCluster + "/v1/catalog.cattle.io.clusterrepos/" + testRepo + "?link=index"
}

func infoPath(chart, version string) string {
	return "/k8s/clusters/" + testCluster + "/v1/catalog.cattle.io.clusterrepos/" + testRepo +
		"?link=info&chartName=" + chart + "&version=" + version
}

func indexJSON(entries map[string][]ChartVersion) string {
	b, err := json.Marshal(Index{Entries: entries})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func objKey(resource, ns, name string) string { return resource + "/" + ns + "/" + name }

// put stores an object; fields holds its top-level fields other than
// metadata.
func (f *appFake) put(resource, ns, name string, fields map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	maps.Copy(obj.Object, fields)
	obj.SetName(name)
	obj.SetNamespace(ns)
	if obj.GetResourceVersion() == "" {
		obj.SetResourceVersion("1")
	}
	f.mu.Lock()
	f.objs[objKey(resource, ns, name)] = obj
	f.mu.Unlock()
	return obj
}

func (f *appFake) remove(resource, ns, name string) {
	f.mu.Lock()
	delete(f.objs, objKey(resource, ns, name))
	f.mu.Unlock()
}

// fail makes a read answer err: "get:<resource>/<ns>/<name>" or
// "list:<resource>/<ns>".
func (f *appFake) fail(key string, err error) { f.errs[key] = err }

func (f *appFake) ResolveCluster(_ context.Context, v string) (string, error) {
	if v == testTypedCluster {
		return testCluster, nil
	}
	return v, nil
}

func (f *appFake) Cluster(context.Context, string) (engine.ClusterClient, error) { return f, nil }

func (f *appFake) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	f.t.Error("an app command impersonated a user")
	return f, nil
}

func (f *appFake) Root() (engine.RootClient, error) { return f.root, nil }

func (f *appFake) Action() (engine.ActionClient, error) { return f, nil }

func (f *appFake) checkSecret(gvr schema.GroupVersionResource) {
	if gvr.Group == "" && gvr.Resource == "secrets" {
		f.t.Errorf("an app command read Secrets with a request that is not a Table read")
	}
}

func (f *appFake) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	f.checkSecret(gvr)
	key := "get:" + objKey(gvr.Resource, ns, name)
	if f.onRead != nil {
		f.onRead(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, key)
	if obj, ok, err := f.scriptedGet(key, ns, name); ok {
		return obj, err
	}
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	obj, ok := f.objs[objKey(gvr.Resource, ns, name)]
	if !ok {
		return nil, clierr.New(clierr.CodeNotFound, gvr.Resource+" "+name+" not found")
	}
	return obj.DeepCopy(), nil
}

func (f *appFake) List(_ context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	f.checkSecret(gvr)
	key := "list:" + gvr.Resource + "/" + ns
	if f.onRead != nil {
		f.onRead(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, key)
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	prefix := gvr.Resource + "/" + ns + "/"
	for _, k := range slices.Sorted(maps.Keys(f.objs)) {
		if strings.HasPrefix(k, prefix) {
			list.Items = append(list.Items, *f.objs[k].DeepCopy())
		}
	}
	return list, nil
}

func (f *appFake) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	f.t.Error("an app command wrote through the Kubernetes client")
	return nil, errors.New("no write")
}

func (f *appFake) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	f.t.Error("an app command wrote through the Kubernetes client")
	return nil, errors.New("no write")
}

func (f *appFake) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	f.t.Error("an app command wrote through the Kubernetes client")
	return errors.New("no write")
}

func (f *appFake) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	f.t.Error("an app command read logs")
	return "", errors.New("no logs")
}

func (f *appFake) PostReview(_ context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	if f.review != nil {
		return engine.ReviewResponse{}, f.review
	}
	out := req.Object.DeepCopy()
	_ = unstructured.SetNestedField(out.Object, f.allowed, "status", "allowed")
	return engine.ReviewResponse{Status: http.StatusCreated, Object: out}, nil
}

func (f *appFake) ServerResources(context.Context) ([]engine.APIResource, error) {
	return f.resources, f.discoveryErr
}

// PostAction records the request after the hooks and answers it.
func (f *appFake) PostAction(_ context.Context, path string, body []byte) ([]byte, error) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		f.t.Errorf("the action body is not a JSON object: %v", err)
	}
	f.checkBody(path, decoded)
	f.mu.Lock()
	f.posts = append(f.posts, post{path: path, body: decoded})
	f.mu.Unlock()
	return f.postAnswer(path)
}

func (f *appFake) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// checkBody is the request hook every POST passes: the body's fields are
// fields of Rancher's action types for the fake's edition, and an install's
// or upgrade's chart entry carries the request id and the source
// annotations. An upgrade body never carries install, which Rancher sets.
func (f *appFake) checkBody(path string, body map[string]any) {
	f.t.Helper()
	action, chartType := "ChartUninstallAction", ""
	switch {
	case strings.HasSuffix(path, "?action=install"):
		action, chartType = "ChartInstallAction", "ChartInstall"
	case strings.HasSuffix(path, "?action=upgrade"):
		action, chartType = "ChartUpgradeAction", "ChartUpgrade"
		if _, ok := body["install"]; ok {
			f.t.Errorf("upgrade body carries install, which Rancher always sets")
		}
	}
	f.checkFields(action, body)
	if chartType == "" {
		return
	}
	charts, _ := body["charts"].([]any)
	if len(charts) == 0 {
		f.t.Errorf("%s body has no chart entry", chartType)
	}
	for _, c := range charts {
		chart, _ := c.(map[string]any)
		f.checkFields(chartType, chart)
		ann, _ := chart["annotations"].(map[string]any)
		if id, _ := ann[annotationRequestID].(string); id == "" {
			f.t.Errorf("%s chart entry has no %s annotation", chartType, annotationRequestID)
		}
		if ann[annotationSourceRepoType] != sourceRepoTypeCluster || ann[annotationSourceRepo] == nil || ann[annotationSourceRepo] == "" {
			f.t.Errorf("%s chart entry lacks the source annotations: %v", chartType, ann)
		}
	}
}

func (f *appFake) checkFields(typ string, obj map[string]any) {
	f.t.Helper()
	for field := range obj {
		if !slices.Contains(f.fields[typ][field], f.edition) {
			f.t.Errorf("%s field %q is not a field of %s for the %s edition", typ, field, typ, f.edition)
		}
	}
}

// goldenActionFields reads the catalog action types of the Rancher API
// fields golden: type, then field, then the editions that have it.
func goldenActionFields(t *testing.T) map[string]map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "cli", "testdata", "rancher-api-fields.golden.json"))
	if err != nil {
		t.Fatalf("reading the Rancher API fields golden: %v", err)
	}
	var g struct {
		CatalogActions []struct {
			Type   string              `json:"type"`
			Fields map[string][]string `json:"fields"`
		} `json:"catalogActions"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("decoding the Rancher API fields golden: %v", err)
	}
	out := map[string]map[string][]string{}
	for _, a := range g.CatalogActions {
		out[a.Type] = a.Fields
	}
	return out
}

// newTestInstall is app install over f, with a fixed request id and clock.
func newTestInstall(f *appFake, src engine.FileSource) *installCmd {
	if src == nil {
		src = files{}
	}
	return &installCmd{clients: f, roots: f, actions: f, src: src,
		now: func() time.Time { return testNow }, newID: func() string { return testRequestID }, interval: time.Second}
}

// newTestUpgrade is app upgrade over f, with a fixed request id and clock.
func newTestUpgrade(f *appFake, src engine.FileSource) *upgradeCmd {
	if src == nil {
		src = files{}
	}
	return &upgradeCmd{clients: f, roots: f, actions: f, src: src,
		now: func() time.Time { return testNow }, newID: func() string { return testRequestID }, interval: time.Second}
}

func newTestUninstall(f *appFake) *uninstallCmd {
	return &uninstallCmd{clients: f, actions: f, now: func() time.Time { return testNow }, interval: time.Second}
}

var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// plan runs a Plan and fails the test on an error.
func plan(t *testing.T, cmd runtime.MutatingCommand, in runtime.Input) (safety.Effect, runtime.ExecuteFunc) {
	t.Helper()
	eff, exec, err := cmd.Plan(t.Context(), in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return eff, exec
}

// operation is an Operation for release created at created, with the
// given Reconciling status ("" for no condition) and generations.
func operation(release string, created time.Time, reconciling string, generation, observed int64) map[string]any {
	status := map[string]any{"releaseName": release, "namespace": testNS, "observedGeneration": observed}
	if reconciling != "" {
		status["conditions"] = []any{map[string]any{"type": "Reconciling", "status": reconciling}}
	}
	return map[string]any{
		"metadata": map[string]any{"creationTimestamp": created.Format(time.RFC3339), "generation": generation},
		"status":   status,
	}
}
