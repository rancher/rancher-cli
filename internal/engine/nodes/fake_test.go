package nodes

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// input is a runtime.Input of typed flags and arguments.
type input struct {
	args  []string
	flags map[string]string
}

func (i input) String(name string) string { return i.flags[name] }
func (i input) Bool(name string) bool     { return i.flags[name] == "true" }
func (i input) Strings(name string) []string {
	if v, ok := i.flags[name]; ok {
		return []string{v}
	}
	return nil
}
func (i input) IsSet(name string) bool { _, ok := i.flags[name]; return ok }
func (i input) Args() []string         { return i.args }

var _ runtime.Input = input{}

// newInput types the node and the flags of a command line; a flag is
// "--name=value" or "--name" for a bool.
func newInput(node string, flags ...string) input {
	in := input{flags: map[string]string{"cluster": "prod"}}
	if node != "" {
		in.args = []string{node}
	}
	for _, f := range flags {
		name, value, found := strings.Cut(strings.TrimPrefix(f, "--"), "=")
		if !found {
			value = "true"
		}
		in.flags[name] = value
	}
	return in
}

// putRecord is a write the server received.
type putRecord struct {
	dryRun bool
	query  string
	body   map[string]any
}

// fakeRancher serves what node drain reads and writes: the management nodes
// of one cluster on the local cluster, the Steve API's view of one of them, the pods
// of a node, the write of a management node and the reads that follow it.
type fakeRancher struct {
	srv *httptest.Server

	mu sync.Mutex
	// nodes is the management node List response for the label selector (when
	// the label is sent) and for the plain list.
	labeled, plain []map[string]any
	// steve is the Steve API's response for the management node, steveStatus its
	// status when not 200.
	steve       map[string]any
	steveStatus int
	pods        []map[string]any
	podStatus   int
	listStatus  int
	putStatus   int
	putMessage  string
	// reads are the responses of the management node GET after the write, in
	// order; the last one repeats.
	reads []map[string]any
	// readStatus, when set, is the status of every management node GET.
	readStatus int
	// requests are the method and path with query of every request.
	requests []string
	puts     []putRecord
}

func newFakeRancher(t *testing.T) *fakeRancher {
	t.Helper()
	f := &fakeRancher{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func statusBody(code int, message string) map[string]any {
	reasons := map[int]string{403: "Forbidden", 404: "NotFound", 409: "Conflict", 503: "ServiceUnavailable"}
	return map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "message": message, "reason": reasons[code], "code": code}
}

func (f *fakeRancher) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/v1/management.cattle.io.nodes/"):
		if f.steveStatus != 0 {
			writeJSON(w, f.steveStatus, statusBody(f.steveStatus, "the Steve API refused the read"))
			return
		}
		if f.steve == nil {
			_, _ = io.WriteString(w, "not json")
			return
		}
		writeJSON(w, 200, f.steve)
	case strings.HasSuffix(path, "/apis/management.cattle.io/v3/namespaces/c-1/nodes") && r.Method == http.MethodGet:
		if f.listStatus != 0 {
			writeJSON(w, f.listStatus, statusBody(f.listStatus, "nodes.management.cattle.io is forbidden: User \"u\" cannot list resource \"nodes\""))
			return
		}
		items := f.plain
		if r.URL.Query().Get("labelSelector") != "" {
			items = f.labeled
		}
		list := make([]any, len(items))
		for i, item := range items {
			list[i] = item
		}
		writeJSON(w, 200, map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "NodeList", "metadata": map[string]any{"resourceVersion": "1"}, "items": list})
	case strings.Contains(path, "/apis/management.cattle.io/v3/namespaces/c-1/nodes/") && r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.puts = append(f.puts, putRecord{dryRun: r.URL.Query().Get("dryRun") == "All", query: r.URL.RawQuery, body: body})
		if f.putStatus != 0 {
			writeJSON(w, f.putStatus, statusBody(f.putStatus, f.putMessage))
			return
		}
		writeJSON(w, 200, body)
	case strings.Contains(path, "/apis/management.cattle.io/v3/namespaces/c-1/nodes/") && r.Method == http.MethodGet:
		if f.readStatus != 0 {
			writeJSON(w, f.readStatus, statusBody(f.readStatus, "the read is refused"))
			return
		}
		if len(f.reads) == 0 {
			writeJSON(w, 404, statusBody(404, "not found"))
			return
		}
		answer := f.reads[0]
		if len(f.reads) > 1 {
			f.reads = f.reads[1:]
		}
		writeJSON(w, 200, answer)
	case strings.HasSuffix(path, "/api/v1/pods"):
		if f.podStatus != 0 {
			writeJSON(w, f.podStatus, statusBody(f.podStatus, "pods is forbidden"))
			return
		}
		items := make([]any, len(f.pods))
		for i, p := range f.pods {
			items[i] = p
		}
		writeJSON(w, 200, map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "1"}, "items": items})
	default:
		writeJSON(w, 404, statusBody(404, "no route "+path))
	}
}

func (f *fakeRancher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeRancher) writes() []putRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]putRecord(nil), f.puts...)
}

// onlyLister hides every method of a client but the ClusterClient's and the
// field selector list, so the client offers no watch and the wait polls.
type onlyLister struct {
	engine.ClusterClient
	engine.FieldSelectorLister
}

// withoutLister hides the field selector list.
type withoutLister struct{ engine.ClusterClient }

type testFactory struct {
	f        *fakeRancher
	noLister bool
}

func (tf testFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	c, err := provider.NewClusterClient(&rest.Config{Host: tf.f.srv.URL + "/k8s/clusters/" + id, BearerToken: "t", Transport: tf.f.srv.Client().Transport}, id)
	if err != nil {
		return nil, err
	}
	if tf.noLister {
		return withoutLister{c}, nil
	}
	return onlyLister{c, c.(engine.FieldSelectorLister)}, nil
}

func (tf testFactory) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return nil, clierr.New(clierr.CodeGeneric, "not used")
}

func (tf testFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	if nameOrID == "prod" {
		return "c-1", nil
	}
	return nameOrID, nil
}

type testRoots struct{ f *fakeRancher }

func (r testRoots) Root() (engine.RootClient, error) { return rootClient(r), nil }

type rootClient struct{ f *fakeRancher }

func (r rootClient) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.f.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.f.srv.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, clierr.FromResponse("", http.MethodGet, resp.StatusCode, body, nil)
	}
	return body, nil
}

// managementNode builds a management node as the Kubernetes API serves it.
func managementNode(name, node, resourceVersion string, spec map[string]any, conditions ...map[string]any) map[string]any {
	status := map[string]any{"nodeName": node}
	if len(conditions) > 0 {
		list := make([]any, len(conditions))
		for i, c := range conditions {
			list[i] = c
		}
		status["conditions"] = list
	}
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Node",
		"metadata": map[string]any{"name": name, "namespace": "c-1", "resourceVersion": resourceVersion, "labels": map[string]any{"management.cattle.io/nodename": node}},
		"spec":     spec, "status": status,
	}
}

func drainedCond(status, message, updated string) map[string]any {
	return map[string]any{"type": "Drained", "status": status, "message": message, "lastUpdateTime": updated}
}

// steveView is the Steve API's response for a management node: the object with its
// state.
func steveView(obj map[string]any, state, message string) map[string]any {
	out := map[string]any{}
	maps.Copy(out, obj)
	meta := map[string]any{}
	maps.Copy(meta, obj["metadata"].(map[string]any))
	meta["state"] = map[string]any{"name": state, "message": message}
	out["metadata"] = meta
	return out
}

// workerSpec is the spec of a worker node that is schedulable.
func workerSpec(extra ...any) map[string]any {
	spec := map[string]any{"worker": true, "internalNodeSpec": map[string]any{}}
	for i := 0; i+1 < len(extra); i += 2 {
		spec[extra[i].(string)] = extra[i+1]
	}
	return spec
}

// idleNode loads the server with an active worker node, worker-1, and no pods.
func idleNode(t *testing.T) *fakeRancher {
	t.Helper()
	f := newFakeRancher(t)
	obj := managementNode("m-1", "worker-1", "7", workerSpec())
	f.labeled = []map[string]any{obj}
	f.steve = steveView(obj, "active", "")
	f.reads = []map[string]any{obj}
	return f
}

// plan runs the Plan of cmd against the server.
func plan(t *testing.T, cmd runtime.Command, in input) (eff planned, err error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok)
	e, exec, err := mc.Plan(t.Context(), in)
	return planned{eff: e, exec: exec}, err
}

type planned struct {
	eff  safety.Effect
	exec runtime.ExecuteFunc
}

// fastWait shortens the poll interval for the test and restores it. A test
// that calls it must not run in parallel.
func fastWait(t *testing.T) {
	t.Helper()
	old := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
}

func unstr(m map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: m} }
