package observe

// Deliberately plain-stdlib (no testify): shared test helpers without
// assertions, nothing to convert.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// input is the runtime.Input fake for this package's tests.
type input struct {
	args []string
	s    map[string]string
	b    map[string]bool
}

func (i input) String(k string) string  { return i.s[k] }
func (i input) Bool(k string) bool      { return i.b[k] }
func (i input) Strings(string) []string { return nil }
func (i input) IsSet(string) bool       { return false }
func (i input) Args() []string          { return i.args }

type objKey struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

// fakeClient is a table-driven engine.ClusterClient fake: objects keyed by
// GVR+ns+name serve Get; lists keyed by GVR+ns serve List (the selector is
// recorded, not filtered - tests assert on the recorded selector directly).
type selKey struct {
	gvr      schema.GroupVersionResource
	ns       string
	selector string
}

type fakeClient struct {
	objects map[objKey]*unstructured.Unstructured
	getErrs map[objKey]error

	listsByNS       map[objKey]*unstructured.UnstructuredList // key: {gvr, ns, ""}
	listsBySelector map[selKey]*unstructured.UnstructuredList // checked before listsByNS
	listErrs        map[objKey]error
	listSelectors   []string
	listNamespaces  []string

	// denied fails every Get and List of a kind with its error;
	// kindReads is the kind of every Get and List, in order.
	denied    map[schema.GroupVersionResource]error
	kindReads []schema.GroupVersionResource

	logs         map[string]string
	logErrs      map[string]error
	prevLogErrs  map[string]error
	logOptsCalls []engine.LogOptions
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		objects:         map[objKey]*unstructured.Unstructured{},
		getErrs:         map[objKey]error{},
		listsByNS:       map[objKey]*unstructured.UnstructuredList{},
		listsBySelector: map[selKey]*unstructured.UnstructuredList{},
		listErrs:        map[objKey]error{},
		denied:          map[schema.GroupVersionResource]error{},
		logs:            map[string]string{},
		logErrs:         map[string]error{},
		prevLogErrs:     map[string]error{},
	}
}

func (f *fakeClient) withObject(gvr schema.GroupVersionResource, ns, name string, obj *unstructured.Unstructured) *fakeClient {
	f.objects[objKey{gvr, ns, name}] = obj
	return f
}

func (f *fakeClient) withListNS(gvr schema.GroupVersionResource, ns string, list *unstructured.UnstructuredList) *fakeClient {
	f.listsByNS[objKey{gvr, ns, ""}] = list
	return f
}

// withListSelector serves list only for List(gvr, ns, selector) calls
// carrying that exact selector, so a test can distinguish a target's
// primary selector query from its fallback selector query against the same
// GVR+namespace.
func (f *fakeClient) withListSelector(gvr schema.GroupVersionResource, ns, selector string, list *unstructured.UnstructuredList) *fakeClient {
	f.listsBySelector[selKey{gvr, ns, selector}] = list
	return f
}

func (f *fakeClient) withListErr(gvr schema.GroupVersionResource, ns string, err error) *fakeClient {
	f.listErrs[objKey{gvr, ns, ""}] = err
	return f
}

// withDeniedKind makes every Get and List of gvr fail with err.
func (f *fakeClient) withDeniedKind(gvr schema.GroupVersionResource, err error) *fakeClient {
	f.denied[gvr] = err
	return f
}

func (f *fakeClient) withLog(container, out string) *fakeClient {
	f.logs[container] = out
	return f
}

func (f *fakeClient) withPreviousLogErr(container string, err error) *fakeClient {
	f.prevLogErrs[container] = err
	return f
}

func (f *fakeClient) withLogErr(container string, err error) *fakeClient {
	f.logErrs[container] = err
	return f
}

func (f *fakeClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	f.kindReads = append(f.kindReads, gvr)
	if err, ok := f.denied[gvr]; ok {
		return nil, err
	}
	k := objKey{gvr, ns, name}
	if err, ok := f.getErrs[k]; ok {
		return nil, err
	}
	if obj, ok := f.objects[k]; ok {
		return obj, nil
	}
	return nil, fmt.Errorf("fakeClient: no object for %+v", k)
}

func (f *fakeClient) List(_ context.Context, gvr schema.GroupVersionResource, ns string, selector string) (*unstructured.UnstructuredList, error) {
	f.kindReads = append(f.kindReads, gvr)
	if err, ok := f.denied[gvr]; ok {
		return nil, err
	}
	f.listSelectors = append(f.listSelectors, selector)
	f.listNamespaces = append(f.listNamespaces, ns)
	k := objKey{gvr, ns, ""}
	if err, ok := f.listErrs[k]; ok {
		return nil, err
	}
	if l, ok := f.listsBySelector[selKey{gvr, ns, selector}]; ok {
		return l, nil
	}
	if l, ok := f.listsByNS[k]; ok {
		return l, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (f *fakeClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (f *fakeClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (f *fakeClient) Logs(_ context.Context, _ string, _ string, opts engine.LogOptions) (string, error) {
	f.logOptsCalls = append(f.logOptsCalls, opts)
	if opts.Previous {
		if err, ok := f.prevLogErrs[opts.Container]; ok {
			return "", err
		}
	}
	if err, ok := f.logErrs[opts.Container]; ok {
		return "", err
	}
	return f.logs[opts.Container], nil
}

var _ engine.ClusterClient = (*fakeClient)(nil)

// fakeFactory hands back a fixed local client and, for downstream ids, a
// per-cluster client from perCluster (falling back to local).
type fakeFactory struct {
	local         engine.ClusterClient
	perCluster    map[string]engine.ClusterClient
	perClusterErr map[string]error
	ctxCluster    string
	ctxClusterErr error
	resolve       map[string]string
	resolveErr    error
	root          *fakeRoot // the steve root; nil answers 404 to every path

	clusterCalls []string
}

// Root hands out the steve root fake, so the factory also serves as the
// engine.RootClientFactory of the commands under test.
func (f *fakeFactory) Root() (engine.RootClient, error) {
	if f.root == nil {
		return &fakeRoot{}, nil
	}
	return f.root, nil
}

// getter is the steve getter the resolve functions take, over Root.
func (f *fakeFactory) getter() steve.Getter { return steve.RootFactoryGetter(f) }

func (f *fakeFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.clusterCalls = append(f.clusterCalls, id)
	if err, ok := f.perClusterErr[id]; ok {
		return nil, err
	}
	if f.perCluster != nil {
		if c, ok := f.perCluster[id]; ok {
			return c, nil
		}
	}
	return f.local, nil
}

func (f *fakeFactory) ContextCluster() (string, error) { return f.ctxCluster, f.ctxClusterErr }
func (f *fakeFactory) ContextProject() (string, error) { return "", nil }

func (f *fakeFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if id, ok := f.resolve[nameOrID]; ok {
		return id, nil
	}
	return nameOrID, nil
}

var (
	_ engine.ClusterClientFactory = (*fakeFactory)(nil)
	_ engine.RootClientFactory    = (*fakeFactory)(nil)
)

// fakeRoot is an engine.RootClient serving management Clusters the way
// steve does: the collection at /v1/management.cattle.io.clusters and one
// object per id. A path in errs answers its error; any other unknown path
// answers the real root client's 404.
type fakeRoot struct {
	mu       sync.Mutex
	clusters map[string]*unstructured.Unstructured
	errs     map[string]error
	paths    []string
}

// withCluster serves obj, a management Cluster named by metadata.name.
func (r *fakeRoot) withCluster(obj *unstructured.Unstructured) *fakeRoot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clusters == nil {
		r.clusters = map[string]*unstructured.Unstructured{}
	}
	obj = obj.DeepCopy()
	obj.Object["id"] = obj.GetName()
	obj.Object["type"] = steve.ClustersType
	r.clusters[obj.GetName()] = obj
	return r
}

// withError makes a GET of path fail with err.
func (r *fakeRoot) withError(path string, err error) *fakeRoot {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errs == nil {
		r.errs = map[string]error{}
	}
	r.errs[path] = err
	return r
}

// rootStatusError is the error the real root client returns for a non-2xx
// answer of status with no body (clierr.FromResponse with the same prefix and
// request).
func rootStatusError(status int, path string) error {
	return clierr.FromResponse("rancher root GET "+path, http.MethodGet, status, nil, &clierr.Request{})
}

func (r *fakeRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
	if err, ok := r.errs[path]; ok {
		return nil, err
	}
	if path == (steve.Request{Type: steve.ClustersType}).Path() {
		ids := make([]string, 0, len(r.clusters))
		for id := range r.clusters {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		data := make([]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, r.clusters[id].Object)
		}
		page := map[string]any{"data": data, "revision": "1"}
		if len(data) > 0 {
			page["count"] = len(data)
		}
		return json.Marshal(page)
	}
	for id, obj := range r.clusters {
		if path == (steve.Request{Type: steve.ClustersType, Name: id}).Path() {
			return json.Marshal(obj.Object)
		}
	}
	return nil, rootStatusError(404, path)
}

// clusterPath is the steve path of one management Cluster.
func clusterPath(id string) string {
	return steve.Request{Type: steve.ClustersType, Name: id}.Path()
}

var _ engine.RootClient = (*fakeRoot)(nil)

func unstructuredPod(name string) unstructured.Unstructured {
	return unstructuredPodWithContainers(name, "main")
}

func unstructuredPodWithContainers(name string, containers ...string) unstructured.Unstructured {
	var cs []any
	for _, c := range containers {
		cs = append(cs, map[string]any{"name": c})
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"containers": cs,
		},
	}}
}

func podList(names ...string) *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	for _, n := range names {
		list.Items = append(list.Items, unstructuredPod(n))
	}
	return list
}

func (*fakeClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*fakeClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f *fakeFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}
