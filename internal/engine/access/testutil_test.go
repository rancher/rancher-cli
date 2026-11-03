package access

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// scripted is one PostReview answer: either a response or a transport
// error.
type scripted struct {
	resp engine.ReviewResponse
	err  error
}

// fakeClient records every review it receives and answers from a script
// in order; when the script is exhausted it repeats the last entry.
// Fan-out tests set fanOutLimit to 1 so the sorted key order is the call
// order.
type fakeClient struct {
	mu        sync.Mutex
	script    []scripted
	Before    func(ctx context.Context) error // optional; runs first in every call (tests block here to spend a budget)
	Reviews   []engine.ReviewRequest
	Resources []engine.APIResource
	// ResourcesErr is returned with Resources: an *engine.DiscoveryIncomplete
	// models a partial answer, anything else a failed discovery.
	ResourcesErr error
	Objects      map[string]*unstructured.Unstructured // "ns/name" or "name" for cluster scope
	Lists        map[string]*unstructured.UnstructuredList
	GetErr       error
	ListErr      error
	// GetErrFor fails the GET of one key ("ns/name" or "name") while every
	// other GET answers; GetErr fails them all.
	GetErrFor map[string]error
	// ListErrFor fails the LIST of one key ("resource|ns|selector") while
	// every other LIST answers; ListErr fails them all.
	ListErrFor map[string]error
	// AttrRefusal sets how steve answers UserAttribute reads.
	AttrRefusal userAttributeRefusal
	// AttrPaths records the steve paths read, AttrReviews the list checks.
	AttrPaths   []string
	AttrReviews int
}

func (f *fakeClient) PostReview(ctx context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	if isListUserAttributesReview(req) {
		// The list check of the UserAttribute reader: answered by what the
		// fake serves, outside the script that orders the other reviews.
		f.mu.Lock()
		f.AttrReviews++
		f.mu.Unlock()
		return review(engine.ReviewSelfSubjectAccess, map[string]any{"allowed": f.AttrRefusal == attrServed}), nil
	}
	if f.Before != nil {
		if err := f.Before(ctx); err != nil {
			return engine.ReviewResponse{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reviews = append(f.Reviews, req)
	if len(f.script) == 0 {
		return engine.ReviewResponse{}, errors.New("no scripted review answer")
	}
	i := len(f.Reviews) - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	s := f.script[i]
	if s.err != nil {
		return engine.ReviewResponse{}, s.err
	}
	return s.resp, nil
}

func (f *fakeClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return f.Resources, f.ResourcesErr
}

func (f *fakeClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if f.Before != nil {
		if err := f.Before(ctx); err != nil {
			return nil, err
		}
	}
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	key := name
	if ns != "" {
		key = ns + "/" + name
	}
	// A UserAttribute is named by the user id, so it needs its own key
	// space to sit beside the User of the same name.
	if gvr.Resource == "userattributes" {
		key = name + "-attr"
	}
	if err, ok := f.GetErrFor[key]; ok {
		return nil, err
	}
	if o, ok := f.Objects[key]; ok {
		return o, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, key+" not found")
}

func (f *fakeClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	if f.Before != nil {
		if err := f.Before(ctx); err != nil {
			return nil, err
		}
	}
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	if err, ok := f.ListErrFor[gvr.Resource+"|"+ns+"|"+selector]; ok {
		return nil, err
	}
	if l, ok := f.Lists[gvr.Resource+"|"+ns+"|"+selector]; ok {
		return l, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (f *fakeClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("create is not used by access")
}

func (f *fakeClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("update is not used by access")
}

func (f *fakeClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return errors.New("delete is not used by access")
}

func (f *fakeClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("logs is not used by access")
}

// fakeFactory hands out one client for the caller and one per impersonated
// user, recording who was impersonated.
type fakeFactory struct {
	mu           sync.Mutex
	caller       *fakeClient
	impersonated map[string]*fakeClient
	Impersonated []string
	resolve      map[string]string
	ResolveCalls int
	ClusterErr   error
	// clusters hands out a client per cluster id for commands that fan out
	// over clusters; a cluster not listed here gets caller.
	clusters map[string]*fakeClient
	// impersonatedPer hands out a client per (cluster, user), keyed
	// cluster + "\x00" + user; a pair not listed here falls back to
	// impersonated[user], then caller.
	impersonatedPer map[string]*fakeClient
	// clusterClients and impersonatedClients are the interface-typed
	// twins of clusters and impersonatedPer, for clients that wrap a fake
	// (a blocking one); they are consulted first.
	clusterClients      map[string]engine.ClusterClient
	impersonatedClients map[string]engine.ClusterClient
}

func (f *fakeFactory) Cluster(_ context.Context, clusterID string) (engine.ClusterClient, error) {
	if f.ClusterErr != nil {
		return nil, f.ClusterErr
	}
	if c, ok := f.clusterClients[clusterID]; ok {
		return c, nil
	}
	if c, ok := f.clusters[clusterID]; ok {
		return c, nil
	}
	return f.caller, nil
}

func (f *fakeFactory) ImpersonatedCluster(_ context.Context, clusterID string, as engine.Impersonation) (engine.ClusterClient, error) {
	f.mu.Lock()
	f.Impersonated = append(f.Impersonated, as.User)
	f.mu.Unlock()
	if c, ok := f.impersonatedClients[clusterID+"\x00"+as.User]; ok {
		return c, nil
	}
	if c, ok := f.impersonatedPer[clusterID+"\x00"+as.User]; ok {
		return c, nil
	}
	if c, ok := f.impersonated[as.User]; ok {
		return c, nil
	}
	return f.caller, nil
}

func (f *fakeFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	f.ResolveCalls++
	if id, ok := f.resolve[s]; ok {
		return id, nil
	}
	return s, nil
}

// review builds a decoded 2xx review response of the given kind with the
// given status map.
func review(kind engine.ReviewKind, status map[string]any) engine.ReviewResponse {
	gvk, _ := engine.ReviewGVK(kind)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.Group + "/" + gvk.Version, "kind": gvk.Kind, "status": status,
	}}
	return engine.ReviewResponse{Status: 201, Object: obj}
}

func whoamiResp(user string, groups ...string) engine.ReviewResponse {
	gs := make([]any, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, g)
	}
	return review(engine.ReviewSelfSubjectIdentity, map[string]any{"userInfo": map[string]any{"username": user, "groups": gs}})
}

func sar(allowed bool, reason string) engine.ReviewResponse {
	return review(engine.ReviewSelfSubjectAccess, map[string]any{"allowed": allowed, "reason": reason})
}

func http403(body string) engine.ReviewResponse {
	return engine.ReviewResponse{Status: 403, Body: []byte(body), Mapped: clierr.New(clierr.CodeRBAC, body)}
}

// saNotFound500 is the proxy's plain-text 500 for an impersonated service
// account whose token cannot be created; the provider marks it retryable
// like every plain-text 500, and the engine overrides that by shape.
func saNotFound500(tail string) engine.ReviewResponse {
	body := "unable to create token for impersonated ServiceAccount: can't create token: " + tail
	return engine.ReviewResponse{Status: 500, Body: []byte(body), Mapped: clierr.Retryable(clierr.CodeGeneric, body)}
}

// clientProject is the project a typed --project resolved to on cluster
// c-phmj7. Its namespaces are the namespace LIST through c filtered by the
// project label, the request the resolver sends first, so the fake's
// lists, errors and Before hook decide them.
func clientProject(c *fakeClient, id, display string) runtime.Project {
	return runtime.Project{Cluster: "c-phmj7", ID: id, DisplayName: display,
		Namespaces: func(ctx context.Context) (map[string]bool, error) {
			nsKind, _ := kinds.Lookup("namespace")
			list, err := c.List(ctx, nsKind.GVR, "", "field.cattle.io/projectId="+id)
			if err != nil {
				return nil, err
			}
			members := make(map[string]bool, len(list.Items))
			for i := range list.Items {
				members[list.Items[i].GetName()] = true
			}
			return members, nil
		}}
}

// projectContext is t.Context() carrying clientProject(c, id, display) as
// the project the pipeline resolved.
func projectContext(t *testing.T, c *fakeClient, id, display string) context.Context {
	t.Helper()
	return runtime.WithProject(t.Context(), clientProject(c, id, display))
}

// unreadProjectContext is t.Context() carrying a project lookup that fails
// the test when the command reads it.
func unreadProjectContext(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
		t.Error("the project was read")
		return runtime.Project{}, errors.New("the project was read")
	})
}

// blockingProjectContext is t.Context() carrying a project lookup that
// blocks until the context it is called with ends, standing in for a slow
// lookup so a test can spend a budget in it. A context that does not end
// within two seconds fails the test: the lookup ran outside any budget.
func blockingProjectContext(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProjectLookup(t.Context(), func(ctx context.Context) (runtime.Project, error) {
		select {
		case <-ctx.Done():
			return runtime.Project{}, ctx.Err()
		case <-time.After(2 * time.Second):
			t.Error("no budget bounded the project lookup")
			return runtime.Project{}, errors.New("no budget bounded the project lookup")
		}
	})
}
