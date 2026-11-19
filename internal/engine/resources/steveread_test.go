package resources

import (
	"context"
	"encoding/json"
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
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	steveClustersPath = "/v1/management.cattle.io.clusters"
	steveProjectsPath = "/v1/management.cattle.io.projects"
)

// steveRoot is the root client of a test: it responds to each GET with what
// serve returns, and records the paths.
type steveRoot struct {
	mu    sync.Mutex
	paths []string
	// serve returns the response to one request: the status, and for a 2xx the
	// body to encode.
	// n counts the requests for that path, from 1.
	serve func(path string, n int) (status int, body any)
	seen  map[string]int
}

func (r *steveRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[path]++
	status, body := r.serve(path, r.seen[path])
	if status >= 200 && status < 300 {
		return json.Marshal(body)
	}
	return nil, clierr.FromResponse("rancher root GET "+path, http.MethodGet, status, nil, &clierr.Request{})
}

func (r *steveRoot) requested() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

// steveFactory is a factory whose root client is root and that counts the
// roots it hands out. It lists projects as the caller can read them.
type steveFactory struct {
	recFactory
	root      *steveRoot
	rootCalls int
	projects  []engine.ProjectRef
}

func (f *steveFactory) Root() (engine.RootClient, error) {
	f.rootCalls++
	return f.root, nil
}

func (f *steveFactory) ListProjects(context.Context, string) ([]engine.ProjectRef, error) {
	return f.projects, nil
}

var (
	_ engine.RootClientFactory = (*steveFactory)(nil)
	_ engine.ProjectNameLister = (*steveFactory)(nil)
)

// refusedClient responds to a List with listErr and, through memClient, to a
// Get with getErr; it counts the calls.
type refusedClient struct {
	memClient
	listErr   error
	listCalls int
	getCalls  int
}

func (c *refusedClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	c.listCalls++
	return nil, c.listErr
}

func (c *refusedClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	c.getCalls++
	return c.memClient.Get(ctx, gvr, ns, name)
}

// watchRefusedClient is a refusedClient that lists and watches one object:
// its List of the object is refused too.
type watchRefusedClient struct {
	refusedClient
	listObjects int
}

func (c *watchRefusedClient) ListObject(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, string, error) {
	c.listObjects++
	return nil, "", c.listErr
}

func (c *watchRefusedClient) WatchObject(context.Context, schema.GroupVersionResource, string, string, string, time.Duration) (<-chan engine.ObjectEvent, error) {
	return nil, engine.ErrWatchUnavailable
}

// kubernetesStatus is the error the Kubernetes client returns for a refusal
// with status: a clierr.Error with the client-go status as its cause.
func kubernetesStatus(code int32, withHint bool) *clierr.Error {
	status := &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code, Message: "the server said no"}}
	e := clierr.New(clierr.CodeRBAC, "kubernetes responded with "+http.StatusText(int(code))).WithCause(status)
	if withHint {
		e = e.WithHintCommand(clierr.NewCommand("access clusters"))
	}
	return e
}

func steveCollection(items ...map[string]any) map[string]any {
	data := make([]any, len(items))
	for i, item := range items {
		data[i] = item
	}
	return map[string]any{"data": data, "count": len(items), "revision": "1", "type": "collection"}
}

// steveClusterItem is a cluster as Steve returns it: the Kubernetes object plus
// what Steve adds.
func steveClusterItem(id, display, ready string, labels map[string]any) map[string]any {
	meta := map[string]any{
		"name": id, "fields": []any{id}, "relationships": []any{},
		"state": map[string]any{"name": "active"},
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"id": id, "_id": id, "type": "management.cattle.io.cluster", "links": map[string]any{"self": "x"}, "actions": map[string]any{},
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": meta,
		"spec":     map[string]any{"displayName": display},
		"status": map[string]any{
			"connected": true,
			"conditions": []any{
				map[string]any{"type": "Ready", "status": ready, "error": false, "transitioning": false, "lastUpdateTime": "2026-01-01T00:00:00Z"},
				map[string]any{"type": "PreBootstrapped", "status": "True", "error": false, "transitioning": false, "lastUpdateTime": ""},
			},
		},
	}
}

func steveProjectItem(cluster, id, display, state string, labels map[string]any) map[string]any {
	meta := map[string]any{"name": id, "namespace": cluster, "fields": []any{id}, "state": map[string]any{"name": state}}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"id": cluster + "/" + id, "_id": cluster + "/" + id, "type": "management.cattle.io.project", "links": map[string]any{}, "actions": map[string]any{},
		"apiVersion": "management.cattle.io/v3", "kind": "Project",
		"metadata": meta,
		"spec":     map[string]any{"clusterName": cluster, "displayName": display},
		"status":   map[string]any{"state": state, "backingNamespace": cluster + "-" + id},
	}
}

// steveSingleObject returns item as Steve returns one object by id: with id and
// without _id, which only a collection item has.
func steveSingleObject(item map[string]any) map[string]any {
	out := maps.Clone(item)
	delete(out, "_id")
	return out
}

// steveItemShapes are the shapes of an object Steve returns: an item of a
// collection and one object by id. A test of a get of one object runs on both.
var steveItemShapes = []struct {
	name  string
	shape func(item map[string]any) map[string]any
}{
	{"collection item", func(item map[string]any) map[string]any { return item }},
	{"one object by id", steveSingleObject},
}

// kubernetesCluster is the cluster the Kubernetes API returns for the same
// input as steveClusterItem.
func kubernetesCluster(id, display, ready string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": ready}}},
	}}
}

// assertKubernetesShape asserts a decoded object has none of what Steve adds
// and keeps spec.
func assertKubernetesShape(t *testing.T, obj map[string]any) {
	t.Helper()
	for _, k := range []string{"id", "type", "links", "actions", "_id", "_type", "_links", "_actions"} {
		assert.NotContains(t, obj, k)
	}
	meta, ok := obj["metadata"].(map[string]any)
	require.True(t, ok)
	for _, k := range []string{"fields", "state", "relationships"} {
		assert.NotContains(t, meta, k)
	}
	assert.Contains(t, obj, "spec", "an unrelated field stays")
	if status, ok := obj["status"].(map[string]any); ok {
		assert.NotContains(t, status, "connected")
		if conds, ok := status["conditions"].([]any); ok {
			for _, c := range conds {
				assert.NotContains(t, c, "error")
				assert.NotContains(t, c, "transitioning")
				if cond, ok := c.(map[string]any); ok {
					assert.NotEqual(t, "", cond["lastUpdateTime"], "an empty lastUpdateTime is removed")
				}
			}
		}
	}
}

// recordedSteveBody reads the observation called name from file, a recorded
// access-downstream golden in the fidelity testdata, and returns its body.
func recordedSteveBody(t *testing.T, file, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testrancher", "fidelity", "testdata", "downstream", "access-downstream", file))
	require.NoError(t, err)
	var golden struct {
		Observations []struct {
			Name string         `json:"name"`
			Body map[string]any `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	for _, o := range golden.Observations {
		if o.Name == name {
			return o.Body
		}
	}
	require.FailNow(t, "the golden "+file+" has no observation named "+name)
	return nil
}

// runGet executes a get of kind key and returns its result.
func runGet(t *testing.T, key string, f engine.ClusterClientFactory, in input) (runtime.Result, error) {
	t.Helper()
	return NewGet(mustKind(t, key), f).(runtime.ReadCommand).Execute(t.Context(), in, nil)
}

// decodeData decodes a result's data as the structured formats render it.
func decodeData(t *testing.T, res runtime.Result) any {
	t.Helper()
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func decodeList(t *testing.T, res runtime.Result) []map[string]any {
	t.Helper()
	items, ok := decodeData(t, res).([]any)
	require.True(t, ok, "a list decodes to an array")
	out := make([]map[string]any, len(items))
	for i, item := range items {
		out[i], ok = item.(map[string]any)
		require.True(t, ok)
	}
	return out
}

func decodeOne(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	obj, ok := decodeData(t, res).(map[string]any)
	require.True(t, ok, "a get of one object decodes to an object")
	return obj
}

// namesOf lists <namespace>/<name> for an object with a namespace and the
// name for one without.
func namesOf(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if ns, ok := meta["namespace"].(string); ok {
			name = ns + "/" + name
		}
		out[i] = name
	}
	return out
}

// memberFixture is a client whose Kubernetes reads respond with 403 and a
// factory whose root responds with what serve returns.
func memberFixture(serve func(path string, n int) (int, any)) (*refusedClient, *steveFactory, *clierr.Error) {
	denied := kubernetesStatus(http.StatusForbidden, true)
	client := &refusedClient{listErr: denied, getErr: denied}
	return client, &steveFactory{client: client, root: &steveRoot{serve: serve}}, denied
}

func clustersResponse(items ...map[string]any) func(string, int) (int, any) {
	return func(path string, _ int) (int, any) {
		if path == steveClustersPath {
			return http.StatusOK, steveCollection(items...)
		}
		return http.StatusNotFound, nil
	}
}

// TestGetAnAdministratorReadsKubernetesOnly asserts that when the Kubernetes
// API returns the objects, get sends no request to Steve and returns the
// objects as the Kubernetes API returned them.
func TestGetAnAdministratorReadsKubernetesOnly(t *testing.T) {
	t.Parallel()
	objs := []unstructured.Unstructured{kubernetesCluster("c-a", "Alpha", "True")}
	client := &memClient{items: objs}
	f := &steveFactory{client: client, root: &steveRoot{serve: func(string, int) (int, any) { return http.StatusInternalServerError, nil }}}
	res, err := runGet(t, "managementcluster", f, input{})
	require.NoError(t, err)
	assert.Equal(t, []string{"c-a"}, namesOf(decodeList(t, res)))
	res, err = runGet(t, "managementcluster", f, input{args: []string{"c-a"}})
	require.NoError(t, err)
	assert.Equal(t, "c-a", decodeOne(t, res)["metadata"].(map[string]any)["name"])
	assert.Zero(t, f.rootCalls)

	for key, twin := range map[string]unstructured.Unstructured{
		"provisioningcluster": kubernetesProvisioningCluster("prod", "fleet-default", "True", nil),
		"etcdsnapshot":        kubernetesEtcdSnapshot(),
	} {
		client := &memClient{items: []unstructured.Unstructured{twin}}
		f := &steveFactory{client: client, root: &steveRoot{serve: func(string, int) (int, any) { return http.StatusInternalServerError, nil }}}
		res, err := runGet(t, key, f, input{})
		require.NoError(t, err, key)
		assert.Len(t, decodeList(t, res), 1, key)
		_, err = runGet(t, key, f, input{args: []string{twin.GetName()}})
		require.NoError(t, err, key)
		assert.Zero(t, f.rootCalls, key)
	}
}

// TestGetClustersAMemberReadsThroughSteve asserts a list the Kubernetes API
// refused with 403 is read from the Steve clusters collection, sorted by
// name, in the Kubernetes shape, with the table cells of the Kubernetes
// objects.
func TestGetClustersAMemberReadsThroughSteve(t *testing.T) {
	t.Parallel()
	client, f, _ := memberFixture(clustersResponse(
		steveClusterItem("c-b", "Beta", "False", nil), steveClusterItem("c-a", "Alpha", "True", nil)))
	res, err := runGet(t, "managementcluster", f, input{})
	require.NoError(t, err)
	assert.Equal(t, []string{steveClustersPath}, f.root.requested())
	assert.Equal(t, 1, client.listCalls)

	items := decodeList(t, res)
	assert.Equal(t, []string{"c-a", "c-b"}, namesOf(items))
	for _, item := range items {
		assertKubernetesShape(t, item)
	}

	want := newTable(mustKind(t, "managementcluster"), []unstructured.Unstructured{kubernetesCluster("c-a", "Alpha", "True"), kubernetesCluster("c-b", "Beta", "False")}, false)
	got, ok := res.Data.(table)
	require.True(t, ok)
	assert.Equal(t, want.Rows(), got.Rows())
	assert.Equal(t, want.WideRows(), got.WideRows())
}

// TestGetProjectsAMemberReadsThroughSteve asserts the projects of every
// cluster, and of one cluster with --cluster, are read from the Steve
// collection that names them, and are sorted by cluster then id.
func TestGetProjectsAMemberReadsThroughSteve(t *testing.T) {
	t.Parallel()
	serve := func(path string, _ int) (int, any) {
		switch path {
		case steveProjectsPath:
			return http.StatusOK, steveCollection(
				steveProjectItem("c-b", "p-1", "B1", "active", nil), steveProjectItem("c-a", "p-9", "A9", "active", nil),
				steveProjectItem("c-a", "p-2", "A2", "active", nil))
		case steveProjectsPath + "/c-a":
			return http.StatusOK, steveCollection(steveProjectItem("c-a", "p-9", "A9", "active", nil), steveProjectItem("c-a", "p-2", "A2", "active", nil))
		}
		return http.StatusNotFound, nil
	}
	_, f, _ := memberFixture(serve)
	res, err := runGet(t, "project", f, input{})
	require.NoError(t, err)
	items := decodeList(t, res)
	assert.Equal(t, []string{"c-a/p-2", "c-a/p-9", "c-b/p-1"}, namesOf(items))
	for _, item := range items {
		assertKubernetesShape(t, item)
		assert.Contains(t, item["metadata"], "namespace")
	}

	res, err = runGet(t, "project", f, input{s: map[string]string{"cluster": "c-a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c-a/p-2", "c-a/p-9"}, namesOf(decodeList(t, res)))
	assert.Equal(t, []string{steveProjectsPath, steveProjectsPath + "/c-a"}, f.root.requested())
}

// TestGetClusterAMemberReadsOneThroughSteve asserts a get of one cluster the
// Kubernetes API refused is read from the Steve cluster by id, in the
// Kubernetes shape.
func TestGetClusterAMemberReadsOneThroughSteve(t *testing.T) {
	t.Parallel()
	for _, s := range steveItemShapes {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			_, f, _ := memberFixture(func(path string, _ int) (int, any) {
				if path == steveClustersPath+"/c-a" {
					return http.StatusOK, s.shape(steveClusterItem("c-a", "Alpha", "True", nil))
				}
				return http.StatusNotFound, nil
			})
			res, err := runGet(t, "managementcluster", f, input{args: []string{"c-a"}})
			require.NoError(t, err)
			obj := decodeOne(t, res)
			assertKubernetesShape(t, obj)
			assert.Equal(t, "Alpha", obj["spec"].(map[string]any)["displayName"])
			assert.Equal(t, []string{steveClustersPath + "/c-a"}, f.root.requested())
		})
	}
}

// TestGetProjectAMemberReadsOneThroughSteve asserts a get of one project the
// Kubernetes API refused is read from the Steve project by cluster and id, that
// a Steve refusal returns the Kubernetes error, and that the error for a typed
// display name carries the lookup's message and candidates.
func TestGetProjectAMemberReadsOneThroughSteve(t *testing.T) {
	t.Parallel()
	in := input{args: []string{"p-1"}, s: map[string]string{"cluster": "c-a"}}
	serve := func(shape func(map[string]any) map[string]any) func(string, int) (int, any) {
		return func(path string, _ int) (int, any) {
			if path == steveProjectsPath+"/c-a/p-1" {
				return http.StatusOK, shape(steveProjectItem("c-a", "p-1", "One", "active", nil))
			}
			return http.StatusForbidden, nil
		}
	}
	for _, s := range steveItemShapes {
		_, f, _ := memberFixture(serve(s.shape))
		res, err := runGet(t, "project", f, in)
		require.NoError(t, err, s.name)
		obj := decodeOne(t, res)
		assertKubernetesShape(t, obj)
		assert.Equal(t, "p-1", obj["metadata"].(map[string]any)["name"], s.name)
	}

	_, f, _ := memberFixture(serve(steveSingleObject))
	f.projects = []engine.ProjectRef{{ID: "p-1", DisplayName: "One"}}
	_, err := runGet(t, "project", f, input{args: []string{"One"}, s: map[string]string{"cluster": "c-a"}})
	ce := asClierr(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	require.ErrorContains(t, err, "'One' is the display name of project p-1")
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "One"}}, ce.Candidates)
}

// TestGetThroughSteveReturnsTheKubernetesErrorWhenSteveRefuses asserts that
// when Steve responds with 403 or 404 the caller gets the Kubernetes API's own
// error, that another Steve status is Steve's error, and that no roles at all
// is an empty list.
func TestGetThroughSteveReturnsTheKubernetesErrorWhenSteveRefuses(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		client, f, denied := memberFixture(func(string, int) (int, any) { return status, nil })
		_, err := runGet(t, "managementcluster", f, input{})
		assert.Equal(t, error(denied), err, "list, steve %d", status)
		assert.Equal(t, clierr.CodeRBAC, asClierr(t, err).Code)
		assert.Equal(t, clierr.Rancher+" access clusters", asClierr(t, err).Hint)
		_, err = runGet(t, "managementcluster", f, input{args: []string{"c-a"}})
		assert.Equal(t, error(denied), err, "one, steve %d", status)
		assert.Equal(t, 1, client.getCalls)
	}

	_, f, denied := memberFixture(func(string, int) (int, any) { return http.StatusInternalServerError, nil })
	_, err := runGet(t, "managementcluster", f, input{})
	require.Error(t, err)
	assert.NotEqual(t, error(denied), err)
	st, ok := clierr.HTTPStatus(err)
	assert.True(t, ok)
	assert.Equal(t, http.StatusInternalServerError, st)

	empty := recordedSteveBody(t, "standard-steve-empty.json", "clusters")
	_, f, _ = memberFixture(func(string, int) (int, any) { return http.StatusOK, empty })
	res, err := runGet(t, "managementcluster", f, input{})
	require.NoError(t, err)
	assert.Empty(t, decodeList(t, res))
}

// TestGetReadsSteveOnlyForAKubernetes403 asserts a Kubernetes 401, 404 or 500
// is returned unchanged and sends nothing to Steve.
func TestGetReadsSteveOnlyForAKubernetes403(t *testing.T) {
	t.Parallel()
	for _, code := range []int32{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		failure := kubernetesStatus(code, false)
		client := &refusedClient{listErr: failure, getErr: failure}
		f := &steveFactory{client: client, root: &steveRoot{serve: clustersResponse(steveClusterItem("c-a", "Alpha", "True", nil))}}
		_, err := runGet(t, "managementcluster", f, input{})
		assert.Equal(t, error(failure), err, "list %d", code)
		_, err = runGet(t, "managementcluster", f, input{args: []string{"c-a"}})
		assert.Equal(t, error(failure), err, "one %d", code)
		assert.Zero(t, f.rootCalls, "status %d", code)
	}
}

// TestGetThroughSteveAppliesTheSelector asserts the CLI keeps the items whose
// labels match --selector, never sends a selector to Steve, and refuses an
// invalid selector before any request.
func TestGetThroughSteveAppliesTheSelector(t *testing.T) {
	t.Parallel()
	_, f, _ := memberFixture(clustersResponse(
		steveClusterItem("c-a", "Alpha", "True", map[string]any{"team": "a"}),
		steveClusterItem("c-b", "Beta", "True", map[string]any{"team": "b"}),
		steveClusterItem("c-c", "Gamma", "True", nil)))
	res, err := runGet(t, "managementcluster", f, input{s: map[string]string{"selector": "team=a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c-a"}, namesOf(decodeList(t, res)))
	res, err = runGet(t, "managementcluster", f, input{s: map[string]string{"selector": "team!=a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c-b", "c-c"}, namesOf(decodeList(t, res)))
	assert.Equal(t, []string{steveClustersPath, steveClustersPath}, f.root.requested(), "no query: Steve is never sent a selector")

	client, f2, _ := memberFixture(clustersResponse())
	_, err = runGet(t, "managementcluster", f2, input{s: map[string]string{"selector": "team in ("}})
	require.ErrorContains(t, err, "is not a valid label selector")
	assert.Zero(t, client.listCalls)
	assert.Empty(t, f2.root.requested())
}

// readyAfter returns the Steve cluster c-a with Ready False until the nth
// request and True from it.
func readyAfter(n int, shape func(map[string]any) map[string]any) func(string, int) (int, any) {
	return func(path string, count int) (int, any) {
		if path != steveClustersPath+"/c-a" {
			return http.StatusNotFound, nil
		}
		ready := "False"
		if count >= n {
			ready = "True"
		}
		return http.StatusOK, shape(steveClusterItem("c-a", "Alpha", ready, nil))
	}
}

func waitInput(args []string, extra map[string]string) input {
	return input{args: args, b: map[string]bool{"wait": true}, s: mergeStrings(map[string]string{"timeout": "30s"}, extra)}
}

// TestGetWaitAMemberPollsTheSteveObject asserts a wait the Kubernetes API
// refuses polls the Steve object by id until the condition holds, sends no
// Steve request for a list or a watch, asks the Kubernetes API for the object
// once, and returns the Kubernetes shape.
func TestGetWaitAMemberPollsTheSteveObject(t *testing.T) {
	withFastPoll(t)
	for name, mk := range map[string]func(denied error) (engine.ClusterClient, func() (gets, lists int)){
		"a client that watches": func(denied error) (engine.ClusterClient, func() (int, int)) {
			c := &watchRefusedClient{listErr: denied, getErr: denied}
			return c, func() (int, int) { return c.getCalls, c.listObjects }
		},
		"a client that polls": func(denied error) (engine.ClusterClient, func() (int, int)) {
			c := &refusedClient{listErr: denied, getErr: denied}
			return c, func() (int, int) { return c.getCalls, 0 }
		},
	} {
		for _, s := range steveItemShapes {
			t.Run(name+", "+s.name, func(t *testing.T) {
				denied := kubernetesStatus(http.StatusForbidden, true)
				client, counts := mk(denied)
				f := &steveFactory{client: client, root: &steveRoot{serve: readyAfter(3, s.shape)}}
				res, err := runGet(t, "managementcluster", f, waitInput([]string{"c-a"}, nil))
				require.NoError(t, err)
				assertKubernetesShape(t, decodeOne(t, res))
				requested := f.root.requested()
				assert.GreaterOrEqual(t, len(requested), 3)
				for _, p := range requested {
					assert.Equal(t, steveClustersPath+"/c-a", p)
				}
				gets, _ := counts()
				assert.Equal(t, 1, gets, "after the first 403 only Steve is asked")
				assert.NotEmpty(t, res.Summary)
			})
		}
	}
}

// TestGetWaitAMemberOnAProjectPollsTheSteveProject asserts the same wait on
// the project row, with --for state=<value>.
func TestGetWaitAMemberOnAProjectPollsTheSteveProject(t *testing.T) {
	withFastPoll(t)
	for _, s := range steveItemShapes {
		t.Run(s.name, func(t *testing.T) {
			_, f, _ := memberFixture(func(path string, n int) (int, any) {
				if path != steveProjectsPath+"/c-a/p-1" {
					return http.StatusNotFound, nil
				}
				state := "updating"
				if n >= 2 {
					state = "active"
				}
				return http.StatusOK, s.shape(steveProjectItem("c-a", "p-1", "One", state, nil))
			})
			res, err := runGet(t, "project", f, waitInput([]string{"p-1"}, map[string]string{"cluster": "c-a", "for": "state=active"}))
			require.NoError(t, err)
			obj := decodeOne(t, res)
			assertKubernetesShape(t, obj)
			assert.Equal(t, "active", obj["status"].(map[string]any)["state"])
			for _, p := range f.root.requested() {
				assert.Equal(t, steveProjectsPath+"/c-a/p-1", p)
			}
		})
	}
}

// TestGetWaitSteve404EndsTheWait asserts a Steve 404 on the polled object
// ends the wait at once with NOT_FOUND, on both rows, and that a Steve 403 is
// the Kubernetes error the wait treats as transient.
func TestGetWaitSteve404EndsTheWait(t *testing.T) {
	withFastPoll(t)
	for _, tc := range []struct {
		key  string
		in   input
		hint string
	}{
		{"managementcluster", waitInput([]string{"c-a"}, nil), clierr.Rancher + " access clusters"},
		{"project", waitInput([]string{"p-1"}, map[string]string{"cluster": "c-a", "for": "state=active"}), clierr.Rancher + " access projects --cluster 'c-a'"},
	} {
		_, f, _ := memberFixture(func(string, int) (int, any) { return http.StatusNotFound, nil })
		start := time.Now()
		_, err := runGet(t, tc.key, f, tc.in)
		require.Error(t, err)
		assert.True(t, clierr.IsNotFound(err), tc.key)
		assert.Equal(t, clierr.CodeNotFound, asClierr(t, err).Code)
		assert.Equal(t, tc.hint, asClierr(t, err).Hint)
		assert.Less(t, time.Since(start), 10*time.Second, "the wait ends at once, not at the timeout")

		_, f, denied := memberFixture(func(string, int) (int, any) { return http.StatusForbidden, nil })
		tc.in.s = mergeStrings(tc.in.s, map[string]string{"timeout": "100ms"})
		_, err = runGet(t, tc.key, f, tc.in)
		require.Error(t, err)
		assert.False(t, clierr.IsNotFound(err))
		require.ErrorContains(t, err, "timed out waiting")
		require.ErrorContains(t, err, denied.Message)
	}
}

// TestWrapSteveWaitImplementsObjectWatcherOnlyForAWatchingClient asserts the
// wrapper of a client that lists and watches one object is an
// engine.ObjectWatcher and the wrapper of any other client is not, and that a
// row without the field is not wrapped.
func TestWrapSteveWaitImplementsObjectWatcherOnlyForAWatchingClient(t *testing.T) {
	t.Parallel()
	cluster := getCmd{k: mustKind(t, "managementcluster")}
	_, watching := wrapSteveWait(cluster, &watchRefusedClient{}, "").(engine.ObjectWatcher)
	assert.True(t, watching)
	_, watching = wrapSteveWait(cluster, &refusedClient{}, "").(engine.ObjectWatcher)
	assert.False(t, watching)

	plain := &refusedClient{}
	assert.Same(t, plain, wrapSteveWait(getCmd{k: mustKind(t, "user")}, plain, ""))
}

// TestGetThroughSteveNeedsTheRootClient asserts a factory that cannot hand out
// the root client leaves the Kubernetes error as it is.
func TestGetThroughSteveNeedsTheRootClient(t *testing.T) {
	t.Parallel()
	denied := kubernetesStatus(http.StatusForbidden, true)
	client := &refusedClient{listErr: denied, getErr: denied}
	_, err := runGet(t, "managementcluster", &recFactory{client: client}, input{})
	assert.Equal(t, error(denied), err)
	_, err = runGet(t, "managementcluster", &recFactory{client: client}, input{args: []string{"c-a"}})
	assert.Equal(t, error(denied), err)
}

// rowReadFactory is a factory whose root counts its requests and responds to
// each with 500.
func rowReadFactory(client engine.ClusterClient) (*steveFactory, *steveRoot) {
	root := &steveRoot{serve: func(string, int) (int, any) { return http.StatusInternalServerError, nil }}
	return &steveFactory{client: client, root: root}, root
}

// TestNoOtherRowReadsThroughSteve asserts that, for every row of the kind
// table, steveList, steveGet (in a get and in a wait) and the Get of a
// steveWaitClient return a Kubernetes 403 unchanged and ask for no root
// client, except on the rows that set SteveReadOnForbidden, which ask for it
// in each, return Steve's error (never the internal error of a row with no
// reader) and read only the collection or object of their own Steve type.
func TestNoOtherRowReadsThroughSteve(t *testing.T) {
	t.Parallel()
	denied := kubernetesStatus(http.StatusForbidden, true)
	for _, k := range kinds.All() {
		reads := map[string]func(c getCmd, client engine.ClusterClient) error{
			"list": func(c getCmd, _ engine.ClusterClient) error {
				_, err := c.steveList(t.Context(), "", nil, denied)
				return err
			},
			"get": func(c getCmd, _ engine.ClusterClient) error {
				_, err := c.steveGet(t.Context(), "c-a", "x", false, denied)
				return err
			},
			"get in a wait": func(c getCmd, _ engine.ClusterClient) error {
				_, err := c.steveGet(t.Context(), "c-a", "x", true, denied)
				return err
			},
			"wait client": func(c getCmd, client engine.ClusterClient) error {
				_, err := newSteveWaitClient(c, client, "c-a").Get(t.Context(), k.GVR, "c-a", "x")
				return err
			},
		}
		for name, read := range reads {
			client := &refusedClient{listErr: denied, getErr: denied}
			f, root := rowReadFactory(client)
			err := read(getCmd{k: k, clients: f}, client)
			require.Error(t, err, "%s of %s", name, k.Key)
			if k.SteveReadOnForbidden {
				assert.NotZero(t, f.rootCalls, "%s of %s reads through Steve", name, k.Key)
				assert.NotEmpty(t, root.requested())
				assert.NotContains(t, err.Error(), "internal error", "%s of %s has a Steve reader", name, k.Key)
				st, ok := clierr.HTTPStatus(err)
				assert.True(t, ok, "%s of %s returns the error of Steve's response", name, k.Key)
				assert.Equal(t, http.StatusInternalServerError, st, "%s of %s", name, k.Key)
				for _, p := range root.requested() {
					assert.True(t, strings.HasPrefix(p, "/v1/"+k.SteveType()), "%s of %s reads %s, not its own Steve type %s", name, k.Key, p, k.SteveType())
				}
				continue
			}
			assert.Equal(t, error(denied), err, "%s of %s returns the Kubernetes error", name, k.Key)
			assert.Zero(t, f.rootCalls, "%s of %s must not ask for the root client", name, k.Key)
		}
	}
}

const (
	steveProvisioningClustersPath = "/v1/provisioning.cattle.io.clusters"
	steveEtcdSnapshotsPath        = "/v1/rke.cattle.io.etcdsnapshots"
	etcdSnapshotName              = "prod-etcd-snapshot-node-1-1794614404-abcde"
)

// steveProvisioningCluster is a provisioning cluster as Steve returns it: the
// Kubernetes object plus what Steve adds.
func steveProvisioningCluster(name, ns, ready string, labels map[string]any) map[string]any {
	meta := map[string]any{
		"name": name, "namespace": ns, "fields": []any{name, true}, "relationships": []any{},
		"state": map[string]any{"name": "active"},
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"id": ns + "/" + name, "_id": ns + "/" + name, "type": "provisioning.cattle.io.cluster", "links": map[string]any{"self": "x"},
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": meta,
		"spec":     map[string]any{"kubernetesVersion": "v1.33.1+rke2r1"},
		"status": map[string]any{
			"clusterName": "c-m-abc", "ready": true,
			"conditions": []any{
				map[string]any{"type": "Ready", "status": ready, "error": false, "transitioning": false, "lastUpdateTime": "2026-01-01T00:00:00Z"},
			},
		},
	}
}

// kubernetesProvisioningCluster is the provisioning cluster the Kubernetes API
// returns for the same input as steveProvisioningCluster.
func kubernetesProvisioningCluster(name, ns, ready string, labels map[string]any) unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": ns}
	if labels != nil {
		meta["labels"] = labels
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": meta,
		"spec":     map[string]any{"kubernetesVersion": "v1.33.1+rke2r1"},
		"status": map[string]any{
			"clusterName": "c-m-abc", "ready": true,
			"conditions": []any{map[string]any{"type": "Ready", "status": ready, "lastUpdateTime": "2026-01-01T00:00:00Z"}},
		},
	}}
}

// steveEtcdSnapshot is an etcd snapshot as Steve returns it.
func steveEtcdSnapshot() map[string]any {
	return map[string]any{
		"id": "fleet-default/" + etcdSnapshotName, "_id": "fleet-default/" + etcdSnapshotName, "type": "rke.cattle.io.etcdsnapshot",
		"links":      map[string]any{"self": "x"},
		"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
		"metadata": map[string]any{
			"name": etcdSnapshotName, "namespace": "fleet-default", "fields": []any{etcdSnapshotName}, "relationships": nil,
			"state": map[string]any{"name": "active"},
		},
		"spec":         map[string]any{"clusterName": "prod"},
		"snapshotFile": map[string]any{"name": "etcd-snapshot-node-1-1794614404", "nodeName": "node-1", "size": float64(1048576), "createdAt": "2026-01-02T00:00:00Z", "status": "successful"},
	}
}

// kubernetesEtcdSnapshot is the etcd snapshot the Kubernetes API returns for
// the same input as steveEtcdSnapshot.
func kubernetesEtcdSnapshot() unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rke.cattle.io/v1", "kind": "ETCDSnapshot",
		"metadata":     map[string]any{"name": etcdSnapshotName, "namespace": "fleet-default"},
		"spec":         map[string]any{"clusterName": "prod"},
		"snapshotFile": map[string]any{"name": "etcd-snapshot-node-1-1794614404", "nodeName": "node-1", "size": int64(1048576), "createdAt": "2026-01-02T00:00:00Z", "status": "successful"},
	}}
}

// typeResponse returns the collection items at each path of collections as
// Steve does; any other path responds with 404.
func typeResponse(collections map[string][]map[string]any) func(string, int) (int, any) {
	return func(path string, _ int) (int, any) {
		if items, ok := collections[path]; ok {
			return http.StatusOK, steveCollection(items...)
		}
		return http.StatusNotFound, nil
	}
}

// assertNoSteveFields asserts each decoded item has none of the fields Steve
// adds.
func assertNoSteveFields(t *testing.T, items []map[string]any) {
	t.Helper()
	for _, item := range items {
		for _, k := range []string{"id", "_id", "type", "links", "actions"} {
			assert.NotContains(t, item, k)
		}
		meta, ok := item["metadata"].(map[string]any)
		require.True(t, ok)
		for _, k := range []string{"fields", "state", "relationships"} {
			assert.NotContains(t, meta, k)
		}
	}
}

// TestGetProvisioningClustersAnOwnerReadsThroughSteve asserts a list of
// provisioning clusters the Kubernetes API refused with 403 is read from the
// Steve collection of the namespace, sorted by name, in the Kubernetes shape,
// with the table cells of the Kubernetes objects, and that --namespace names
// the namespace of the request.
func TestGetProvisioningClustersAnOwnerReadsThroughSteve(t *testing.T) {
	t.Parallel()
	client, f, _ := memberFixture(typeResponse(map[string][]map[string]any{
		steveProvisioningClustersPath + "/fleet-default": {steveProvisioningCluster("prod", "fleet-default", "True", nil)},
		steveProvisioningClustersPath + "/team-a": {
			steveProvisioningCluster("web", "team-a", "False", nil), steveProvisioningCluster("api", "team-a", "True", nil),
		},
	}))
	res, err := runGet(t, "provisioningcluster", f, input{})
	require.NoError(t, err)
	items := decodeList(t, res)
	assert.Equal(t, []string{"fleet-default/prod"}, namesOf(items))
	assertNoSteveFields(t, items)
	assert.Equal(t, 1, client.listCalls)

	want := newTable(mustKind(t, "provisioningcluster"), []unstructured.Unstructured{kubernetesProvisioningCluster("prod", "fleet-default", "True", nil)}, false)
	got, ok := res.Data.(table)
	require.True(t, ok)
	assert.Equal(t, want.Rows(), got.Rows())
	assert.Equal(t, want.WideRows(), got.WideRows())

	res, err = runGet(t, "provisioningcluster", f, input{s: map[string]string{"namespace": "team-a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a/api", "team-a/web"}, namesOf(decodeList(t, res)))
	assert.Equal(t, []string{steveProvisioningClustersPath + "/fleet-default", steveProvisioningClustersPath + "/team-a"}, f.root.requested())
}

// TestGetEtcdSnapshotsAnOwnerReadsThroughSteve asserts a list of etcd
// snapshots the Kubernetes API refused with 403 has the snapshot Steve
// returns for the caller, in the Kubernetes shape, equal to the Kubernetes
// object, and that the columns STATUS, CLUSTER, NODE, SIZE-BYTES and
// CREATED-AT read their values (SIZE-BYTES as a whole number, as when the
// Kubernetes API returns the snapshot).
func TestGetEtcdSnapshotsAnOwnerReadsThroughSteve(t *testing.T) {
	t.Parallel()
	_, f, _ := memberFixture(typeResponse(map[string][]map[string]any{
		steveEtcdSnapshotsPath + "/fleet-default": {steveEtcdSnapshot()},
	}))
	res, err := runGet(t, "etcdsnapshot", f, input{})
	require.NoError(t, err)
	items := decodeList(t, res)
	require.Len(t, items, 1)
	assertNoSteveFields(t, items)
	twin := kubernetesEtcdSnapshot()
	var twinJSON map[string]any
	raw, err := json.Marshal(twin.Object)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &twinJSON))
	assert.Equal(t, twinJSON, items[0])
	assert.Equal(t, []string{steveEtcdSnapshotsPath + "/fleet-default"}, f.root.requested())

	got, ok := res.Data.(table)
	require.True(t, ok)
	want := newTable(mustKind(t, "etcdsnapshot"), []unstructured.Unstructured{twin}, false)
	assert.Equal(t, want.Rows(), got.Rows())
	require.Len(t, got.Rows(), 1)
	assert.Equal(t, []string{etcdSnapshotName, "successful", "prod", "node-1", "1048576", "2026-01-02T00:00:00Z"}, got.Rows()[0][:6], "NAME, STATUS, CLUSTER, NODE, SIZE-BYTES, CREATED-AT")
}

// TestGetProvisioningClusterAndEtcdSnapshotAnOwnerReadsOneThroughSteve asserts
// a get of one object the Kubernetes API refused with 403 is read from the
// Steve object by namespace and name, in the Kubernetes shape, and that a
// Steve 403 or 404 returns the Kubernetes error.
func TestGetProvisioningClusterAndEtcdSnapshotAnOwnerReadsOneThroughSteve(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key, path, name string
		item            map[string]any
	}{
		{"provisioningcluster", steveProvisioningClustersPath + "/fleet-default/prod", "prod", steveProvisioningCluster("prod", "fleet-default", "True", nil)},
		{"etcdsnapshot", steveEtcdSnapshotsPath + "/fleet-default/" + etcdSnapshotName, etcdSnapshotName, steveEtcdSnapshot()},
	} {
		for _, s := range steveItemShapes {
			t.Run(tc.key+", "+s.name, func(t *testing.T) {
				t.Parallel()
				_, f, _ := memberFixture(func(path string, _ int) (int, any) {
					if path == tc.path {
						return http.StatusOK, s.shape(tc.item)
					}
					return http.StatusNotFound, nil
				})
				res, err := runGet(t, tc.key, f, input{args: []string{tc.name}})
				require.NoError(t, err)
				obj := decodeOne(t, res)
				assertNoSteveFields(t, []map[string]any{obj})
				assert.Equal(t, tc.name, obj["metadata"].(map[string]any)["name"])
				assert.Equal(t, []string{tc.path}, f.root.requested())
			})
		}
		for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
			client, f, denied := memberFixture(func(string, int) (int, any) { return status, nil })
			_, err := runGet(t, tc.key, f, input{args: []string{tc.name}})
			assert.Equal(t, error(denied), err, "%s, steve %d", tc.key, status)
			assert.Equal(t, 1, client.getCalls)
			_, err = runGet(t, tc.key, f, input{})
			assert.Equal(t, error(denied), err, "%s list, steve %d", tc.key, status)
		}
	}
}

// TestGetProvisioningClustersThroughSteveAppliesTheSelector asserts the CLI
// keeps the provisioning clusters whose labels match --selector and sends
// Steve no selector.
func TestGetProvisioningClustersThroughSteveAppliesTheSelector(t *testing.T) {
	t.Parallel()
	_, f, _ := memberFixture(typeResponse(map[string][]map[string]any{
		steveProvisioningClustersPath + "/fleet-default": {
			steveProvisioningCluster("a", "fleet-default", "True", map[string]any{"team": "a"}),
			steveProvisioningCluster("b", "fleet-default", "True", map[string]any{"team": "b"}),
		},
	}))
	res, err := runGet(t, "provisioningcluster", f, input{s: map[string]string{"selector": "team=a"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"fleet-default/a"}, namesOf(decodeList(t, res)))
	assert.Equal(t, []string{steveProvisioningClustersPath + "/fleet-default"}, f.root.requested(), "no query: Steve is never sent a selector")
}

// TestGetWaitAnOwnerOnAProvisioningClusterPollsTheSteveObject asserts a wait
// on a provisioning cluster whose Kubernetes get is refused with 403 polls the
// Steve object until the condition holds, and that a Steve 404 ends the wait
// with NOT_FOUND and the command that lists the provisioning clusters of the
// namespace.
func TestGetWaitAnOwnerOnAProvisioningClusterPollsTheSteveObject(t *testing.T) {
	withFastPoll(t)
	const path = steveProvisioningClustersPath + "/fleet-default/prod"
	for _, s := range steveItemShapes {
		t.Run(s.name, func(t *testing.T) {
			_, f, _ := memberFixture(func(p string, n int) (int, any) {
				if p != path {
					return http.StatusNotFound, nil
				}
				ready := "False"
				if n >= 2 {
					ready = "True"
				}
				return http.StatusOK, s.shape(steveProvisioningCluster("prod", "fleet-default", ready, nil))
			})
			res, err := runGet(t, "provisioningcluster", f, waitInput([]string{"prod"}, nil))
			require.NoError(t, err)
			assertNoSteveFields(t, []map[string]any{decodeOne(t, res)})
			for _, p := range f.root.requested() {
				assert.Equal(t, path, p)
			}
		})
	}

	_, f, _ := memberFixture(func(string, int) (int, any) { return http.StatusNotFound, nil })
	_, err := runGet(t, "provisioningcluster", f, waitInput([]string{"prod"}, map[string]string{"namespace": "team-a"}))
	require.Error(t, err)
	assert.True(t, clierr.IsNotFound(err))
	assert.Equal(t, clierr.Rancher+" get provisioning-clusters --namespace 'team-a'", asClierr(t, err).Hint)
}

// TestSteveNotFoundHintNamesTheListOfTheKind asserts the NOT_FOUND error of a
// Steve 404 during a wait names the command that lists the objects of the kind,
// in the namespace.
func TestSteveNotFoundHintNamesTheListOfTheKind(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]string{
		"managementcluster":   clierr.Rancher + " access clusters",
		"project":             clierr.Rancher + " access projects --cluster 'ns'",
		"provisioningcluster": clierr.Rancher + " get provisioning-clusters --namespace 'ns'",
		"etcdsnapshot":        clierr.Rancher + " get etcd-snapshots --namespace 'ns'",
	} {
		err := getCmd{k: mustKind(t, key)}.steveNotFound("ns", "x", nil)
		assert.Equal(t, want, asClierr(t, err).Hint, key)
		require.ErrorContains(t, err, "not found: 'x'")
	}
}

// TestEtcdSnapshotsAfterForbiddenReturnTheKubernetesShape asserts the reader
// lists the snapshots of the namespace through Steve after a Kubernetes 403
// and returns each in the shape the Kubernetes API returns.
func TestEtcdSnapshotsAfterForbiddenReturnTheKubernetesShape(t *testing.T) {
	t.Parallel()
	path := steveEtcdSnapshotsPath + "/fleet-default"
	root := &steveRoot{serve: func(p string, _ int) (int, any) {
		if p == path {
			return http.StatusOK, steveCollection(steveEtcdSnapshot())
		}
		return http.StatusForbidden, nil
	}}
	f := &steveFactory{root: root}

	got, err := EtcdSnapshotsAfterForbidden(t.Context(), f, "fleet-default", kubernetesStatus(http.StatusForbidden, false))
	require.NoError(t, err)
	assert.Equal(t, []string{path}, root.requested())
	assert.Equal(t, []unstructured.Unstructured{kubernetesEtcdSnapshot()}, got)
}

// TestEtcdSnapshotsAfterForbiddenKeepTheKubernetesError asserts the reader
// makes no Steve request for a Kubernetes error that is not a 403, returns the
// Kubernetes error when Steve responds with 403 or 404, and returns an error
// with the status Steve responded with for any other status.
func TestEtcdSnapshotsAfterForbiddenKeepTheKubernetesError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		k8sErr      error
		steve       int
		wantRequest bool
		wantK8s     bool
		wantStatus  int
	}{
		"Kubernetes 500": {k8sErr: kubernetesStatus(http.StatusInternalServerError, false), wantK8s: true},
		"Kubernetes 404": {k8sErr: kubernetesStatus(http.StatusNotFound, false), wantK8s: true},
		"no status":      {k8sErr: clierr.New(clierr.CodeGeneric, "connection refused"), wantK8s: true},
		"Steve 403":      {k8sErr: kubernetesStatus(http.StatusForbidden, false), steve: http.StatusForbidden, wantRequest: true, wantK8s: true},
		"Steve 404":      {k8sErr: kubernetesStatus(http.StatusForbidden, false), steve: http.StatusNotFound, wantRequest: true, wantK8s: true},
		"Steve 500":      {k8sErr: kubernetesStatus(http.StatusForbidden, false), steve: http.StatusInternalServerError, wantRequest: true, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := &steveRoot{serve: func(string, int) (int, any) { return tc.steve, nil }}
			got, err := EtcdSnapshotsAfterForbidden(t.Context(), &steveFactory{root: root}, "fleet-default", tc.k8sErr)
			assert.Nil(t, got)
			require.Error(t, err)
			assert.Equal(t, tc.wantRequest, len(root.requested()) == 1, "the Steve request")
			if tc.wantK8s {
				assert.Same(t, tc.k8sErr, err)
				return
			}
			status, ok := clierr.HTTPStatus(err)
			require.True(t, ok)
			assert.Equal(t, tc.wantStatus, status)
		})
	}
}

// TestEtcdSnapshotsAfterForbiddenReadNothingForARowWithoutTheFlag asserts the
// reader returns the Kubernetes 403 and makes no Steve request when the row
// does not set SteveReadOnForbidden.
func TestEtcdSnapshotsAfterForbiddenReadNothingForARowWithoutTheFlag(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "etcdsnapshot")
	require.True(t, k.SteveReadOnForbidden)
	k.SteveReadOnForbidden = false
	root := &steveRoot{serve: func(string, int) (int, any) { return http.StatusOK, steveCollection(steveEtcdSnapshot()) }}
	k8sErr := kubernetesStatus(http.StatusForbidden, false)

	got, err := steveEtcdSnapshots(t.Context(), k, &steveFactory{root: root}, "fleet-default", k8sErr)
	assert.Nil(t, got)
	assert.Same(t, k8sErr, err)
	assert.Empty(t, root.requested())
}
