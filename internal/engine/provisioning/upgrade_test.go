package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// upgradeClient is a fake cluster client: Get and List return what a test
// stores, one response per request or a sequence whose last entry repeats, and
// every request and update is recorded.
type upgradeClient struct {
	engine.ClusterClient
	mu      sync.Mutex
	objects map[string][]*unstructured.Unstructured
	lists   map[string][][]unstructured.Unstructured
	errs    map[string]error
	// errSeq returns one error for each request of a key, in order, before
	// errs and the stored objects: a nil entry lets the request through.
	errSeq   map[string][]error
	requests []string
	updates  []upgradeUpdate
	// updateErr, when set, is returned by every Update.
	updateErr error
	// blockLists makes every List wait until its context ends and return the
	// error of the context.
	blockLists bool
	// write, when set, receives every Update after it is recorded and returns
	// its result.
	write engine.ClusterClient
}

type upgradeUpdate struct {
	resource string
	ns       string
	dryRun   bool
	obj      *unstructured.Unstructured
}

func newUpgradeClient() *upgradeClient {
	return &upgradeClient{objects: map[string][]*unstructured.Unstructured{}, lists: map[string][][]unstructured.Unstructured{}, errs: map[string]error{}, errSeq: map[string][]error{}}
}

func objKey(gvr schema.GroupVersionResource, ns, name string) string {
	return gvr.Resource + "/" + ns + "/" + name
}

func (c *upgradeClient) put(gvr schema.GroupVersionResource, ns, name string, objs ...*unstructured.Unstructured) {
	c.objects[objKey(gvr, ns, name)] = objs
}

func (c *upgradeClient) putList(gvr schema.GroupVersionResource, ns string, lists ...[]unstructured.Unstructured) {
	c.lists[objKey(gvr, ns, "")] = lists
}

func (c *upgradeClient) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := objKey(gvr, ns, name)
	c.requests = append(c.requests, "GET "+k)
	if err := c.nextErr("GET " + k); err != nil {
		return nil, err
	}
	if err, ok := c.errs["GET "+k]; ok {
		return nil, err
	}
	seq := c.objects[k]
	if len(seq) == 0 {
		return nil, clierr.New(clierr.CodeNotFound, k+" not found")
	}
	obj := seq[0]
	if len(seq) > 1 {
		c.objects[k] = seq[1:]
	}
	return obj.DeepCopy(), nil
}

func (c *upgradeClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockLists {
		c.mu.Unlock()
		<-ctx.Done()
		c.mu.Lock()
		return nil, ctx.Err()
	}
	k := objKey(gvr, ns, "")
	c.requests = append(c.requests, "LIST "+k)
	if err := c.nextErr("LIST " + k); err != nil {
		return nil, err
	}
	if err, ok := c.errs["LIST "+k]; ok {
		return nil, err
	}
	seq := c.lists[k]
	if len(seq) == 0 {
		return &unstructured.UnstructuredList{}, nil
	}
	items := seq[0]
	if len(seq) > 1 {
		c.lists[k] = seq[1:]
	}
	return &unstructured.UnstructuredList{Items: items}, nil
}

// nextErr pops the next sequenced error of a request key; the caller holds
// the lock.
func (c *upgradeClient) nextErr(key string) error {
	seq := c.errSeq[key]
	if len(seq) == 0 {
		return nil
	}
	c.errSeq[key] = seq[1:]
	return seq[0]
}

func (c *upgradeClient) Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, "PUT "+objKey(gvr, ns, obj.GetName()))
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	c.updates = append(c.updates, upgradeUpdate{resource: gvr.Resource, ns: ns, dryRun: dryRun, obj: obj.DeepCopy()})
	if c.write != nil {
		return c.write.Update(ctx, gvr, ns, obj, dryRun)
	}
	out := obj.DeepCopy()
	out.SetGeneration(obj.GetGeneration() + 1)
	return out, nil
}

func (c *upgradeClient) requestsOf(prefix string) (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// upgradeFactory hands the local client for the local cluster and the
// downstream client for any other id, and refuses an empty id as the surface
// does.
type upgradeFactory struct {
	local      *upgradeClient
	downstream *upgradeClient
	ids        *[]string
}

func (f upgradeFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	*f.ids = append(*f.ids, id)
	switch id {
	case "":
		return nil, clierr.New(clierr.CodeValidation, "cluster id is empty")
	case engine.LocalCluster:
		return f.local, nil
	}
	return f.downstream, nil
}
func (f upgradeFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}
func (f upgradeFactory) ContextCluster() (string, error)                            { return "", nil }
func (f upgradeFactory) ContextProject() (string, error)                            { return "", nil }
func (f upgradeFactory) ResolveCluster(_ context.Context, s string) (string, error) { return s, nil }

// releaseServer serves the two release lists, every row with serverArgs {}.
func releaseServer(t *testing.T, rke2, k3s []string, store *steveStore) *httptest.Server {
	t.Helper()
	rows := func(vs []string) []map[string]any {
		out := make([]map[string]any, 0, len(vs))
		for _, v := range vs {
			out = append(out, map[string]any{"version": v, "serverArgs": map[string]any{}})
		}
		return out
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]any
		switch r.URL.Path {
		case rke2ReleasePath:
			data = rows(rke2)
		case k3sReleasePath:
			data = rows(k3s)
		default:
			if ns, ok := strings.CutPrefix(r.URL.Path, "/v1/rke.cattle.io.etcdsnapshots/"); ok {
				store.serveSnapshots(w, ns)
				return
			}
			if id, ok := strings.CutPrefix(r.URL.Path, "/v1/management.cattle.io.clusters/"); ok {
				if obj, found := store.get(id); found {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(obj.Object)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

var (
	nodeRow              = kindRow("node")
	etcdSnapshotRow      = kindRow("etcdsnapshot")
	managementClusterRow = kindRow("managementcluster")
)

// steveStore keeps the management Clusters the test server returns through
// Steve.
type steveStore struct {
	mu             sync.Mutex
	objs           map[string]*unstructured.Unstructured
	snapshots      map[string][]unstructured.Unstructured
	snapshotStatus map[string]int
	snapshotCalls  int
}

// setSnapshots stores the etcd snapshots Steve lists in namespace ns, each with
// the namespace and the id Steve gives it.
func (s *steveStore) setSnapshots(ns string, snaps []unstructured.Unstructured) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshots == nil {
		s.snapshots = map[string][]unstructured.Unstructured{}
	}
	out := make([]unstructured.Unstructured, 0, len(snaps))
	for _, snap := range snaps {
		obj := snap.DeepCopy()
		obj.SetNamespace(ns)
		obj.Object["id"] = ns + "/" + obj.GetName()
		obj.Object["type"] = "rke.cattle.io.etcdsnapshot"
		out = append(out, *obj)
	}
	s.snapshots[ns] = out
}

// failSnapshots makes the list of the etcd snapshots in namespace ns respond
// with the HTTP status.
func (s *steveStore) failSnapshots(ns string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshotStatus == nil {
		s.snapshotStatus = map[string]int{}
	}
	s.snapshotStatus[ns] = status
}

// serveSnapshots responds to the Steve list of the etcd snapshots in namespace ns.
func (s *steveStore) serveSnapshots(w http.ResponseWriter, ns string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotCalls++
	if status := s.snapshotStatus[ns]; status != 0 {
		w.WriteHeader(status)
		return
	}
	data := s.snapshots[ns]
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"revision": "1", "count": len(data), "data": data})
}

// snapshotRequests is the number of Steve lists of etcd snapshots the test
// server responded to.
func (s *steveStore) snapshotRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotCalls
}

func (s *steveStore) get(id string) (*unstructured.Unstructured, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	obj, ok := s.objs[id]
	if !ok {
		return nil, false
	}
	return obj.DeepCopy(), true
}

// set stores a management Cluster as Steve returns it: the object with an id
// and a type added.
func (s *steveStore) set(obj *unstructured.Unstructured) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objs == nil {
		s.objs = map[string]*unstructured.Unstructured{}
	}
	steveObj := obj.DeepCopy()
	steveObj.Object["id"] = obj.GetName()
	steveObj.Object["type"] = "management.cattle.io.cluster"
	s.objs[obj.GetName()] = steveObj
}

type upgradeFixture struct {
	steve             *steveStore
	local, downstream *upgradeClient
	factory           upgradeFactory
	ids               *[]string
	srv               *httptest.Server
}

var defaultRKE2 = []string{"v1.30.4+rke2r1", "v1.31.3+rke2r1", "v1.31.4+rke2r1", "v1.31.5+rke2r1", "v1.32.0+rke2r1"}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	local, downstream := newUpgradeClient(), newUpgradeClient()
	ids := &[]string{}
	local.put(settingsGVR, "", rke2DefaultVersionSetting, settingObj(rke2DefaultVersionSetting, "v1.31.5+rke2r1"))
	store := &steveStore{}
	return &upgradeFixture{
		steve: store,
		local: local, downstream: downstream, ids: ids,
		factory: upgradeFactory{local: local, downstream: downstream, ids: ids},
		srv:     releaseServer(t, defaultRKE2, []string{"v1.31.4+k3s1", "v1.31.5+k3s1"}, store),
	}
}

func (f *upgradeFixture) command() runtime.MutatingCommand {
	return NewUpgrade(f.factory, fakeRootFactory{rc: classifiedRoot{srv: f.srv}}).(runtime.MutatingCommand)
}

// classifiedRoot is a RootClient over a test server that classifies a non-2xx
// response by its status, as the CLI's root client does: a 403 is RBAC.
type classifiedRoot struct{ srv *httptest.Server }

func (c classifiedRoot) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, "GET "+path+": "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, clierr.FromResponse("GET "+path, http.MethodGet, resp.StatusCode, body, nil)
}

func (f *upgradeFixture) plan(t *testing.T, in mcInput) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	return f.command().Plan(t.Context(), in)
}

func upgradeInput(cluster, version string, extra ...string) mcInput {
	in := mcInput{a: []string{cluster}, s: map[string]string{"k8s-version": version}, b: map[string]bool{}, is: map[string]bool{}}
	for i := 0; i+1 < len(extra); i += 2 {
		switch extra[i] {
		case "wait":
			in.b["wait"] = extra[i+1] == "true"
			in.is["wait"] = true
		default:
			in.s[extra[i]] = extra[i+1]
			in.is[extra[i]] = true
		}
	}
	return in
}

func provisionedObject(version string, rke map[string]any) *unstructured.Unstructured {
	if rke == nil {
		rke = map[string]any{}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "prod", "namespace": "fleet-default", "resourceVersion": "7", "generation": int64(3)},
		"spec":     map[string]any{"kubernetesVersion": version, "rkeConfig": rke},
		"status": map[string]any{
			"clusterName": "c-m-prod1", "observedGeneration": int64(3),
			"conditions": []any{map[string]any{"type": "Updated", "status": "True", "message": ""}},
		},
	}}
}

func importedProvisioning() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "edge", "namespace": "fleet-default", "resourceVersion": "5", "generation": int64(1)},
		"spec":     map[string]any{},
		"status":   map[string]any{"clusterName": "c-m-edge1"},
	}}
}

func importedManagement(annotation *string, running, configVersion string, upgraded string) *unstructured.Unstructured {
	annotations := map[string]any{}
	if annotation != nil {
		annotations[versionManagementAnnotation] = *annotation
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "management.cattle.io/v3", "kind": "Cluster",
		"metadata": map[string]any{"name": "c-m-edge1", "resourceVersion": "9", "generation": int64(2), "annotations": annotations},
		"spec": map[string]any{"rke2Config": map[string]any{
			"kubernetesVersion":   configVersion,
			"rke2upgradeStrategy": map[string]any{"serverConcurrency": int64(1), "workerConcurrency": int64(2), "drainServerNodes": true},
		}},
		"status": map[string]any{
			"driver": "rke2", "version": map[string]any{"gitVersion": running},
			"conditions": []any{map[string]any{"type": "Upgraded", "status": upgraded, "message": "worker node [w1] being upgraded"}},
		},
	}}
}

func node(name, version string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"nodeInfo": map[string]any{"kubeletVersion": version}},
	}}
}

func snapshotObj(name, cluster, status, createdAt string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": name},
		"spec":         map[string]any{"clusterName": cluster},
		"snapshotFile": map[string]any{"status": status, "createdAt": createdAt},
	}}
}

func (f *upgradeFixture) provisioned(version string) {
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", provisionedObject(version, nil))
	f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{}}})
	f.downstream.putList(nodeRow.GVR, "", []unstructured.Unstructured{node("n1", "v1.31.4+rke2r1"), node("n2", "v1.31.4+rke2r1")})
	f.local.putList(etcdSnapshotRow.GVR, "fleet-default", []unstructured.Unstructured{
		snapshotObj("old", "prod", "successful", "2026-11-01T10:00:00Z"),
		snapshotObj("new", "prod", "successful", "2026-11-03T10:00:00Z"),
		snapshotObj("failed", "prod", "failed", "2026-11-05T10:00:00Z"),
		snapshotObj("other", "elsewhere", "successful", "2026-11-06T10:00:00Z"),
	})
}

func (f *upgradeFixture) imported(annotation *string, running, configVersion, upgraded string) {
	f.local.put(provisioningClusterGVR, "fleet-default", "edge", importedProvisioning())
	f.local.put(managementClusterRow.GVR, "", "c-m-edge1", importedManagement(annotation, running, configVersion, upgraded))
	f.steve.set(importedManagement(annotation, running, configVersion, upgraded))
	f.downstream.putList(nodeRow.GVR, "", []unstructured.Unstructured{node("n1", running)})
	f.local.putList(etcdSnapshotRow.GVR, "c-m-edge1", []unstructured.Unstructured{snapshotObj("reg-snap", "c-m-edge1", "successful", "2026-11-04T10:00:00Z")})
}

func asCode(t *testing.T, err error) *clierr.Error {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	return ce
}

// TestUpgradeFlagRefusalsMakeNoRequest asserts a missing version and
// --wait=false with --timeout are refused with no request.
func TestUpgradeFlagRefusalsMakeNoRequest(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]mcInput{
		"no version":             upgradeInput("prod", ""),
		"wait false and timeout": upgradeInput("prod", "v1.31.5+rke2r1", "wait", "false", "timeout", "5m"),
		"bad timeout":            upgradeInput("prod", "v1.31.5+rke2r1", "timeout", "soon"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.provisioned("v1.31.4+rke2r1")
			_, _, err := f.plan(t, in)
			assert.Equal(t, clierr.CodeValidation, asCode(t, err).Code)
			assert.Empty(t, f.local.requests)
			assert.Empty(t, *f.ids)
		})
	}
}

// TestUpgradeNoProvisioningCluster asserts the refusal of a cluster with no
// provisioning cluster in the namespace.
func TestUpgradeNoProvisioningCluster(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	_, _, err := f.plan(t, upgradeInput("ghost", "v1.31.5+rke2r1"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster ghost has no provisioning cluster in namespace fleet-default; an imported cluster whose management cluster names no Fleet workspace has none, and its version is set in spec.rke2Config or spec.k3sConfig of its management cluster", ce.Message)
	assert.NotEmpty(t, ce.Hint)
}

func warningsContain(t *testing.T, warnings []string, want string) {
	t.Helper()
	for _, w := range warnings {
		if strings.Contains(w, want) {
			return
		}
	}
	assert.Fail(t, "no warning contains the text", "want %q in %q", want, warnings)
}

// TestUpgradeProvisionedPreview asserts the preview of a provisioned cluster:
// one server dry-run, an update that differs from the object read in
// spec.kubernetesVersion only, an Effect that is destructive and not
// reversible, and the warnings that say how the upgrade runs and how to
// return to an earlier version.
func TestUpgradeProvisionedPreview(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	eff, exec, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.PreviewServerDryRun, eff.PreviewSource)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	assert.True(t, eff.ConflictDetection)
	assert.Equal(t, "7", eff.ResourceVersion)
	assert.Equal(t, "provisioning-cluster", eff.DisplayKind)
	assert.Equal(t, "prod", eff.DisplayName)

	require.Len(t, f.local.updates, 1)
	sent := f.local.updates[0]
	assert.True(t, sent.dryRun)
	assert.Equal(t, "fleet-default", sent.ns)
	want := provisionedObject("v1.31.4+rke2r1", nil)
	require.NoError(t, unstructured.SetNestedField(want.Object, "v1.31.5+rke2r1", "spec", "kubernetesVersion"))
	assert.Equal(t, want.Object, sent.obj.Object, "the update changes spec.kubernetesVersion only")

	warningsContain(t, eff.Warnings, "the server-side dry-run checks the request; it does not run the upgrade. Rancher upgrades etcd nodes one at a time, then control plane nodes (1 at a time), then worker nodes (1 at a time)")
	warningsContain(t, eff.Warnings, "drain before upgrade: control plane nodes off, worker nodes off")
	warningsContain(t, eff.Warnings, "the upgrade cannot be reversed with this command. To return to an earlier version, restore an etcd snapshot of type Kubernetes version and etcd: it sets the Kubernetes version recorded in that snapshot, which can differ from v1.31.4+rke2r1, and restores etcd to its state when the snapshot was taken")
	warningsContain(t, eff.Warnings, "Newest successful snapshot of cluster prod: new, taken 2026-11-03T10:00:00Z")
	warningsContain(t, eff.Warnings, "2 of 2 nodes run v1.31.4+rke2r1")
}

// TestUpgradeSnapshotLines asserts the snapshot line for each outcome of the
// snapshot list: none recorded, only failed ones, several successful ones, a
// Kubernetes 403 followed by the list through Steve (that succeeds, responds with 403
// or 404, or fails), another Kubernetes error that makes no Steve request, and
// running the command again while the upgrade is not finished.
func TestUpgradeSnapshotLines(t *testing.T) {
	t.Parallel()
	const none = "No successful etcd snapshot of cluster prod is recorded; take one first, in the Rancher UI"
	several := []unstructured.Unstructured{snapshotObj("a", "prod", "successful", "2026-11-01T10:00:00Z"), snapshotObj("b", "prod", "successful", "2026-11-02T10:00:00Z")}
	const newest = "Newest successful snapshot of cluster prod: b, taken 2026-11-02T10:00:00Z"
	for name, tc := range map[string]struct {
		snaps       []unstructured.Unstructured
		k8sErr      error
		steveSnaps  []unstructured.Unstructured
		steveStatus int
		want        string
		steveCalls  int
	}{
		"none":                             {snaps: nil, want: none},
		"failed only":                      {snaps: []unstructured.Unstructured{snapshotObj("f", "prod", "failed", "2026-11-05T10:00:00Z")}, want: none},
		"several":                          {snaps: several, want: newest},
		"forbidden then Steve":             {k8sErr: snapshotListResponse(http.StatusForbidden), steveSnaps: several, want: newest, steveCalls: 1},
		"forbidden then Steve none":        {k8sErr: snapshotListResponse(http.StatusForbidden), want: none, steveCalls: 1},
		"forbidden then Steve forbidden":   {k8sErr: snapshotListResponse(http.StatusForbidden), steveStatus: http.StatusForbidden, want: "cannot list the etcd snapshots of cluster prod (RBAC)", steveCalls: 1},
		"forbidden then Steve not found":   {k8sErr: snapshotListResponse(http.StatusForbidden), steveStatus: http.StatusNotFound, want: "cannot list the etcd snapshots of cluster prod (RBAC)", steveCalls: 1},
		"forbidden then Steve unavailable": {k8sErr: snapshotListResponse(http.StatusForbidden), steveStatus: http.StatusServiceUnavailable, want: "cannot list the etcd snapshots of cluster prod (GENERIC)", steveCalls: 1},
		"Kubernetes unavailable":           {k8sErr: snapshotListResponse(http.StatusServiceUnavailable), steveSnaps: several, want: "cannot list the etcd snapshots of cluster prod (GENERIC)"},
		"no status":                        {k8sErr: clierr.New(clierr.CodeRBAC, "forbidden"), steveSnaps: several, want: "cannot list the etcd snapshots of cluster prod (RBAC)"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.provisioned("v1.31.4+rke2r1")
			f.local.putList(etcdSnapshotRow.GVR, "fleet-default", tc.snaps)
			if tc.k8sErr != nil {
				f.local.errs["LIST "+objKey(etcdSnapshotRow.GVR, "fleet-default", "")] = tc.k8sErr
			}
			f.steve.setSnapshots("fleet-default", tc.steveSnaps)
			if tc.steveStatus != 0 {
				f.steve.failSnapshots("fleet-default", tc.steveStatus)
			}
			eff, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
			require.NoError(t, err)
			warningsContain(t, eff.Warnings, tc.want)
			assert.Equal(t, tc.steveCalls, f.steve.snapshotRequests(), "the Steve lists of etcd snapshots")
		})
	}
}

// snapshotListResponse is an etcd snapshot List error as the Kubernetes client
// returns it for an HTTP response with the given status.
func snapshotListResponse(status int) error {
	return apierrors.NewGenericServerResponse(status, "list", schema.GroupResource{Resource: "etcdsnapshots"}, "", "the API server responded", 0, false)
}

// TestUpgradeImportedPreview asserts the preview of an imported cluster:
// the management Cluster is updated in the version of its config and, with no
// annotation, in the annotation; the imported cluster's etcd snapshot objects are read in
// the namespace named by the cluster's id.
func TestUpgradeImportedPreview(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.imported(nil, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	f.local.put(settingsGVR, "", versionManagementSetting, settingObj(versionManagementSetting, "true"))
	eff, _, err := f.plan(t, upgradeInput("edge", "v1.31.5+rke2r1"))
	require.NoError(t, err)
	assert.Equal(t, "c-m-edge1", eff.DisplayName, "the display name is the written object's name")
	assert.Equal(t, "cluster", eff.DisplayKind)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	require.Len(t, f.local.updates, 1)
	sent := f.local.updates[0]
	assert.Equal(t, "clusters", sent.resource)
	assert.Empty(t, sent.ns)
	want := importedManagement(nil, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	require.NoError(t, unstructured.SetNestedField(want.Object, "v1.31.5+rke2r1", "spec", "rke2Config", "kubernetesVersion"))
	require.NoError(t, unstructured.SetNestedField(want.Object, "system-default", "metadata", "annotations", versionManagementAnnotation))
	assert.Equal(t, jsonShaped(t, want.Object), jsonShaped(t, sent.obj.Object), "the update changes the version and adds the annotation only")
	warningsContain(t, eff.Warnings, "Rancher's system-upgrade-controller upgrades the control plane nodes (1 at a time), then the worker nodes (2 at a time)")
	warningsContain(t, eff.Warnings, "drain before upgrade: control plane nodes on (force true), worker nodes off")
	warningsContain(t, eff.Warnings, "On Rancher v2.16 and later, to return to an earlier version, restore an etcd snapshot")
	warningsContain(t, eff.Warnings, "earlier servers cannot restore the version of an imported cluster")
	warningsContain(t, eff.Warnings, "Newest successful snapshot of cluster edge: reg-snap, taken 2026-11-04T10:00:00Z")
	warningsContain(t, eff.Warnings, "has no annotation rancher.io/imported-cluster-version-management; Rancher's admission webhook denies an update without it, and the command sets it to system-default")
	assert.Equal(t, 1, f.local.requestsOf("GET settings//"+versionManagementSetting), "the Setting is read when the annotation is absent")
}

// TestUpgradeImportedKeepsAnAnnotation asserts a cluster that has the
// annotation keeps it, and the Setting is not read.
func TestUpgradeImportedKeepsAnAnnotation(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	yes := "true"
	f.imported(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
	_, _, err := f.plan(t, upgradeInput("edge", "v1.31.5+rke2r1"))
	require.NoError(t, err)
	sent := f.local.updates[0].obj
	annotation, _, _ := unstructured.NestedString(sent.Object, "metadata", "annotations", versionManagementAnnotation)
	assert.Equal(t, "true", annotation)
	assert.Zero(t, f.local.requestsOf("GET settings//"+versionManagementSetting), "no Setting read for annotation true")
}

// TestUpgradeLocalIsRefusedBeforeAnyRead asserts the cluster Rancher runs in
// is refused after the provisioning Cluster read and before any other read.
func TestUpgradeLocalIsRefusedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	prov := importedProvisioning()
	require.NoError(t, unstructured.SetNestedField(prov.Object, "local", "status", "clusterName"))
	f.local.put(provisioningClusterGVR, "fleet-local", "local", prov)
	_, _, err := f.plan(t, upgradeInput("local", "v1.31.5+rke2r1", "namespace", "fleet-local"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "this CLI does not upgrade the local cluster", ce.Message)
	assert.Equal(t, 1, len(f.local.requests), "one request: the provisioning cluster")
}

// TestUpgradeRunningOperations asserts each of the four operations in progress
// refuses with a retryable GENERIC and no hint, a running upgrade alone does
// not refuse, and a control plane that cannot be read warns and goes on.
func TestUpgradeRunningOperations(t *testing.T) {
	t.Parallel()
	requests := map[string]map[string]any{
		"etcdSnapshotCreate":   {"generation": int64(1)},
		"etcdSnapshotRestore":  {"name": "s1", "generation": int64(1)},
		"rotateCertificates":   {"generation": int64(1)},
		"rotateEncryptionKeys": {"generation": int64(1)},
	}
	for field, request := range requests {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.provisioned("v1.31.4+rke2r1")
			f.local.put(provisioningClusterGVR, "fleet-default", "prod", provisionedObject("v1.31.4+rke2r1", map[string]any{field: request}))
			_, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
			ce := asCode(t, err)
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.True(t, ce.Retryable)
			assert.Empty(t, ce.Hint)
			assert.Contains(t, ce.Message, "in progress")
			assert.Contains(t, ce.Message, "Rancher runs one operation at a time; try again after it ends")
			assert.Empty(t, f.local.updates, "no dry-run")
		})
	}

	t.Run("an upgrade alone", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.provisioned("v1.31.4+rke2r1")
		f.local.put(rkeControlPlaneGVR, "fleet-default", "prod", &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"appliedSpec": map[string]any{"kubernetesVersion": "v1.30.4+rke2r1"}}}})
		eff, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
		require.NoError(t, err)
		assert.Len(t, f.local.updates, 1)
		assert.NotEmpty(t, eff.Warnings)
	})

	for name, readErr := range map[string]error{
		"control plane not found": clierr.New(clierr.CodeNotFound, "missing"),
		"control plane forbidden": clierr.New(clierr.CodeRBAC, "forbidden"),
		"control plane failed":    clierr.New(clierr.CodeGeneric, "boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFixture(t)
			f.provisioned("v1.31.4+rke2r1")
			f.local.errs["GET "+objKey(rkeControlPlaneGVR, "fleet-default", "prod")] = readErr
			eff, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
			require.NoError(t, err)
			warningsContain(t, eff.Warnings, "cannot read the RKEControlPlane of cluster prod ("+clierr.From(readErr).Code+"); an etcd snapshot restore in progress would replace the version this command writes")
			assert.Len(t, f.local.updates, 1)
		})
	}
}

// TestUpgradeFinishedAndAgain asserts the target equal to the current version
// is refused when the upgrade is finished, and goes on, with the object as
// read, when it is not.
func TestUpgradeFinishedAndAgain(t *testing.T) {
	t.Parallel()
	t.Run("provisioned finished", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.provisioned("v1.31.4+rke2r1")
		_, _, err := f.plan(t, upgradeInput("prod", "v1.31.4+rke2r1"))
		ce := asCode(t, err)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Equal(t, "cluster prod is already upgraded to v1.31.4+rke2r1: Rancher reports the upgrade done and every listed node runs it", ce.Message)
		assert.Empty(t, f.local.updates)
	})
	t.Run("provisioned finished, nodes not checked", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.provisioned("v1.31.4+rke2r1")
		f.downstream.errs["LIST "+objKey(nodeRow.GVR, "", "")] = clierr.New(clierr.CodeRBAC, "forbidden")
		_, _, err := f.plan(t, upgradeInput("prod", "v1.31.4+rke2r1"))
		assert.Equal(t, "cluster prod is already upgraded to v1.31.4+rke2r1: Rancher reports the upgrade done (its Updated condition is True); the nodes were not checked", asCode(t, err).Message)
	})
	t.Run("provisioned not finished", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		f.provisioned("v1.31.5+rke2r1")
		f.downstream.putList(nodeRow.GVR, "", []unstructured.Unstructured{node("n1", "v1.31.4+rke2r1"), node("n2", "v1.31.5+rke2r1")})
		eff, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
		require.NoError(t, err)
		require.Len(t, f.local.updates, 1)
		assert.Equal(t, provisionedObject("v1.31.5+rke2r1", nil).Object, f.local.updates[0].obj.Object, "the object as read")
		warningsContain(t, eff.Warnings, "cluster prod already has version v1.31.5+rke2r1 set and its upgrade is not finished: nothing changes; the command waits for the upgrade")
		warningsContain(t, eff.Warnings, "it may have been taken after the upgrade was requested")
	})
	t.Run("imported finished", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		yes := "true"
		f.imported(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True")
		_, _, err := f.plan(t, upgradeInput("edge", "v1.31.4+rke2r1"))
		assert.Equal(t, "cluster edge is already upgraded to v1.31.4+rke2r1: Rancher reports the upgrade done", asCode(t, err).Message)
	})
	t.Run("imported not finished", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFixture(t)
		yes := "true"
		f.imported(&yes, "v1.31.4+rke2r1", "v1.31.5+rke2r1", "Unknown")
		eff, _, err := f.plan(t, upgradeInput("edge", "v1.31.5+rke2r1"))
		require.NoError(t, err)
		assert.Equal(t, jsonShaped(t, importedManagement(&yes, "v1.31.4+rke2r1", "v1.31.5+rke2r1", "Unknown").Object), jsonShaped(t, f.local.updates[0].obj.Object))
		warningsContain(t, eff.Warnings, "already has version v1.31.5+rke2r1 set and its upgrade is not finished")
		warningsContain(t, eff.Warnings, "an upgrade is in progress: worker node [w1] being upgraded; the new version replaces the version it upgrades to")
	})
}

// TestUpgradeNodeListFailureIsAWarning asserts a refused Node List does not
// refuse the command: it warns that the preview does not show how many nodes
// run the current version, and for a provisioned cluster that the wait does not
// check the nodes. An empty cluster id
// fails the same way.
func TestUpgradeNodeListFailureIsAWarning(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	f.downstream.errs["LIST "+objKey(nodeRow.GVR, "", "")] = clierr.New(clierr.CodeRBAC, "forbidden")
	eff, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
	require.NoError(t, err)
	warningsContain(t, eff.Warnings, "cannot list the nodes of cluster prod (RBAC); the preview does not show how many nodes run the current version and the wait does not check the nodes")

	f = newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	obj := provisionedObject("v1.31.4+rke2r1", nil)
	require.NoError(t, unstructured.SetNestedField(obj.Object, "", "status", "clusterName"))
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", obj)
	eff, _, err = f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
	require.NoError(t, err)
	warningsContain(t, eff.Warnings, "cannot list the nodes of cluster prod (VALIDATION)")
}

// TestUpgradeVersionRulesAreCheckedBeforeTheNodes asserts a refused target
// ends the Plan before the Node and snapshot Lists.
func TestUpgradeVersionRulesAreCheckedBeforeTheNodes(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	_, _, err := f.plan(t, upgradeInput("prod", "v1.30.4+rke2r1"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "is lower than the current version v1.31.4+rke2r1")
	assert.Empty(t, f.downstream.requests)
	assert.Zero(t, f.local.requestsOf("LIST "))
	assert.Empty(t, f.local.updates)
}

// TestUpgradeReleaseListNotFound asserts a server with no release route for
// the distribution gives CAPABILITY_UNSUPPORTED with the feature named.
func TestUpgradeReleaseListNotFound(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	f.provisioned("v1.31.4+rke2r1")
	f.srv = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(f.srv.Close)
	_, _, err := f.plan(t, upgradeInput("prod", "v1.31.5+rke2r1"))
	ce := asCode(t, err)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, "/v1-rke2-release/release was not found: the server does not list RKE2 releases; this CLI cannot check the version", ce.Message)
	details, ok := ce.Details.(clierr.CapabilityUnsupportedDetails)
	require.True(t, ok)
	assert.Equal(t, "cluster upgrade", details.Feature)
}

// TestUpgradeWarnings asserts the warnings that depend on the target: a skipped
// minor, an older patch, a version newer than the default, a concurrency of 0,
// a running upgrade and a short timeout.
func TestUpgradeWarnings(t *testing.T) {
	t.Parallel()
	f := newUpgradeFixture(t)
	obj := provisionedObject("v1.30.4+rke2r1", map[string]any{"upgradeStrategy": map[string]any{
		"controlPlaneConcurrency": "0", "workerConcurrency": "25%",
		"workerDrainOptions": map[string]any{"enabled": true, "force": true, "timeout": int64(120)},
	}})
	require.NoError(t, unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"type": "Updated", "status": "Unknown", "message": "waiting on the plan"}}, "status", "conditions"))
	f.provisioned("v1.30.4+rke2r1")
	f.local.put(provisioningClusterGVR, "fleet-default", "prod", obj)
	eff, _, err := f.plan(t, upgradeInput("prod", "v1.32.0+rke2r1", "timeout", "5m"))
	require.NoError(t, err)
	warningsContain(t, eff.Warnings, "v1.32.0+rke2r1 skips Kubernetes 1.31; Kubernetes supports upgrading one minor at a time")
	warningsContain(t, eff.Warnings, "v1.32.0+rke2r1 is newer than the default version v1.31.5+rke2r1 (setting rke2-default-version); the Rancher UI labels it experimental")
	warningsContain(t, eff.Warnings, "control plane nodes (0 at a time), then worker nodes (25% at a time)")
	warningsContain(t, eff.Warnings, "every control plane node is upgraded at once")
	warningsContain(t, eff.Warnings, "drain before upgrade: control plane nodes off, worker nodes on (force true, timeout 120 seconds)")
	warningsContain(t, eff.Warnings, "an upgrade is in progress: waiting on the plan; the new version replaces the version it upgrades to")
	warningsContain(t, eff.Warnings, "an upgrade usually takes longer; the upgrade goes on after the wait ends")

	f = newUpgradeFixture(t)
	f.provisioned("v1.30.4+rke2r1")
	eff, _, err = f.plan(t, upgradeInput("prod", "v1.31.3+rke2r1"))
	require.NoError(t, err)
	warningsContain(t, eff.Warnings, "v1.31.3+rke2r1 is not the newest patch of 1.31; the Rancher UI lists it as deprecated")
}

// jsonShaped returns obj as JSON decoding makes it. An object read from the
// test server then compares with one built in the test.
func jsonShaped(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// recordedWrite is one write a test server received.
type recordedWrite struct {
	method, path string
	query        url.Values
}

// TestUpgradeWritesCarryTheFieldManager asserts the preview and the write of
// each kind of cluster reach a server as PUTs to the cluster's own path, each
// with fieldManager=cli.rancher.io, and the preview with dryRun=All. The update
// goes through the real cluster client to a server that fails the test on a
// write without the field manager.
func TestUpgradeWritesCarryTheFieldManager(t *testing.T) {
	t.Parallel()
	yes := "true"
	for name, tc := range map[string]struct {
		prepare func(f *upgradeFixture)
		cluster string
		path    string
	}{
		"provisioned": {func(f *upgradeFixture) { f.provisioned("v1.31.4+rke2r1") }, "prod",
			"/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/prod"},
		"imported": {func(f *upgradeFixture) { f.imported(&yes, "v1.31.4+rke2r1", "v1.31.4+rke2r1", "True") }, "edge",
			"/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-m-edge1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var (
				mu     sync.Mutex
				writes []recordedWrite
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					if r.URL.Query().Get("fieldManager") != "cli.rancher.io" {
						t.Errorf("%s %s was sent without fieldManager=cli.rancher.io", r.Method, r.URL.RequestURI())
					}
					mu.Lock()
					writes = append(writes, recordedWrite{method: r.Method, path: r.URL.Path, query: r.URL.Query()})
					mu.Unlock()
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(srv.Close)
			rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, Token: "id:secret"}, engine.LocalCluster)
			require.NoError(t, err)
			rc.Transport = srv.Client().Transport
			client, err := provider.NewClusterClient(rc, engine.LocalCluster)
			require.NoError(t, err)

			f := newUpgradeFixture(t)
			tc.prepare(f)
			f.local.write = client
			_, exec, err := f.plan(t, upgradeInput(tc.cluster, "v1.31.5+rke2r1", "wait", "false"))
			require.NoError(t, err)
			_, err = exec(t.Context())
			require.NoError(t, err)

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, writes, 2, "the preview and the write")
			for _, w := range writes {
				assert.Equal(t, http.MethodPut, w.method)
				assert.Equal(t, tc.path, w.path)
				assert.Equal(t, "cli.rancher.io", w.query.Get("fieldManager"))
			}
			assert.Equal(t, "All", writes[0].query.Get("dryRun"), "the preview is a server dry-run")
			assert.False(t, writes[1].query.Has("dryRun"), "the write is not")
		})
	}
}

// TestDrainText asserts the drain options of a provisioned cluster read with
// their unit: a timeout and a grace period in seconds, a negative grace period
// as the pod's default and a timeout of 0 as the 600 seconds Rancher uses.
func TestDrainText(t *testing.T) {
	t.Parallel()
	rows := map[string]struct {
		options map[string]any
		want    string
	}{
		"off":                   {map[string]any{"enabled": false, "force": true}, "off"},
		"on with no option":     {map[string]any{"enabled": true}, "on"},
		"timeout":               {map[string]any{"enabled": true, "force": true, "timeout": int64(120)}, "on (force true, timeout 120 seconds)"},
		"negative grace period": {map[string]any{"enabled": true, "force": true, "gracePeriod": int64(-1), "timeout": int64(120)}, "on (force true, gracePeriod -1 (the pod's default), timeout 120 seconds)"},
		"grace period":          {map[string]any{"enabled": true, "gracePeriod": float64(30)}, "on (gracePeriod 30 seconds)"},
		"grace period of 0":     {map[string]any{"enabled": true, "gracePeriod": int64(0)}, "on (gracePeriod 0 seconds)"},
		"timeout of 0":          {map[string]any{"enabled": true, "force": true, "timeout": int64(0)}, "on (force true, timeout 0 (Rancher uses 600 seconds))"},
	}
	for name, tc := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, drainText(tc.options))
		})
	}
}
