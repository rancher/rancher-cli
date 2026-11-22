package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// dwObject is the object every delete row deletes.
	dwObject = "obj1"
	// dwCluster is the downstream cluster of every row; a project's
	// backing namespace is dwCluster-<project>.
	dwCluster   = "c-test"
	dwNamespace = "ns1"
	dwBacking   = dwCluster + "-" + dwObject
	// dwMember is the one namespace of the project.
	dwMember = "ns-a"
	// dwAfter is how many GETs of a name after its delete still return
	// the object before the server responds NOT_FOUND.
	dwAfter = 2

	dwOwnTokenNote = "the server accepted the delete request for the token this command used; the command cannot read with a deleted token, and the deletion is not confirmed; " +
		"log in again before the next command (if RANCHER_CLI_TOKEN holds this token, set it to another token or unset it)"
	dwBackingRefusedWarning = "you cannot get namespace " + dwBacking + " (the project's backing namespace on the local cluster); the command does not wait for it"
)

// dwRefusedWarning is the warning of a wait that ended at a refused read of
// an object the command read before the delete.
func dwRefusedWarning(kind, name string) string {
	return "your access to " + kind + " " + name + " ended after the delete request; the deletion was requested and not confirmed"
}

// dwRequest is one request a fake client received; rv and uid are the
// precondition a delete carried.
type dwRequest struct {
	cluster, verb, ns, name string
	gvr                     schema.GroupVersionResource
	rv, uid                 string
}

// dwServer is what every client of one run talks to. An object of any name
// is there until a delete that is not a dry run deletes it; a GET of a
// deleted name returns the object dwAfter more times (n sets that count per
// name), then NOT_FOUND; a name in never returns the object for good. The
// delete of a Project also deletes its backing namespace, as Rancher does.
type dwServer struct {
	mu    sync.Mutex
	reqs  []dwRequest
	n     map[string]int
	never map[string]bool
	gone  map[string]bool
	after map[string]int
	// uid is the uid of every object; "" serves objects without one.
	uid string
	// afterGet returns the response to the i-th GET of a name after its
	// delete, before the rule above; ok false leaves the response to the
	// rule.
	afterGet func(name string, i int) (obj *unstructured.Unstructured, err error, ok bool)
	// beforeBacking is the response to a GET of the backing namespace before
	// the project's delete; nil serves the namespace.
	beforeBacking error
	// getErr is the response to every GET of the deleted object before its
	// delete.
	getErr error
	// deleteErr is the response to the delete that is not a dry run.
	deleteErr error
	// derived serves a management-token with isDerived true, and after its
	// delete every read of the tokens resource responds with 403 and an
	// empty body, as Rancher responds to a read made with a deleted token.
	derived bool
	// cancel is called at the first GET after a delete.
	cancel func()
	// listErr and watchErr are the responses of ListObject and WatchObject
	// of a watching client.
	listErr, watchErr error
	// terminating serves dwMember as a namespace that is being deleted
	// before the run starts: the List says Terminating, and its GETs respond
	// as after a delete, with no delete sent.
	terminating bool
	// extraMembers are further namespaces of the project the List serves;
	// a name in notFoundOnDelete responds NOT_FOUND to its delete.
	extraMembers     []string
	notFoundOnDelete map[string]bool
	// refuseGet responds to every GET of dwObject before its delete with the
	// authorizer's refusal of get, as for a caller whose role grants delete
	// and not get.
	refuseGet bool
	// dryRunErr is the response to the dry-run delete of dwObject.
	dryRunErr error
	// noResponseUID serves the response to a dry-run delete with no uid.
	noResponseUID bool
}

func newDWServer() *dwServer {
	return &dwServer{n: map[string]int{}, never: map[string]bool{}, gone: map[string]bool{}, after: map[string]int{}, notFoundOnDelete: map[string]bool{}, uid: "u1"}
}

// beingDeleted makes dwMember a namespace that is already being deleted.
func (s *dwServer) beingDeleted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminating = true
	s.gone[dwMember] = true
}

func (s *dwServer) record(cluster, verb string, gvr schema.GroupVersionResource, ns, name string) {
	s.recordDelete(cluster, verb, gvr, ns, name, "", "")
}

// recordDelete records a request with the precondition it carried.
func (s *dwServer) recordDelete(cluster, verb string, gvr schema.GroupVersionResource, ns, name, rv, uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, dwRequest{cluster: cluster, verb: verb, gvr: gvr, ns: ns, name: name, rv: rv, uid: uid})
}

func (s *dwServer) requests() []dwRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// afterDelete is the requests after the delete of dwObject that is not a
// dry run; nil when there was none.
func (s *dwServer) afterDelete() []dwRequest {
	reqs := s.requests()
	for i, r := range reqs {
		if r.verb == "delete" && r.name == dwObject {
			return reqs[i+1:]
		}
	}
	return nil
}

func (s *dwServer) count(verb, name string, reqs []dwRequest) int {
	n := 0
	for _, r := range reqs {
		if r.verb == verb && r.name == name {
			n++
		}
	}
	return n
}

func (s *dwServer) object(gvr schema.GroupVersionResource, ns, name, uid string) *unstructured.Unstructured {
	apiVersion := gvr.Version
	if apiVersion == "" {
		apiVersion = "v1"
	}
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + apiVersion
	}
	meta := map[string]any{"name": name, "resourceVersion": "5"}
	if ns != "" {
		meta["namespace"] = ns
	}
	if uid != "" {
		meta["uid"] = uid
	}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": "Object", "metadata": meta}}
	switch {
	case dwIsProjects(gvr):
		obj.Object["kind"] = "Project"
		_ = unstructured.SetNestedField(obj.Object, dwCluster+"-"+name, "status", "backingNamespace")
	case dwIsTokens(gvr) && s.derived:
		obj.Object["isDerived"] = true
	case gvr.Resource == "namespaces":
		obj.Object["kind"] = "Namespace"
		_ = unstructured.SetNestedField(obj.Object, "Active", "status", "phase")
		// A namespace belongs to the project dwObject on dwCluster through its
		// field.cattle.io/projectId annotation and label.
		obj.SetAnnotations(map[string]string{"field.cattle.io/projectId": dwCluster + ":" + dwObject})
		obj.SetLabels(map[string]string{"kubernetes.io/metadata.name": name, "field.cattle.io/projectId": dwObject})
	}
	return obj
}

func dwIsProjects(gvr schema.GroupVersionResource) bool {
	return gvr.Group == "management.cattle.io" && gvr.Resource == "projects"
}

func dwIsTokens(gvr schema.GroupVersionResource) bool {
	return gvr.Group == "management.cattle.io" && gvr.Resource == "tokens"
}

func dwNotFound(name string) error { return clierr.New(clierr.CodeNotFound, name+" not found") }

// dwForbidden is a 403 with an empty body.
func dwForbidden() error {
	return clierr.FromResponse("", http.MethodGet, http.StatusForbidden, nil, nil)
}

func dwAuthErr() error {
	return clierr.New(clierr.CodeAuth, "the server rejected the credential (401)").WithHint(clierr.Rancher + " auth login")
}

func dwTLSErr() error {
	return clierr.New(clierr.CodeTLSTrust, "the server's certificate is not trusted")
}

func (s *dwServer) get(cluster string, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	s.record(cluster, "get", gvr, ns, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gone[name] {
		switch {
		case name == dwObject && s.getErr != nil:
			return nil, s.getErr
		case name == dwObject && s.refuseGet:
			return nil, dwAuthorizerRefusal(gvr, ns, name)
		case name == dwBacking && s.beforeBacking != nil:
			return nil, s.beforeBacking
		}
		return s.object(gvr, ns, name, s.uid), nil
	}
	s.after[name]++
	i := s.after[name]
	if i == 1 && s.cancel != nil {
		s.cancel()
	}
	if s.derived && dwIsTokens(gvr) {
		return nil, dwForbidden()
	}
	if s.afterGet != nil {
		if obj, err, ok := s.afterGet(name, i); ok {
			return obj, err
		}
	}
	limit, ok := s.n[name]
	if !ok {
		limit = dwAfter
	}
	if s.never[name] || i <= limit {
		return s.object(gvr, ns, name, s.uid), nil
	}
	return nil, dwNotFound(name)
}

func (s *dwServer) delete(cluster string, gvr schema.GroupVersionResource, ns, name, rv, uid string, dryRun bool) error {
	verb := "delete"
	if dryRun {
		verb = "dry-run delete"
	}
	s.recordDelete(cluster, verb, gvr, ns, name, rv, uid)
	if dryRun {
		if name == dwObject {
			return s.dryRunErr
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.notFoundOnDelete[name] {
		return dwNotFound(name)
	}
	s.gone[name] = true
	if dwIsProjects(gvr) {
		s.gone[dwCluster+"-"+name] = true
	}
	return nil
}

// dwClient is one cluster's client of a dwServer.
type dwClient struct {
	s       *dwServer
	cluster string
}

var _ engine.ClusterClient = (*dwClient)(nil)

func (c *dwClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	return c.s.get(c.cluster, gvr, ns, name)
}

func (c *dwClient) List(_ context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	c.s.record(c.cluster, "list", gvr, ns, "")
	out := &unstructured.UnstructuredList{}
	if gvr.Resource == "namespaces" {
		member := c.s.object(gvr, "", dwMember, c.s.uid)
		c.s.mu.Lock()
		if c.s.terminating {
			_ = unstructured.SetNestedField(member.Object, "Terminating", "status", "phase")
		}
		c.s.mu.Unlock()
		out.Items = append(out.Items, *member)
		for _, name := range c.s.extraMembers {
			out.Items = append(out.Items, *c.s.object(gvr, "", name, c.s.uid))
		}
	}
	return out, nil
}

func (c *dwClient) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, _ bool) (*unstructured.Unstructured, error) {
	c.s.record(c.cluster, "create", gvr, ns, obj.GetName())
	out := obj.DeepCopy()
	if gvr.Resource == "selfusers" {
		_ = unstructured.SetNestedField(out.Object, "u-caller", "status", "userID")
	}
	return out, nil
}

func (c *dwClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, errors.New("dwClient: unexpected update")
}

func (c *dwClient) Delete(_ context.Context, gvr schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	return c.s.delete(c.cluster, gvr, ns, name, rv, "", dryRun)
}

var _ engine.UIDDeleter = (*dwClient)(nil)

// DeleteWithUID deletes as Delete does with a uid precondition; the response
// to a dry run carries the server's uid, or none with noResponseUID.
func (c *dwClient) DeleteWithUID(_ context.Context, gvr schema.GroupVersionResource, ns, name, uid string, dryRun bool) (string, error) {
	if err := c.s.delete(c.cluster, gvr, ns, name, "", uid, dryRun); err != nil || !dryRun {
		return "", err
	}
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.noResponseUID {
		return "", nil
	}
	return c.s.uid, nil
}

func (c *dwClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", errors.New("dwClient: unexpected logs")
}

func (c *dwClient) PostReview(_ context.Context, req engine.ReviewRequest) (engine.ReviewResponse, error) {
	c.s.record(c.cluster, "review", schema.GroupVersionResource{}, "", "")
	return engine.ReviewResponse{Status: http.StatusCreated, Object: &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"allowed": true},
	}}}, nil
}

func (c *dwClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("dwClient: unexpected discovery")
}

// dwNoUIDClient is a client that cannot send a uid precondition: embedding
// the interface hides DeleteWithUID.
type dwNoUIDClient struct{ engine.ClusterClient }

// dwAuthorizerRefusal is the 403 the API server's authorizer writes, as the
// cluster client returns it, for a GET of name in ns by a caller whose role
// grants delete and not get.
func dwAuthorizerRefusal(gvr schema.GroupVersionResource, ns, name string) error {
	where := " at the cluster scope"
	switch {
	case ns != "":
		where = " in the namespace " + strconv.Quote(ns)
	case gvr.Resource == "namespaces" && gvr.Group == "":
		// The API server words a GET of a namespace as a request in that
		// namespace.
		where = " in the namespace " + strconv.Quote(name)
	}
	se := apierrors.NewForbidden(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, name,
		fmt.Errorf("User %q cannot get resource %q in API group %q%s", "u-delete", gvr.Resource, gvr.Group, where))
	return clierr.FromStatus(se, &clierr.Request{Verb: "get", Group: gvr.Group, Resource: gvr.Resource, Name: name})
}

// dwWatchClient is a dwClient that can list and watch one object.
type dwWatchClient struct{ *dwClient }

var _ engine.ObjectWatcher = dwWatchClient{}

func (c dwWatchClient) ListObject(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, string, error) {
	c.s.record(c.cluster, "listobject", gvr, ns, name)
	if c.s.listErr != nil {
		return nil, "", c.s.listErr
	}
	return c.s.object(gvr, ns, name, c.s.uid), "5", nil
}

func (c dwWatchClient) WatchObject(_ context.Context, gvr schema.GroupVersionResource, ns, name, _ string, _ time.Duration) (<-chan engine.ObjectEvent, error) {
	c.s.record(c.cluster, "watch", gvr, ns, name)
	if c.s.watchErr != nil {
		return nil, c.s.watchErr
	}
	return nil, engine.ErrWatchUnavailable
}

// dwFactory hands out one client per cluster id, each recording the
// requests it gets with that id.
type dwFactory struct {
	s     *dwServer
	watch bool
	// noUID hands out clients that cannot send a uid precondition.
	noUID   bool
	mu      sync.Mutex
	clients map[string]engine.ClusterClient
}

func (f *dwFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clients == nil {
		f.clients = map[string]engine.ClusterClient{}
	}
	if c, ok := f.clients[id]; ok {
		return c, nil
	}
	var c engine.ClusterClient = &dwClient{s: f.s, cluster: id}
	if f.watch {
		c = dwWatchClient{&dwClient{s: f.s, cluster: id}}
	}
	if f.noUID {
		c = dwNoUIDClient{c}
	}
	f.clients[id] = c
	return c, nil
}

func (f *dwFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

func (f *dwFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	if s == "" {
		return engine.LocalCluster, nil
	}
	return s, nil
}

// dwNamer names the token the command uses.
type dwNamer struct{ id string }

func (n dwNamer) ActiveTokenID() (string, error) { return n.id, nil }

// dwRow is one delete command of the registry and the flags that route it.
type dwRow struct {
	// name titles the row's subtests; command is the registered command it
	// runs, which differs from name for the row of a flag.
	name    string
	command string
	// flagRun is the row of delete project with --delete-namespaces.
	flagRun bool
	k       kinds.Kind
	e       entry
	routing map[string]string
	// cluster is the cluster whose client reads and deletes the object.
	cluster string
}

func (r dwRow) project() bool { return r.k.Key == "project" }

// registeredRows are the rows that run a registered command by its name.
func registeredRows(rows []dwRow) int {
	n := 0
	for _, r := range rows {
		if !r.flagRun {
			n++
		}
	}
	return n
}

// dwRows is every entry whose command name starts with "delete ".
func dwRows(t *testing.T) []dwRow {
	t.Helper()
	var rows []dwRow
	for _, e := range buildEntries() {
		d := e.make(runDeps{}).Describe()
		if !strings.HasPrefix(d.Name, "delete ") {
			continue
		}
		k, ok := kinds.Lookup(strings.TrimPrefix(d.Name, "delete "))
		require.True(t, ok, "no kind row for %s", d.Name)
		routing := map[string]string{}
		for _, f := range d.Flags {
			if !f.Required {
				continue
			}
			switch f.Name {
			case "cluster":
				routing["cluster"] = dwCluster
			case "namespace":
				routing["namespace"] = dwNamespace
			default:
				t.Fatalf("%s requires --%s, which the table does not fill", d.Name, f.Name)
			}
		}
		if k.ProjectBackingNamespace {
			routing["namespace"] = dwNamespace
		}
		cluster := dwCluster
		if k.Scope == kinds.Management {
			cluster = engine.LocalCluster
		}
		row := dwRow{name: d.Name, command: d.Name, k: k, e: e, routing: routing, cluster: cluster}
		rows = append(rows, row)
		if row.project() {
			flagRow := row
			flagRow.name += " --" + resources.FlagDeleteNamespaces
			flagRow.flagRun = true
			rows = append(rows, flagRow)
		}
	}
	return rows
}

// input is the row's routing flags plus the given string and bool flags.
func (r dwRow) input(values map[string]string, bools map[string]bool) fieldCoverageInput {
	v := map[string]string{}
	maps.Copy(v, r.routing)
	maps.Copy(v, values)
	b := map[string]bool{}
	maps.Copy(b, bools)
	if r.flagRun {
		b[resources.FlagDeleteNamespaces] = true
	}
	if len(b) == 0 {
		b = nil
	}
	return fieldCoverageInput{args: []string{dwObject}, values: v, bools: b}
}

// dwRun is what one Plan and executor run gave.
type dwRun struct {
	planErr, execErr error
	res              runtime.Result
	elapsed          time.Duration
}

// run plans the row's command against s and runs its executor.
func (r dwRow) run(t *testing.T, s *dwServer, namer engine.SessionTokenNamer, watch bool, in fieldCoverageInput) dwRun {
	t.Helper()
	deps := runDeps{clients: &dwFactory{s: s, watch: watch}, roots: failingRoots{}, sessionNamer: namer}
	cmd, ok := r.e.make(deps).(runtime.MutatingCommand)
	require.True(t, ok, "%s is a mutating command", r.name)
	_, exec, err := cmd.Plan(t.Context(), in)
	if err != nil {
		return dwRun{planErr: err}
	}
	start := time.Now()
	res, err := exec(t.Context())
	return dwRun{res: res, execErr: err, elapsed: time.Since(start)}
}

// dwData is the data of a delete result as the JSON envelope carries it.
type dwData struct {
	Deleted string `json:"deleted"`
	Waited  *bool  `json:"waited"`
}

// dwObjectItem is an item of the objects a project delete reports.
type dwObjectItem struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

// dwOutcomes maps "kind/name" to the outcome of each item of objects as the
// JSON envelope carries them.
func dwOutcomes(t *testing.T, objects any) map[string]string {
	t.Helper()
	raw, err := json.Marshal(objects)
	require.NoError(t, err)
	var entries []dwObjectItem
	require.NoError(t, json.Unmarshal(raw, &entries))
	out := map[string]string{}
	for _, e := range entries {
		out[e.Kind+"/"+e.Name] = e.Outcome
	}
	return out
}

func (run dwRun) data(t *testing.T) dwData {
	t.Helper()
	raw, err := json.Marshal(run.res.Data)
	require.NoError(t, err)
	var d dwData
	require.NoError(t, json.Unmarshal(raw, &d))
	require.NotNil(t, d.Waited, "the result data has waited")
	return d
}

func (run dwRun) outcomes(t *testing.T) map[string]string {
	t.Helper()
	raw, err := json.Marshal(run.res.Data)
	require.NoError(t, err)
	var data map[string]any
	require.NoError(t, json.Unmarshal(raw, &data))
	return dwOutcomes(t, data["objects"])
}

func (run dwRun) firstLine() string {
	line, _, _ := strings.Cut(run.res.Text, "\n")
	return line
}

// requireSuccess asserts the run succeeded and its data, first line and
// warnings.
func (run dwRun) requireSuccess(t *testing.T, waited bool, warnings []string) {
	t.Helper()
	require.NoError(t, run.planErr)
	require.NoError(t, run.execErr)
	d := run.data(t)
	assert.Equal(t, dwObject, d.Deleted)
	assert.Equal(t, waited, *d.Waited)
	if waited {
		assert.True(t, strings.HasSuffix(run.firstLine(), " deleted"), run.firstLine())
	} else {
		assert.True(t, strings.HasSuffix(run.firstLine(), " deletion started"), run.firstLine())
	}
	assert.Equal(t, warnings, run.res.Warnings)
}

// dwDetails is the details of an error as the JSON envelope carries them.
func dwDetails(t *testing.T, ce *clierr.Error) map[string]any {
	t.Helper()
	raw, err := json.Marshal(ce.Details)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// requireCommittedError asserts the run's executor ended with code and
// committed details that name a get command, and returns the error.
func (run dwRun) requireCommittedError(t *testing.T, r dwRow, code string, exit int) *clierr.Error {
	t.Helper()
	require.NoError(t, run.planErr)
	ce, ok := errors.AsType[*clierr.Error](run.execErr)
	require.True(t, ok, "a clierr error: %v", run.execErr)
	assert.Equal(t, code, ce.Code)
	assert.Equal(t, exit, ce.ExitCode())
	assert.False(t, ce.Retryable)
	details := dwDetails(t, ce)
	assert.Equal(t, true, details["committed"])
	recovery, _ := details["recovery"].(string)
	assert.True(t, strings.HasPrefix(recovery, clierr.Rancher+" get "), "recovery %q", recovery)
	if r.project() {
		assert.Contains(t, details, "objects")
	}
	return ce
}

// TestEveryDeleteWaitsUntilTheObjectIsGone asserts, for every delete
// command the registry builds, the wait after its delete: it reads until
// the server responds NOT_FOUND or an object of that name has a different
// uid, --wait=false sends nothing after the delete, a timeout, an interrupt, an
// authentication or certificate failure end with an error that carries the
// committed details and a get command that reads the object the delete
// read, and a refused read of the object ends with a warning.
func TestEveryDeleteWaitsUntilTheObjectIsGone(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	gets := map[string]entry{}
	for _, e := range buildEntries() {
		if name := e.make(runDeps{}).Describe().Name; strings.HasPrefix(name, "get ") {
			gets[name] = e
		}
	}
	rows := dwRows(t)
	var registered int
	for _, c := range wr.reg.Commands() {
		if strings.HasPrefix(c.Describe().Name, "delete ") {
			registered++
		}
	}
	require.Equal(t, registered, registeredRows(rows), "one row per registered delete command")

	for _, r := range rows {
		t.Run(r.name+" default", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				run := r.run(t, s, nil, false, r.input(nil, nil))
				run.requireSuccess(t, true, nil)
				after := s.afterDelete()
				assert.Equal(t, dwAfter+1, s.count("get", dwObject, after))
				if r.project() {
					assert.Equal(t, dwAfter+1, s.count("get", dwBacking, after))
					for _, req := range s.requests() {
						if req.name == dwBacking {
							assert.Equal(t, engine.LocalCluster, req.cluster, "the backing namespace is read on the local cluster")
						}
					}
				}
				for _, req := range s.requests() {
					if req.name == dwObject {
						assert.Equal(t, r.cluster, req.cluster, "%s %s", req.verb, req.name)
					}
					if req.verb == "delete" {
						assert.Equal(t, "5", req.rv, "the delete of %s carries the resourceVersion the command read", req.name)
						assert.Empty(t, req.uid, "the delete of %s carries no uid precondition", req.name)
					}
				}
				if r.flagRun {
					dwRequireNamespacesFirst(t, s, run)
				}
			})
		})

		t.Run(r.name+" --wait=false", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				run := r.run(t, s, nil, false, r.input(nil, map[string]bool{"wait": false}))
				run.requireSuccess(t, false, nil)
				require.NotNil(t, s.afterDelete())
				assert.Empty(t, s.afterDelete(), "nothing is sent after the delete")
				assert.Zero(t, s.count("get", dwBacking, s.requests()), "the backing namespace is not read")
				if r.flagRun {
					assert.Zero(t, s.count("get", dwMember, s.requests()), "the namespace is not read")
					assert.Equal(t, map[string]string{"namespace/" + dwMember: "deletion started", "project/" + dwObject: "deletion started"}, run.outcomes(t))
				}
			})
		})

		t.Run(r.name+" different uid", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.afterGet = func(name string, _ int) (*unstructured.Unstructured, error, bool) {
					obj := s.object(schema.GroupVersionResource{Version: "v1", Resource: "objects"}, "", name, "u2")
					return obj, nil, true
				}
				run := r.run(t, s, nil, false, r.input(nil, nil))
				run.requireSuccess(t, true, nil)
				assert.Equal(t, 1, s.count("get", dwObject, s.afterDelete()), "an object with a different uid is a new object")
			})
		})

		t.Run(r.name+" no uid after the delete", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
					if i > dwAfter {
						return nil, nil, false
					}
					return s.object(schema.GroupVersionResource{Version: "v1", Resource: "objects"}, "", name, ""), nil, true
				}
				run := r.run(t, s, nil, false, r.input(nil, nil))
				run.requireSuccess(t, true, nil)
				assert.Equal(t, dwAfter+1, s.count("get", dwObject, s.afterDelete()), "an object without a uid is not a new object")
			})
		})

		t.Run(r.name+" no uid before the delete", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.uid = ""
				s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
					if i > dwAfter {
						return nil, nil, false
					}
					return s.object(schema.GroupVersionResource{Version: "v1", Resource: "objects"}, "", name, "u2"), nil, true
				}
				run := r.run(t, s, nil, false, r.input(nil, nil))
				run.requireSuccess(t, true, nil)
				assert.Equal(t, dwAfter+1, s.count("get", dwObject, s.afterDelete()), "with no uid read before, only NOT_FOUND ends the wait")
			})
		})

		t.Run(r.name+" timeout", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.never[dwObject], s.never[dwBacking] = true, true
				if r.flagRun {
					s.n[dwMember] = 0
				}
				run := r.run(t, s, nil, false, r.input(map[string]string{"timeout": "1s"}, nil))
				ce := run.requireCommittedError(t, r, clierr.CodeGeneric, 1)
				assert.Equal(t, time.Second, run.elapsed)
				require.ErrorContains(t, ce, "timed out after 1s waiting for "+r.k.Name+" "+dwObject+" to be deleted; the deletion was requested")
				recovery, _ := dwDetails(t, ce)["recovery"].(string)
				dwRequireRecoveryReadsTheObject(t, r, gets, s, recovery)
			})
		})

		t.Run(r.name+" interrupt", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				s.cancel = cancel
				_, ce := dwRunPipeline(ctx, t, wr.reg, r, s, nil, r.input(nil, nil), false)
				require.NotNil(t, ce)
				assert.Equal(t, clierr.CodeInterrupted, ce.Code)
				assert.Equal(t, 130, ce.ExitCode())
				assert.Equal(t, true, dwDetails(t, ce)["committed"])
			})
		})

		for _, tc := range []struct {
			name string
			err  func() error
			code string
			exit int
		}{
			{"authentication failure", dwAuthErr, clierr.CodeAuth, 3},
			{"certificate failure", dwTLSErr, clierr.CodeTLSTrust, 10},
		} {
			t.Run(r.name+" "+tc.name+" after the delete", func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := newDWServer()
					s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
						if name == dwObject && i == 1 {
							return nil, tc.err(), true
						}
						return nil, nil, false
					}
					run := r.run(t, s, nil, false, r.input(nil, nil))
					_ = run.requireCommittedError(t, r, tc.code, tc.exit)
					assert.Len(t, s.afterDelete(), 1, "the wait ends at the first such response")
				})
			})
		}

		t.Run(r.name+" read refused after the delete", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
					if name == dwObject && i == 1 {
						return nil, dwForbidden(), true
					}
					return nil, nil, false
				}
				run := r.run(t, s, nil, false, r.input(nil, nil))
				run.requireSuccess(t, false, []string{dwRefusedWarning(r.k.Name, dwObject)})
				after := s.afterDelete()
				assert.Equal(t, 1, s.count("get", dwObject, after), "one GET of the object after the delete")
				if r.project() {
					assert.Equal(t, dwAfter+1, s.count("get", dwBacking, after), "the backing namespace is still waited for")
				}
			})
		})

		t.Run(r.name+" dry run", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				planned := newDWServer()
				cmd, ok := r.e.make(runDeps{clients: &dwFactory{s: planned}, roots: failingRoots{}}).(runtime.MutatingCommand)
				require.True(t, ok)
				_, _, err := cmd.Plan(t.Context(), r.input(nil, nil))
				require.NoError(t, err)

				s := newDWServer()
				status, ce := dwRunPipeline(t.Context(), t, wr.reg, r, s, nil, r.input(nil, nil), true)
				require.Nil(t, ce)
				assert.Equal(t, 0, status)
				assert.Equal(t, planned.requests(), s.requests(), "nothing is sent after Plan returns")
			})
		})

		for _, tc := range []struct {
			name   string
			values map[string]string
			bools  map[string]bool
		}{
			{"--timeout 0s", map[string]string{"timeout": "0s"}, nil},
			{"--wait=false --timeout 1m", map[string]string{"timeout": "1m"}, map[string]bool{"wait": false}},
		} {
			t.Run(r.name+" dry run "+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := newDWServer()
					_, ce := dwRunPipeline(t.Context(), t, wr.reg, r, s, nil, r.input(tc.values, tc.bools), true)
					require.NotNil(t, ce)
					assert.Equal(t, clierr.CodeValidation, ce.Code)
					assert.Equal(t, 2, ce.ExitCode())
					assert.Empty(t, s.requests(), "no request at all")
				})
			})
		}

		if r.project() {
			dwProjectRows(t, r)
		}
		if r.flagRun {
			dwFlagRows(t, r, wr.reg, gets)
		}
	}
}

// dwProjectRows asserts the rows only delete project has: the read of the
// backing namespace before the delete and the one time limit of the wait.
func dwProjectRows(t *testing.T, r dwRow) {
	t.Helper()
	t.Run(r.name+" backing namespace refused before the delete", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.beforeBacking = dwForbidden()
			run := r.run(t, s, nil, false, r.input(nil, nil))
			run.requireSuccess(t, false, []string{dwBackingRefusedWarning})
			assert.Zero(t, s.count("get", dwBacking, s.afterDelete()))
			assert.Equal(t, dwAfter+1, s.count("get", dwObject, s.afterDelete()))
		})
	})
	t.Run(r.name+" backing namespace not found before the delete", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.beforeBacking = dwNotFound(dwBacking)
			run := r.run(t, s, nil, false, r.input(nil, nil))
			run.requireSuccess(t, true, nil)
			assert.Zero(t, s.count("get", dwBacking, s.afterDelete()))
		})
	})
	t.Run(r.name+" backing namespace read fails before the delete", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.beforeBacking = clierr.FromResponse("", http.MethodGet, http.StatusInternalServerError, nil, nil)
			run := r.run(t, s, nil, false, r.input(nil, nil))
			require.Error(t, run.planErr)
			assert.Zero(t, s.count("delete", dwObject, s.requests())+s.count("dry-run delete", dwObject, s.requests()), "no delete is sent")
		})
	})
	t.Run(r.name+" one time limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.n[dwObject] = 1
			s.never[dwBacking] = true
			if r.flagRun {
				s.n[dwMember] = 0
			}
			run := r.run(t, s, nil, false, r.input(map[string]string{"timeout": "12s"}, nil))
			ce := run.requireCommittedError(t, r, clierr.CodeGeneric, 1)
			assert.Equal(t, 12*time.Second, run.elapsed, "one time limit covers the project and its backing namespace")
			require.ErrorContains(t, ce, "timed out after 12s waiting for namespace "+dwBacking+" to be deleted")
			assert.Equal(t, 2, s.count("get", dwObject, s.afterDelete()))
		})
	})
}

// dwProjectKept is the end of the message of a run that stopped because of a
// namespace and did not delete the project.
const dwProjectKept = "; the project was kept; run the same command again, which reads the project's current namespaces"

// dwProjectRecovery is the get command the details of an error give as the
// recovery when the project was not deleted.
var dwProjectRecovery = clierr.Rancher + " get project '" + dwObject + "' --cluster '" + dwCluster + "'"

// dwRequireNamespacesFirst asserts, for a successful run with
// --delete-namespaces, that the namespace is read dwAfter+1 times after its
// delete, all before the project's delete, that every request goes to the
// cluster that holds the object, and that the result lists the namespace and
// the project as deleted.
func dwRequireNamespacesFirst(t *testing.T, s *dwServer, run dwRun) {
	t.Helper()
	reqs := s.requests()
	memberDelete, projectDelete, lastMemberGet := -1, -1, -1
	var memberGets int
	for i, req := range reqs {
		switch {
		case req.verb == "delete" && req.name == dwMember:
			memberDelete = i
		case req.verb == "delete" && req.name == dwObject:
			projectDelete = i
		case req.verb == "get" && req.name == dwMember && memberDelete >= 0:
			memberGets++
			lastMemberGet = i
		}
		switch req.name {
		case dwMember:
			assert.Equal(t, dwCluster, req.cluster, "%s %s goes to the cluster of the project", req.verb, req.name)
		case dwObject, dwBacking:
			assert.Equal(t, engine.LocalCluster, req.cluster, "%s %s goes to the local cluster", req.verb, req.name)
		}
	}
	require.GreaterOrEqual(t, memberDelete, 0, "the namespace is deleted")
	require.Greater(t, projectDelete, memberDelete, "the project is deleted after the namespace")
	assert.Equal(t, dwAfter+1, memberGets, "the namespace is read until the server responds NOT_FOUND")
	assert.Less(t, lastMemberGet, projectDelete, "the project is deleted after the last read of the namespace")
	assert.Equal(t, map[string]string{"namespace/" + dwMember: "deleted", "project/" + dwObject: "deleted"}, run.outcomes(t))
	assert.Equal(t, "project/"+dwObject+" deleted", run.firstLine())
}

// dwRequireNamespaceError asserts the run failed in the wait for a namespace
// with the error code `code` and did not delete the project: committed
// details that name the project, the project not attempted, no delete of the
// project.
func dwRequireNamespaceError(t *testing.T, s *dwServer, run dwRun, code string) *clierr.Error {
	t.Helper()
	require.NoError(t, run.planErr)
	ce, ok := errors.AsType[*clierr.Error](run.execErr)
	require.True(t, ok, "a clierr error: %v", run.execErr)
	assert.Equal(t, code, ce.Code)
	assert.False(t, ce.Retryable)
	assert.True(t, strings.HasSuffix(ce.Message, dwProjectKept), ce.Message)
	details := dwDetails(t, ce)
	assert.Equal(t, true, details["committed"])
	assert.Equal(t, "project", details["resourceKind"])
	assert.Equal(t, dwObject, details["resourceName"])
	assert.Equal(t, dwProjectRecovery, details["recovery"])
	assert.Equal(t, "not attempted", dwOutcomes(t, details["objects"])["project/"+dwObject])
	assert.Zero(t, s.count("delete", dwObject, s.requests()), "the project is not deleted")
	return ce
}

// dwFlagRows asserts the rows only delete project --delete-namespaces has:
// if the wait for a namespace fails, the project is not deleted; a namespace
// that was being deleted is waited for with no delete; a refused read of a
// namespace is a warning; namespaces that are already gone or not deleted are
// not read; and one time limit covers every wait.
func dwFlagRows(t *testing.T, r dwRow, reg *runtime.Registry, gets map[string]entry) {
	t.Helper()
	for _, tc := range []struct {
		name string
		err  func() error
		code string
		exit int
	}{
		{"authentication failure", dwAuthErr, clierr.CodeAuth, 3},
		{"certificate failure", dwTLSErr, clierr.CodeTLSTrust, 10},
	} {
		t.Run(r.name+" "+tc.name+" after the delete of a namespace", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
					if name == dwMember && i == 1 {
						return nil, tc.err(), true
					}
					return nil, nil, false
				}
				run := r.run(t, s, nil, false, r.input(nil, nil))
				ce := dwRequireNamespaceError(t, s, run, tc.code)
				assert.Equal(t, tc.exit, ce.ExitCode())
				assert.NotEmpty(t, ce.Hint)
				assert.Equal(t, map[string]string{"namespace/" + dwMember: "deletion started", "project/" + dwObject: "not attempted"}, dwOutcomes(t, dwDetails(t, ce)["objects"]))
			})
		})
	}

	t.Run(r.name+" timeout of a namespace", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.never[dwMember] = true
			run := r.run(t, s, nil, false, r.input(map[string]string{"timeout": "1s"}, nil))
			ce := dwRequireNamespaceError(t, s, run, clierr.CodeGeneric)
			assert.Equal(t, time.Second, run.elapsed)
			assert.Equal(t, dwProjectRecovery, ce.Hint)
			require.ErrorContains(t, ce, "timed out after 1s waiting for namespace "+dwMember+" to be deleted; the deletion was requested")
			recovery, _ := dwDetails(t, ce)["recovery"].(string)
			dwRequireRecoveryReadsTheObject(t, r, gets, s, recovery)
			assert.Equal(t, map[string]string{"namespace/" + dwMember: "deletion started", "project/" + dwObject: "not attempted"}, dwOutcomes(t, dwDetails(t, ce)["objects"]))
		})
	})

	t.Run(r.name+" interrupt during the wait for a namespace", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s.cancel = cancel
			_, ce := dwRunPipeline(ctx, t, reg, r, s, nil, r.input(nil, nil), false)
			require.NotNil(t, ce)
			assert.Equal(t, clierr.CodeInterrupted, ce.Code)
			assert.Equal(t, 130, ce.ExitCode())
			details := dwDetails(t, ce)
			assert.Equal(t, true, details["committed"])
			assert.Equal(t, "project", details["resourceKind"])
			assert.Equal(t, dwProjectRecovery, details["recovery"])
			assert.Equal(t, "not attempted", dwOutcomes(t, details["objects"])["project/"+dwObject])
			assert.Zero(t, s.count("delete", dwObject, s.requests()), "the project is not deleted")
		})
	})

	t.Run(r.name+" a namespace that is being deleted", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.beingDeleted()
			run := r.run(t, s, nil, false, r.input(nil, nil))
			run.requireSuccess(t, true, nil)
			reqs := s.requests()
			assert.Zero(t, s.count("delete", dwMember, reqs), "no delete of the namespace")
			assert.Zero(t, s.count("dry-run delete", dwMember, reqs), "no dry-run delete of the namespace")
			assert.Equal(t, dwAfter+1, s.count("get", dwMember, reqs))
			var lastGet, projectDelete int
			for i, req := range reqs {
				if req.verb == "get" && req.name == dwMember {
					lastGet = i
				}
				if req.verb == "delete" && req.name == dwObject {
					projectDelete = i
				}
			}
			assert.Less(t, lastGet, projectDelete, "the project is deleted after the last read of the namespace")
			assert.Equal(t, map[string]string{"namespace/" + dwMember: "being deleted", "project/" + dwObject: "deleted"}, run.outcomes(t))
		})
	})

	t.Run(r.name+" a namespace that is being deleted and never goes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.beingDeleted()
			s.never[dwMember] = true
			run := r.run(t, s, nil, false, r.input(map[string]string{"timeout": "1s"}, nil))
			ce := dwRequireNamespaceError(t, s, run, clierr.CodeGeneric)
			require.ErrorContains(t, ce, "timed out after 1s waiting for namespace "+dwMember+" to be deleted")
			assert.Equal(t, map[string]string{"namespace/" + dwMember: "being deleted", "project/" + dwObject: "not attempted"}, dwOutcomes(t, dwDetails(t, ce)["objects"]))
			assert.Zero(t, s.count("delete", dwMember, s.requests()), "no delete of the namespace")
		})
	})

	t.Run(r.name+" read of a namespace refused after the delete", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.afterGet = func(name string, i int) (*unstructured.Unstructured, error, bool) {
				if name == dwMember && i == 1 {
					return nil, dwForbidden(), true
				}
				return nil, nil, false
			}
			run := r.run(t, s, nil, false, r.input(nil, nil))
			run.requireSuccess(t, false, []string{dwRefusedWarning("namespace", dwMember)})
			assert.Equal(t, 1, s.count("get", dwMember, s.requests()), "one read of the namespace")
			assert.Equal(t, map[string]string{"namespace/" + dwMember: "deletion started", "project/" + dwObject: "deleted"}, run.outcomes(t))
			after := s.afterDelete()
			assert.Equal(t, dwAfter+1, s.count("get", dwObject, after), "the project is still deleted and waited for")
			assert.Equal(t, dwAfter+1, s.count("get", dwBacking, after))
		})
	})

	t.Run(r.name+" namespaces that are already gone or not deleted are not read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.extraMembers = []string{"default", "gone-ns"}
			s.notFoundOnDelete["gone-ns"] = true
			run := r.run(t, s, nil, false, r.input(nil, nil))
			run.requireSuccess(t, true, nil)
			assert.Zero(t, s.count("get", "default", s.requests()))
			assert.Zero(t, s.count("get", "gone-ns", s.requests()))
			assert.Equal(t, map[string]string{
				"namespace/" + dwMember: "deleted", "namespace/default": "not deleted", "namespace/gone-ns": "already gone", "project/" + dwObject: "deleted",
			}, run.outcomes(t))
		})
	})

	t.Run(r.name+" one time limit for the namespace and the project", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.n[dwMember] = 1
			s.never[dwObject] = true
			run := r.run(t, s, nil, false, r.input(map[string]string{"timeout": "12s"}, nil))
			ce := run.requireCommittedError(t, r, clierr.CodeGeneric, 1)
			assert.Equal(t, 12*time.Second, run.elapsed, "one time limit covers the namespace and the project")
			require.ErrorContains(t, ce, "timed out after 12s waiting for project "+dwObject+" to be deleted")
			assert.Equal(t, 2, s.count("get", dwMember, s.requests()))
		})
	})
}

// dwRunPipeline runs the row's command through runtime.Run with --apply and
// every gate confirmed, as a dry run when dryRun is set.
func dwRunPipeline(ctx context.Context, t *testing.T, reg *runtime.Registry, r dwRow, s *dwServer, namer engine.SessionTokenNamer, in fieldCoverageInput, dryRun bool) (int, *clierr.Error) {
	t.Helper()
	deps := runDeps{clients: &dwFactory{s: s}, roots: failingRoots{}, sessionNamer: namer}
	var out, errOut bytes.Buffer
	return runtime.Run(ctx, r.e.make(deps), in, runtime.IO{
		Out: &out, Err: &errOut, Format: "json",
		Apply: true, Yes: true, AllowDeprecated: true, DryRun: dryRun,
		Capability: scrubAllowAll{}, Projects: scrubProjects{},
	}, reg.Binding(r.command))
}

// dwRequireRecoveryReadsTheObject runs the recovery command through the
// registry's get against a fresh server and asserts its GET reads the
// cluster, group, version, resource, namespace and name the delete's first
// GET read.
func dwRequireRecoveryReadsTheObject(t *testing.T, r dwRow, gets map[string]entry, deleted *dwServer, recovery string) {
	t.Helper()
	var want *dwRequest
	for _, req := range deleted.requests() {
		if req.verb == "get" && req.name == dwObject {
			want = &req
			break
		}
	}
	require.NotNil(t, want, "the delete read the object")

	words, args, flags := dwParseCommand(t, recovery)
	e, ok := gets[strings.Join(words, " ")]
	require.True(t, ok, "the recovery %q is a get command", recovery)
	s := newDWServer()
	cmd, ok := e.make(runDeps{clients: &dwFactory{s: s}, roots: failingRoots{}}).(runtime.ReadCommand)
	require.True(t, ok)
	_, err := cmd.Execute(t.Context(), fieldCoverageInput{args: args, values: flags}, nil)
	require.NoError(t, err, recovery)
	var got *dwRequest
	for _, req := range s.requests() {
		if req.verb == "get" && req.name == dwObject {
			got = &req
			break
		}
	}
	require.NotNil(t, got, "the recovery %q reads the object", recovery)
	assert.Equal(t, *want, *got, "the recovery %q reads the object the delete read", recovery)
}

// dwParseCommand splits a printed get command into its command words, its
// positional values and its flags. Every value is single-quoted.
func dwParseCommand(t *testing.T, line string) (words, args []string, flags map[string]string) {
	t.Helper()
	rest, ok := strings.CutPrefix(line, clierr.Rancher+" ")
	require.True(t, ok, "%q starts with the product name", line)
	var tokens []string
	for rest != "" {
		rest = strings.TrimLeft(rest, " ")
		if strings.HasPrefix(rest, "'") {
			end := strings.Index(rest[1:], "'")
			require.GreaterOrEqual(t, end, 0, "an unterminated quote in %q", line)
			tokens = append(tokens, rest[:end+2])
			rest = rest[end+2:]
			continue
		}
		tok, after, _ := strings.Cut(rest, " ")
		tokens = append(tokens, tok)
		rest = after
	}
	flags = map[string]string{}
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch {
		case strings.HasPrefix(tok, "--"):
			require.Less(t, i+1, len(tokens), "flag %s has a value in %q", tok, line)
			flags[strings.TrimPrefix(tok, "--")] = strings.Trim(tokens[i+1], "'")
			i++
		case strings.HasPrefix(tok, "'"):
			args = append(args, strings.Trim(tok, "'"))
		default:
			words = append(words, tok)
		}
	}
	return words, args, flags
}

// TestDeleteOfTheCommandsOwnTokenDoesNotWait asserts that the delete of the
// token the command uses, when Rancher marks it as derived, sends nothing
// after the delete and says the deletion started with a note, whatever
// --wait and --timeout say, and that a failed delete or a failed read before
// it is the error it was.
func TestDeleteOfTheCommandsOwnTokenDoesNotWait(t *testing.T) {
	t.Parallel()
	var r dwRow
	for _, row := range dwRows(t) {
		if row.name == "delete management-token" {
			r = row
		}
	}
	require.NotEmpty(t, r.name)
	namer := dwNamer{id: dwObject}
	for _, tc := range []struct {
		name   string
		values map[string]string
		bools  map[string]bool
	}{
		{"no flag", nil, nil},
		{"--wait=false", nil, map[string]bool{"wait": false}},
		{"--timeout 1m", map[string]string{"timeout": "1m"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				s.derived = true
				run := r.run(t, s, namer, false, r.input(tc.values, tc.bools))
				run.requireSuccess(t, false, []string{dwOwnTokenNote})
				require.NotNil(t, s.afterDelete())
				assert.Empty(t, s.afterDelete(), "nothing is sent after the delete")
			})
		})
	}
	t.Run("the delete fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.derived = true
			s.deleteErr = dwAuthErr()
			run := r.run(t, s, namer, false, r.input(nil, nil))
			require.NoError(t, run.planErr)
			ce, ok := errors.AsType[*clierr.Error](run.execErr)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Nil(t, ce.Details, "the delete was not accepted: no committed details")
		})
	})
	t.Run("the read before the delete fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.derived = true
			s.getErr = dwAuthErr()
			run := r.run(t, s, namer, false, r.input(nil, nil))
			ce, ok := errors.AsType[*clierr.Error](run.planErr)
			require.True(t, ok)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Zero(t, s.count("delete", dwObject, s.requests())+s.count("dry-run delete", dwObject, s.requests()), "no delete is sent")
		})
	})
}

// TestDeleteWaitWithAWatchingClient asserts the wait of a delete whose
// client can watch: a refused List makes it poll and it ends when the GET
// responds NOT_FOUND, and an authentication or certificate failure on the
// List or the watch open ends it with the committed details.
func TestDeleteWaitWithAWatchingClient(t *testing.T) {
	t.Parallel()
	var r dwRow
	for _, row := range dwRows(t) {
		if row.name == "delete configmap" {
			r = row
		}
	}
	require.NotEmpty(t, r.name)
	t.Run("a refused List", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := newDWServer()
			s.n[dwObject] = 1
			s.listErr = dwForbidden()
			run := r.run(t, s, nil, true, r.input(nil, nil))
			run.requireSuccess(t, true, nil)
			assert.Equal(t, 2, s.count("get", dwObject, s.afterDelete()))
		})
	})
	for _, tc := range []struct {
		name     string
		list     func() error
		watch    func() error
		code     string
		exit     int
		requests int
	}{
		{"an authentication failure on the List", dwAuthErr, nil, clierr.CodeAuth, 3, 1},
		{"an authentication failure on the watch open", nil, dwAuthErr, clierr.CodeAuth, 3, 2},
		{"a certificate failure on the List", dwTLSErr, nil, clierr.CodeTLSTrust, 10, 1},
		{"a certificate failure on the watch open", nil, dwTLSErr, clierr.CodeTLSTrust, 10, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newDWServer()
				if tc.list != nil {
					s.listErr = tc.list()
				}
				if tc.watch != nil {
					s.watchErr = tc.watch()
				}
				run := r.run(t, s, nil, true, r.input(nil, nil))
				_ = run.requireCommittedError(t, r, tc.code, tc.exit)
				assert.Len(t, s.afterDelete(), tc.requests)
			})
		})
	}
}
