package inspect

import (
	"context"
	"fmt"
	"sync"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// input is the runtime.Input fake shared across the inspect package's tests.
type input struct {
	args []string
	s    map[string]string
	b    map[string]bool
}

func (i input) String(k string) string { return i.s[k] }
func (i input) Bool(k string) bool     { return i.b[k] }
func (i input) Args() []string         { return i.args }

type objKey struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

// fakeClient is a table-driven engine.ClusterClient fake: objects keyed by
// GVR+ns+name serve Get; lists keyed by GVR serve List (the selector is
// recorded, not filtered — tests assert on the recorded selector directly).
// mu guards the recorded-call slices: the --all-clusters fan-out drives a
// distinct fakeClient per goroutine, but the shared management-cluster
// fakeClient and fakeFactory are still hit concurrently under -race.
type fakeClient struct {
	mu sync.Mutex

	objects  map[objKey]*unstructured.Unstructured
	getErrs  map[objKey]error
	getCalls []objKey

	lists         map[schema.GroupVersionResource]*unstructured.UnstructuredList
	listErrs      map[schema.GroupVersionResource]error
	listSelectors []string

	logs    map[string]string
	logErrs map[string]error
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		objects:  map[objKey]*unstructured.Unstructured{},
		getErrs:  map[objKey]error{},
		lists:    map[schema.GroupVersionResource]*unstructured.UnstructuredList{},
		listErrs: map[schema.GroupVersionResource]error{},
		logs:     map[string]string{},
		logErrs:  map[string]error{},
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

func (f *fakeClient) withList(gvr schema.GroupVersionResource, list *unstructured.UnstructuredList) *fakeClient {
	f.lists[gvr] = list
	return f
}

func (f *fakeClient) withListErr(gvr schema.GroupVersionResource, err error) *fakeClient {
	f.listErrs[gvr] = err
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
	f.mu.Lock()
	f.getCalls = append(f.getCalls, k)
	f.mu.Unlock()
	if err, ok := f.getErrs[k]; ok {
		return nil, err
	}
	if obj, ok := f.objects[k]; ok {
		return obj, nil
	}
	return nil, fmt.Errorf("fakeClient: no object for %+v", k)
}

func (f *fakeClient) List(_ context.Context, gvr schema.GroupVersionResource, _ string, selector string) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	f.listSelectors = append(f.listSelectors, selector)
	f.mu.Unlock()
	if err, ok := f.listErrs[gvr]; ok {
		return nil, err
	}
	if l, ok := f.lists[gvr]; ok {
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

func (f *fakeClient) Logs(_ context.Context, _ string, _ string, container string, _ int) (string, error) {
	if err, ok := f.logErrs[container]; ok {
		return "", err
	}
	return f.logs[container], nil
}

var _ engine.ClusterClient = (*fakeClient)(nil)

// fakeFactory hands back a fixed (or per-cluster) client for any cluster id.
// mu guards clusterCalls: the --all-clusters fan-out calls Cluster()
// concurrently from multiple goroutines.
type fakeFactory struct {
	mu sync.Mutex

	client        engine.ClusterClient
	perCluster    map[string]engine.ClusterClient
	perClusterErr map[string]error
	ctxCluster    string
	ctxClusterErr error
	resolve       map[string]string
	resolveErr    error

	clusterCalls []string
}

func (f *fakeFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.mu.Lock()
	f.clusterCalls = append(f.clusterCalls, id)
	f.mu.Unlock()
	if err, ok := f.perClusterErr[id]; ok {
		return nil, err
	}
	if f.perCluster != nil {
		if c, ok := f.perCluster[id]; ok {
			return c, nil
		}
	}
	return f.client, nil
}

func (f *fakeFactory) ContextCluster() (string, error) { return f.ctxCluster, f.ctxClusterErr }

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
