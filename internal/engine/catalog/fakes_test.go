package catalog

import (
	"context"
	"net/http"
	"sync"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// input is the runtime.Input the tests hand to a command.
type input struct {
	s map[string]string
	b map[string]bool
}

func (i input) String(k string) string  { return i.s[k] }
func (i input) Bool(k string) bool      { return i.b[k] }
func (i input) Strings(string) []string { return nil }
func (i input) IsSet(string) bool       { return false }
func (i input) Args() []string          { return nil }

// rootAnswer is one path's answer: a status and a body.
type rootAnswer struct {
	status int
	body   string
}

// fakeRoot answers engine.RootClient from a table and records every path.
// A non-2xx answer carries the error the production root client builds from
// it. A path with no answer is a 404.
type fakeRoot struct {
	mu      sync.Mutex
	answers map[string]rootAnswer
	paths   []string
}

func newFakeRoot(answers map[string]rootAnswer) *fakeRoot { return &fakeRoot{answers: answers} }

func (r *fakeRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	a, ok := r.answers[path]
	r.mu.Unlock()
	if !ok {
		a = rootAnswer{status: http.StatusNotFound}
	}
	if a.status >= 200 && a.status < 300 {
		return []byte(a.body), nil
	}
	return nil, clierr.FromResponse("GET "+path+" on the Rancher server", http.MethodGet, a.status, []byte(a.body), nil)
}

func (r *fakeRoot) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

type fakeRoots struct {
	root engine.RootClient
	err  error
}

func (f fakeRoots) Root() (engine.RootClient, error) { return f.root, f.err }

// fakeCluster lists ClusterRepos and records the kind and namespace asked.
type fakeCluster struct {
	engine.ClusterClient
	items []*unstructured.Unstructured
	err   error
	gvrs  []schema.GroupVersionResource
	nss   []string
}

func (c *fakeCluster) List(_ context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	c.gvrs = append(c.gvrs, gvr)
	c.nss = append(c.nss, ns)
	if c.err != nil {
		return nil, c.err
	}
	list := &unstructured.UnstructuredList{}
	for _, it := range c.items {
		list.Items = append(list.Items, *it)
	}
	return list, nil
}

// fakeClients resolves the typed cluster through a table and hands out one
// fakeCluster, recording the id it was asked for.
type fakeClients struct {
	engine.ClusterClientFactory
	resolve map[string]string
	cluster *fakeCluster
	asked   []string
}

func (f *fakeClients) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if id, ok := f.resolve[nameOrID]; ok {
		return id, nil
	}
	return nameOrID, nil
}

func (f *fakeClients) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.asked = append(f.asked, id)
	return f.cluster, nil
}

// repoObject builds a ClusterRepo; enabled nil leaves spec.enabled out.
func repoObject(name string, enabled *bool) *unstructured.Unstructured {
	spec := map[string]any{"url": "https://charts.example.test"}
	if enabled != nil {
		spec["enabled"] = *enabled
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.cattle.io/v1",
		"kind":       "ClusterRepo",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}
