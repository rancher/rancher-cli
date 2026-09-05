package observe

// Deliberately plain-stdlib (no testify): shared test helpers without
// assertions, nothing to convert.

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine"
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

	logs         map[string]string
	logErrs      map[string]error
	logOptsCalls []engine.LogOptions
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		objects:         map[objKey]*unstructured.Unstructured{},
		getErrs:         map[objKey]error{},
		listsByNS:       map[objKey]*unstructured.UnstructuredList{},
		listsBySelector: map[selKey]*unstructured.UnstructuredList{},
		listErrs:        map[objKey]error{},
		logs:            map[string]string{},
		logErrs:         map[string]error{},
	}
}

func (f *fakeClient) withObject(gvr schema.GroupVersionResource, ns, name string, obj *unstructured.Unstructured) *fakeClient {
	f.objects[objKey{gvr, ns, name}] = obj
	return f
}

func (f *fakeClient) withGetErr(gvr schema.GroupVersionResource, ns, name string, err error) *fakeClient {
	f.getErrs[objKey{gvr, ns, name}] = err
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

func (f *fakeClient) withLog(container, out string) *fakeClient {
	f.logs[container] = out
	return f
}

func (f *fakeClient) withLogErr(container string, err error) *fakeClient {
	f.logErrs[container] = err
	return f
}

func (f *fakeClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
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

	clusterCalls []string
}

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

var _ engine.ClusterClientFactory = (*fakeFactory)(nil)

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
