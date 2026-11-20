package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// nodeResultReason is the reason of the node drain rows: the result is built
// from names, never from a server object.
const nodeResultReason = "the result is built from names: the node, its management node and the four fields of the Drained condition; no server object"

// nodeServer is a Rancher test server for node drain and node stop-drain: it
// responds to the management node list and read, the Steve API's view of it,
// the pods of the node and the write, planting the row's values in every
// response for the management node.
type nodeServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	// before is the management node the Plan reads; after is what the reads
	// after the write return.
	before, after map[string]any
	state         string
	plant         func(obj map[string]any)
	// puts records every write of the management node: true for a dry run.
	puts []bool
	// norman, when set to a node action, makes the server answer as it does
	// to a caller without access to the local cluster: the lists through the
	// local cluster are a 403, the Steve API serves the management nodes of
	// the cluster as a collection and the single node as it is after the
	// write, and the Norman node offers that action.
	norman string
	// posts records every Norman action request: its path and query.
	posts []string
}

func nodeObject(spec map[string]any, conditions ...any) map[string]any {
	status := map[string]any{"nodeName": "worker-1"}
	if len(conditions) > 0 {
		status["conditions"] = conditions
	}
	return map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Node",
		"metadata": map[string]any{"name": "m-1", "namespace": scrubCluster, "resourceVersion": "7", "labels": map[string]any{"management.cattle.io/nodename": "worker-1"}},
		"spec":     spec, "status": status,
	}
}

func (s *nodeServer) answer(obj map[string]any) map[string]any {
	copied := (&unstructured.Unstructured{Object: obj}).DeepCopy().Object
	s.plant(copied)
	return copied
}

func (s *nodeServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	list := func(items ...map[string]any) map[string]any {
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = s.answer(item)
		}
		return map[string]any{"apiVersion": "management.cattle.io/v3", "kind": "NodeList", "metadata": map[string]any{"resourceVersion": "7"}, "items": out}
	}
	path := r.URL.Path
	steveItem := func(source map[string]any) map[string]any {
		obj := s.answer(source)
		meta := obj["metadata"].(map[string]any)
		meta["state"] = map[string]any{"name": s.state}
		obj["id"] = scrubCluster + "/m-1"
		return obj
	}
	switch {
	case path == "/v1/management.cattle.io.nodes/"+scrubCluster:
		reply(200, map[string]any{"data": []any{steveItem(s.before)}, "count": 1, "revision": "1"})
	case strings.HasPrefix(path, "/v1/management.cattle.io.nodes/"):
		if s.norman != "" {
			reply(200, steveItem(s.after))
			return
		}
		reply(200, steveItem(s.before))
	case strings.HasPrefix(path, "/v3/nodes/"):
		if r.Method == http.MethodPost {
			s.posts = append(s.posts, path+"?"+r.URL.RawQuery)
			reply(200, map[string]any{"type": "node"})
			return
		}
		reply(200, map[string]any{"id": scrubCluster + ":m-1", "type": "node", "state": s.state,
			"actions": map[string]any{s.norman: "https://rancher.example/v3/nodes/" + scrubCluster + ":m-1?action=" + s.norman}})
	case strings.HasSuffix(path, "/selfsubjectaccessreviews") && r.Method == http.MethodPost:
		reply(201, map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]any{"allowed": true}})
	case s.norman != "" && strings.Contains(path, "/namespaces/"+scrubCluster+"/nodes"):
		reply(403, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "message": "nodes.management.cattle.io is forbidden", "reason": "Forbidden", "code": 403})
	case strings.HasSuffix(path, "/namespaces/"+scrubCluster+"/nodes") && r.Method == http.MethodGet:
		if strings.Contains(r.URL.Query().Get("fieldSelector"), "metadata.name") {
			reply(200, list(s.after))
			return
		}
		reply(200, list(s.before))
	case strings.Contains(path, "/namespaces/"+scrubCluster+"/nodes/m-1") && r.Method == http.MethodPut:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.puts = append(s.puts, r.URL.Query().Get("dryRun") == "All")
		s.plant(body)
		reply(200, body)
	case strings.Contains(path, "/namespaces/"+scrubCluster+"/nodes/m-1") && r.Method == http.MethodGet:
		reply(200, s.answer(s.after))
	case strings.HasSuffix(path, "/api/v1/pods"):
		reply(200, map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
	default:
		reply(404, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "message": "no route", "reason": "NotFound", "code": 404})
	}
}

type nodeFactory struct{ srv *httptest.Server }

func (f nodeFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	return provider.NewClusterClient(&rest.Config{Host: f.srv.URL + "/k8s/clusters/" + id, BearerToken: "t", Transport: f.srv.Client().Transport}, id)
}

func (nodeFactory) ImpersonatedCluster(context.Context, string, engine.Impersonation) (engine.ClusterClient, error) {
	return nil, clierr.New(clierr.CodeGeneric, "not used")
}

func (nodeFactory) ResolveCluster(_ context.Context, nameOrID string) (string, error) {
	return nameOrID, nil
}

type nodeRoots struct{ srv *httptest.Server }

func (r nodeRoots) Root() (engine.RootClient, error) { return nodeRoot(r), nil }

type nodeRoot struct{ srv *httptest.Server }

func (r nodeRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// newNodeServer starts a node server that responds with before to the Plan and
// with after to the reads that follow the write; plant changes every response
// for the management node.
func newNodeServer(t *testing.T, state string, before, after map[string]any, plant func(obj map[string]any)) *nodeServer {
	t.Helper()
	s := &nodeServer{before: before, after: after, state: state, plant: plant}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *nodeServer) deps() runDeps {
	return runDeps{clients: nodeFactory{s.srv}, roots: nodeRoots{s.srv}, nodeActions: nodeActionFactory{s.srv}}
}

// nodeActionFactory is the node action client over the node server: the
// client the command line builds, with the server's own http client.
type nodeActionFactory struct{ srv *httptest.Server }

func (f nodeActionFactory) NodeActions() (engine.NodeActionClient, error) {
	return &nodeActionClient{base: f.srv.URL, token: "t", hc: f.srv.Client()}, nil
}

func (s *nodeServer) actions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts...)
}

func (s *nodeServer) writes() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.puts...)
}

// nodeScrubDeps returns the deps of a node command row: a test server that
// responds with before to the Plan and with after to the reads that follow the
// write, and the factories over it. The server plants the row's values in
// every response for the management node, and the fake counts the responses
// that carry a value the row must not show.
func nodeScrubDeps(state string, before, after map[string]any) func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
	return nodeScrubDepsVia("", state, before, after)
}

// nodeScrubDepsVia is nodeScrubDeps for a server that answers as it does to a
// caller without access to the local cluster when norman names a node action
// ("drain" or "stopDrain"), the action the Norman node offers.
func nodeScrubDepsVia(norman, state string, before, after map[string]any) func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
	return func(t *testing.T, f *scrubFake, ps *plantSet) runDeps {
		t.Helper()
		require.NotNil(t, f.kind)
		s := newNodeServer(t, state, before, after, func(obj map[string]any) {
			mergeMissing(obj, ps.fragment)
			f.scan(&unstructured.Unstructured{Object: obj})
		})
		s.norman = norman
		return s.deps()
	}
}

// nodeScrubRows are the result-scrub rows of node drain and node stop-drain:
// each command without the wait, and node drain with the wait, whose server
// responds with the Drained condition at the first read.
func nodeScrubRows() []scrubRow {
	worker := map[string]any{"worker": true, "internalNodeSpec": map[string]any{}}
	draining := map[string]any{"worker": true, "desiredNodeUnschedulable": "drain", "internalNodeSpec": map[string]any{"unschedulable": true}}
	drainedAt := map[string]any{"type": "Drained", "status": "True", "reason": "Drained", "message": "done", "lastUpdateTime": "t1"}
	noWait := func(*plantSet) fieldCoverageInput {
		return fieldCoverageInput{args: []string{"worker-1"}, values: map[string]string{"cluster": scrubCluster}, bools: map[string]bool{"wait": false}}
	}
	waited := func(*plantSet) fieldCoverageInput {
		return fieldCoverageInput{args: []string{"worker-1"}, values: map[string]string{"cluster": scrubCluster}}
	}
	return []scrubRow{
		{command: "node drain", variant: "no wait", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: noWait, deps: nodeScrubDeps("active", nodeObject(worker), nodeObject(worker))},
		{command: "node drain", variant: "waited", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: waited, deps: nodeScrubDeps("active", nodeObject(worker), nodeObject(worker, drainedAt))},
		{command: "node stop-drain", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: noWait, deps: nodeScrubDeps("draining", nodeObject(draining, map[string]any{"type": "Drained", "status": "Unknown", "lastUpdateTime": "t0"}), nodeObject(draining))},
		{command: "node drain", variant: "no wait, through Rancher's node action", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: noWait, deps: nodeScrubDepsVia("drain", "active", nodeObject(worker), nodeObject(worker))},
		{command: "node drain", variant: "waited, through Rancher's node action", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: waited, deps: nodeScrubDepsVia("drain", "active", nodeObject(worker), nodeObject(worker, drainedAt))},
		{command: "node stop-drain", variant: "through Rancher's node action", kind: "managementnode", class: classNames, reason: nodeResultReason,
			input: noWait, deps: nodeScrubDepsVia("stopDrain", "draining", nodeObject(draining, map[string]any{"type": "Drained", "status": "Unknown", "lastUpdateTime": "t0"}), nodeObject(draining))},
	}
}
