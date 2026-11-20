package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// plainStatus is the status of the plain list only, when set.
	plainStatus int
	putStatus   int
	// applyPutStatus is the status of a write that is not a dry-run, when
	// set.
	applyPutStatus int
	putMessage     string
	// reads are the responses of the management node GET after the write, in
	// order; the last one repeats.
	reads []map[string]any
	// readStatus, when set, is the status of every management node GET.
	readStatus int
	// requests are the method and path with query of every request.
	requests []string
	puts     []putRecord

	// steveItems are the data of the Steve API's collection of the cluster's
	// management nodes, steveListStatus its status when not 200, steveListBody
	// the whole answer when set, and steveReads the answers of the single
	// object in order, the last one repeating, when set in place of steve.
	steveItems      []map[string]any
	steveListStatus int
	steveListBody   string
	steveReads      []map[string]any

	// norman are the answers of the Norman node GET in order, the last one
	// repeating; normanStatus is the status of every GET when set and
	// normanBody the whole answer when set. normanPostStatus is the status of
	// the action POST, normanPostDrop closes the connection after the request
	// is read, and posts are the action requests received.
	norman           []map[string]any
	normanStatus     int
	normanBody       string
	normanPostStatus int
	normanPostDrop   bool
	posts            []postRecord

	// reviewStatus is the status of the access review when set,
	// reviewDenied and reviewEvalError its answer, and reviews the bodies
	// received.
	reviewStatus    int
	reviewDenied    bool
	reviewEvalError string
	reviews         []map[string]any
}

// postRecord is a Norman action request the server received.
type postRecord struct {
	path        string
	query       string
	contentType string
	body        []byte
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
	case path == "/v1/management.cattle.io.nodes/c-1":
		f.serveSteveList(w)
	case strings.HasPrefix(path, "/v3/nodes/"):
		f.serveNorman(w, r)
	case strings.HasSuffix(path, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews") && r.Method == http.MethodPost:
		f.serveReview(w, r)
	case strings.HasPrefix(path, "/v1/management.cattle.io.nodes/"):
		if f.steveStatus != 0 {
			writeJSON(w, f.steveStatus, statusBody(f.steveStatus, "the Steve API refused the read"))
			return
		}
		if len(f.steveReads) > 0 {
			answer := f.steveReads[0]
			if len(f.steveReads) > 1 {
				f.steveReads = f.steveReads[1:]
			}
			writeJSON(w, 200, answer)
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
		} else if f.plainStatus != 0 {
			writeJSON(w, f.plainStatus, statusBody(f.plainStatus, "nodes.management.cattle.io is forbidden: User \"u\" cannot list resource \"nodes\""))
			return
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
		if f.applyPutStatus != 0 && r.URL.Query().Get("dryRun") != "All" {
			writeJSON(w, f.applyPutStatus, statusBody(f.applyPutStatus, f.putMessage))
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

// serveSteveList serves the Steve API's collection of the management nodes of
// the cluster as a page with data, count and revision.
func (f *fakeRancher) serveSteveList(w http.ResponseWriter) {
	if f.steveListStatus != 0 {
		writeJSON(w, f.steveListStatus, statusBody(f.steveListStatus, "the Steve API refused the list"))
		return
	}
	if f.steveListBody != "" {
		_, _ = io.WriteString(w, f.steveListBody)
		return
	}
	data := make([]any, len(f.steveItems))
	for i, item := range f.steveItems {
		data[i] = item
	}
	writeJSON(w, 200, map[string]any{"type": "collection", "data": data, "count": len(data), "revision": "1"})
}

// serveNorman serves the Norman node GET and the action POST.
func (f *fakeRancher) serveNorman(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, postRecord{path: r.URL.Path, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), body: body})
		if f.normanPostDrop {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		if f.normanPostStatus != 0 {
			writeJSON(w, f.normanPostStatus, map[string]any{"type": "error", "status": f.normanPostStatus, "message": "Rancher refused the action"})
			return
		}
		writeJSON(w, 200, map[string]any{"type": "node"})
		return
	}
	if f.normanStatus != 0 {
		writeJSON(w, f.normanStatus, map[string]any{"type": "error", "status": f.normanStatus, "message": "Rancher refused the read"})
		return
	}
	if f.normanBody != "" {
		_, _ = io.WriteString(w, f.normanBody)
		return
	}
	if len(f.norman) == 0 {
		writeJSON(w, 404, map[string]any{"type": "error", "status": 404, "message": "not found"})
		return
	}
	answer := f.norman[0]
	if len(f.norman) > 1 {
		f.norman = f.norman[1:]
	}
	writeJSON(w, 200, answer)
}

// serveReview answers a SelfSubjectAccessReview.
func (f *fakeRancher) serveReview(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.reviews = append(f.reviews, body)
	if f.reviewStatus != 0 {
		writeJSON(w, f.reviewStatus, statusBody(f.reviewStatus, "the review is refused"))
		return
	}
	status := map[string]any{"allowed": !f.reviewDenied}
	if f.reviewEvalError != "" {
		status["evaluationError"] = f.reviewEvalError
	}
	writeJSON(w, 201, map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "spec": body["spec"], "status": status})
}

// normanSeen are the requests of the Norman API, in order.
func (f *fakeRancher) normanSeen() []string {
	var out []string
	for _, r := range f.seen() {
		if strings.Contains(r, " /v3/nodes/") {
			out = append(out, r)
		}
	}
	return out
}

// postsSeen are the Norman action requests received.
func (f *fakeRancher) postsSeen() []postRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts)
}

// kubernetesWrites counts the PUTs received.
func (f *fakeRancher) kubernetesWrites() int { return len(f.writes()) }

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
	out["id"] = "c-1/" + meta["name"].(string)
	out["type"] = "management.cattle.io.node"
	out["links"] = map[string]any{"self": "https://rancher.example/v1/management.cattle.io.nodes/c-1/" + meta["name"].(string)}
	return out
}

// normanNode is the Norman API's node: its state and a map with a link for
// each action.
func normanNode(id, state string, actions ...string) map[string]any {
	out := map[string]any{"id": id, "type": "node", "state": state}
	if actions != nil {
		links := map[string]any{}
		for _, a := range actions {
			links[a] = "https://rancher.example/v3/nodes/" + id + "?action=" + a
		}
		out["actions"] = links
	}
	return out
}

// testNodeActions is the node action port over the fake server.
type testNodeActions struct{ f *fakeRancher }

func (a testNodeActions) NodeActions() (engine.NodeActionClient, error) {
	return normanTestClient(a), nil
}

type normanTestClient struct{ f *fakeRancher }

func (c normanTestClient) path(cluster, node string) string {
	return c.f.srv.URL + "/v3/nodes/" + cluster + ":" + node
}

func (c normanTestClient) GetNode(ctx context.Context, cluster, node string) (engine.NormanNode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.path(cluster, node), nil)
	if err != nil {
		return engine.NormanNode{}, err
	}
	resp, err := c.f.srv.Client().Do(req)
	if err != nil {
		return engine.NormanNode{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return engine.NormanNode{}, clierr.FromResponse("GET on the Rancher server", http.MethodGet, resp.StatusCode, body, nil)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return engine.NormanNode{}, clierr.New(clierr.CodeGeneric, "could not read management node "+node+" of cluster "+cluster+": not a JSON object").WithRetryable(false)
	}
	var out engine.NormanNode
	out.State, _ = obj["state"].(string)
	if actions, ok := obj["actions"].(map[string]any); ok {
		for name := range actions {
			out.Actions = append(out.Actions, name)
		}
		slices.Sort(out.Actions)
		out.HasActions = len(out.Actions) > 0
	}
	return out, nil
}

func (c normanTestClient) PostNodeAction(ctx context.Context, cluster, node string, action engine.NodeAction, body []byte) error {
	var written atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { written.Store(true) }})
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path(cluster, node)+"?action="+string(action), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.f.srv.Client().Do(req)
	if err != nil {
		if written.Load() {
			return &engine.ActionOutcomeUnknown{Err: err}
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == 500 || resp.StatusCode == 502 || resp.StatusCode == 504:
		return &engine.ActionOutcomeUnknown{Err: clierr.ResponseError(http.MethodPost, resp.StatusCode, answer), Body: answer}
	}
	return &engine.ActionRefused{Err: clierr.FromResponse("POST on the Rancher server", http.MethodPost, resp.StatusCode, answer, nil), Body: answer}
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
	f.steveItems = []map[string]any{f.steve}
	f.reads = []map[string]any{obj}
	f.norman = []map[string]any{normanNode("c-1:m-1", "active", "drain")}
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
