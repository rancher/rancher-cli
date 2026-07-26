package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeRoot is a canned engine.RootClient: one body or one error, and the
// requested path recorded for assertion.
type fakeRoot struct {
	body []byte
	err  error
	path string
}

func (f *fakeRoot) GetJSON(_ context.Context, path string) ([]byte, error) {
	f.path = path
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

var _ engine.RootClient = (*fakeRoot)(nil)

type fakeRoots struct {
	rc  engine.RootClient
	err error
}

func (f fakeRoots) Root() (engine.RootClient, error) { return f.rc, f.err }

var _ engine.RootClientFactory = (fakeRoots{})

func mgmtClusterFixture(id, display, state, gitVersion string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{},
	}}
	if state != "" {
		_ = unstructured.SetNestedField(obj.Object, state, "status", "state")
	}
	if gitVersion != "" {
		_ = unstructured.SetNestedField(obj.Object, gitVersion, "status", "version", "gitVersion")
	}
	return obj
}

// provClusterFixture builds a provisioning.cattle.io/v1 Cluster whose
// status.clusterName carries the correlation key. metadata.name and
// spec.displayName are DELIBERATELY different values so any test passing
// through here re-proves correlation is by status.clusterName alone.
func provClusterFixture(name, clusterName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": "fleet-default"},
		"spec": map[string]any{
			"displayName": name + "-renamed",
			"rkeConfig":   map[string]any{},
		},
		"status": map[string]any{"clusterName": clusterName, "ready": false},
	}}
}

func crtBodyFixture(t *testing.T, ns, token string, overrides map[string]any) []byte {
	t.Helper()
	status := map[string]any{
		"command":                    "curl -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"insecureCommand":            "curl --insecure -sfL https://rancher.example/v3/import/" + token + ".yaml | kubectl apply -f -",
		"nodeCommand":                "sudo docker run -d --privileged --net=host --name=cattle-agent rancher/rancher-agent --server https://rancher.example --token " + token,
		"insecureNodeCommand":        "sudo docker run -d --name=cattle-agent rancher/rancher-agent --token " + token,
		"windowsNodeCommand":         "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"insecureWindowsNodeCommand": "docker run --isolation=hyperv --name=cattle-agent rancher/rancher-agent --token " + token,
		"manifestUrl":                "https://rancher.example/v3/import/" + token + ".yaml",
		"token":                      token,
		"expiresAt":                  "2099-01-01T00:00:00Z",
	}
	for k, v := range overrides {
		status[k] = v
	}
	b, err := json.Marshal(map[string]any{
		"id":       ns + "/default-token",
		"metadata": map[string]any{"namespace": ns, "name": "default-token"},
		"spec":     map[string]any{"clusterName": ns},
		"status":   status,
	})
	require.NoError(t, err)
	return b
}

// marshaledDoc pins the MARSHALED envelope shape (IC-5): the wire JSON,
// not the in-memory struct.
func marshaledDoc(t *testing.T, data any) (map[string]any, string) {
	t.Helper()
	b, err := json.Marshal(data)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc, string(b)
}

func inspectClusterFixture(t *testing.T) (*fakeClient, *fakeRoot) {
	t.Helper()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	prov := provClusterFixture("my-cluster", "c-abc")
	_ = unstructured.SetNestedField(prov.Object, "v1.30.5+rke2r1", "spec", "kubernetesVersion")
	_ = unstructured.SetNestedField(prov.Object, true, "status", "ready")
	_ = unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "Provisioned", "status": "False", "reason": "Waiting", "message": "waiting for nodes"},
	}, "status", "conditions")
	_ = unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{
			"name":             "pool1",
			"quantity":         int64(3),
			"etcdRole":         true,
			"controlPlaneRole": true,
			"workerRole":       true,
			"machineConfigRef": map[string]any{"kind": "Amazonec2Config", "name": "mycfg"},
		},
	}, "spec", "rkeConfig", "machinePools")

	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "v1.30.4+rke2r1")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
	root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}
	return client, root
}

func TestInspectClusterHappyPathEnvelope(t *testing.T) {
	t.Parallel()
	client, root := inspectClusterFixture(t)
	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	assert.Equal(t, "ClusterInspection", res.Kind)
	assert.Empty(t, res.Warnings)
	assert.Equal(t, "/v1/management.cattle.io.clusterregistrationtokens/c-abc/default-token", root.path,
		"the default-token fetch targets the MANAGEMENT CLUSTER ID namespace, not fleet-default")

	doc, _ := marshaledDoc(t, res.Data)

	cluster, ok := doc["cluster"].(map[string]any)
	require.True(t, ok, "cluster section is always present")
	assert.Equal(t, "c-abc", cluster["id"])
	assert.Equal(t, "prod", cluster["name"])
	assert.Equal(t, "active", cluster["state"])
	assert.Equal(t, "v1.30.4+rke2r1", cluster["kubernetesVersion"],
		"the management-OBSERVED version wins over the provisioning spec (IC-4b)")

	_, hasNote := doc["provisioningNote"]
	assert.False(t, hasNote, "the note is RESERVED for confirmed absence")

	status, ok := doc["status"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, status["ready"])
	conds, ok := status["conditions"].([]any)
	require.True(t, ok)
	require.Len(t, conds, 2)
	first, ok := conds[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Ready", first["type"])
	assert.Equal(t, "True", first["status"])

	pools, ok := doc["machinePools"].([]any)
	require.True(t, ok)
	require.Len(t, pools, 1)
	pool, ok := pools[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "pool1", pool["name"])
	assert.Equal(t, "Amazonec2Config", pool["machineConfigKind"])
	assert.Equal(t, "mycfg", pool["machineConfigName"])
	assert.Equal(t, float64(3), pool["quantity"])
	assert.Equal(t, []any{"controlplane", "etcd", "worker"}, pool["roles"], "roles are a sorted string slice")

	reg, ok := doc["registration"].(map[string]any)
	require.True(t, ok)
	for _, field := range []string{"nodeCommand", "insecureNodeCommand", "command", "insecureCommand", "manifestUrl"} {
		v, ok := reg[field].(string)
		require.True(t, ok, "registration.%s must be present", field)
		assert.Contains(t, v, "tok-abc", "registration.%s carries the real spliced token", field)
	}
}

// TestInspectClusterCorrelatesRenamedClusterByStatusClusterName is IC-14's
// correlation pin: a provisioning CR whose metadata.name AND
// spec.displayName differ from every management identity still correlates
// via status.clusterName, and a non-matching sibling CR is ignored.
func TestInspectClusterCorrelatesRenamedClusterByStatusClusterName(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	match := provClusterFixture("totally-different-name", "c-abc")
	other := provClusterFixture("other-cluster", "c-zzz")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*other, *match}})

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	doc, _ := marshaledDoc(t, res.Data)
	_, hasStatus := doc["status"]
	assert.True(t, hasStatus, "the renamed CR must still correlate")
	_, hasNote := doc["provisioningNote"]
	assert.False(t, hasNote)
	mpRaw, ok := doc["machinePools"]
	require.True(t, ok, "the matched CR's empty rkeConfig marshals an EMPTY pools array (IC-9)")
	mpSlice, ok := mpRaw.([]any)
	require.True(t, ok, "machinePools is a JSON array")
	assert.Empty(t, mpSlice, "the matched CR's empty rkeConfig marshals an EMPTY pools array (IC-9)")
	statusDoc, ok := doc["status"].(map[string]any)
	require.True(t, ok)
	condsRaw, ok := statusDoc["conditions"]
	require.True(t, ok, "conditions key is always present (IC-5)")
	condsSlice, ok := condsRaw.([]any)
	require.True(t, ok, "conditions is a JSON array")
	assert.Empty(t, condsSlice, "conditions are ALWAYS a non-nil slice (IC-5)")
}

func TestInspectClusterVersionPrecedence(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	run := func(t *testing.T, mgmtVersion, provVersion string) string {
		t.Helper()
		prov := provClusterFixture("my-cluster", "c-abc")
		if provVersion != "" {
			_ = unstructured.SetNestedField(prov.Object, provVersion, "spec", "kubernetesVersion")
		}
		client := newFakeClient().
			withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "provisioning", mgmtVersion)).
			withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)
		doc, _ := marshaledDoc(t, res.Data)
		cluster, ok := doc["cluster"].(map[string]any)
		require.True(t, ok)
		v, ok := cluster["kubernetesVersion"].(string)
		require.True(t, ok, "kubernetesVersion is always present on the cluster section, even when empty")
		return v
	}

	t.Run("management observed wins", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "v1.30.4+rke2r1", run(t, "v1.30.4+rke2r1", "v1.30.5+rke2r1"))
	})
	t.Run("pending cluster falls back to the provisioning spec", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "v1.30.5+rke2r1", run(t, "", "v1.30.5+rke2r1"))
	})
	t.Run("neither observed nor desired stays empty, never fabricated", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "", run(t, "", ""))
	})
}

// TestInspectClusterProvisioningAbsent is IC-7: the LIST succeeded and
// matched zero objects — confirmed absence. provisioningNote at the ROOT,
// status/machinePools keys absent from the wire, NOT an error, NO warning
// for it (the note is the signal).
func TestInspectClusterProvisioningAbsent(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "v1.30.4+rke2r1")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{})

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)
	assert.Empty(t, res.Warnings, "confirmed absence is the note's job, never a warning")

	doc, _ := marshaledDoc(t, res.Data)
	assert.Equal(t, "no provisioning.cattle.io Cluster for this cluster in fleet-default", doc["provisioningNote"])
	_, hasStatus := doc["status"]
	assert.False(t, hasStatus, "status must be ABSENT from the wire, not null/{}")
	_, hasPools := doc["machinePools"]
	assert.False(t, hasPools, "machinePools must be ABSENT from the wire")
	_, hasReg := doc["registration"]
	assert.True(t, hasReg, "the registration read is independent of the provisioning CR")
}

// TestInspectClusterProvisioningUnavailable is IC-7a's unavailable branch:
// the lookup failed (RBAC/discovery/transport), so an agent must NOT read
// "definitely none" — no note, a warning naming the cause, NOT an error.
func TestInspectClusterProvisioningUnavailable(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListErr(provKind.GVR, errors.New("clusters.provisioning.cattle.io is forbidden"))

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err, "a failed provisioning lookup degrades, never errors")

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "provisioning cluster lookup unavailable")
	assert.Contains(t, res.Warnings[0], "forbidden", "the warning names the cause")

	doc, _ := marshaledDoc(t, res.Data)
	_, hasNote := doc["provisioningNote"]
	assert.False(t, hasNote, "unavailable is NOT the absence note — the distinction is the contract (IC-7a)")
	_, hasStatus := doc["status"]
	assert.False(t, hasStatus)
	_, hasPools := doc["machinePools"]
	assert.False(t, hasPools)
}

// TestInspectClusterProvisioningAmbiguous is IC-7a's ambiguous branch:
// multiple provisioning CRs reporting the same status.clusterName is a
// server-side anomaly — treated as unavailable, never as a pick-one.
func TestInspectClusterProvisioningAmbiguous(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	a := provClusterFixture("prov-a", "c-abc")
	b := provClusterFixture("prov-b", "c-abc")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*a, *b}})

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "ambiguous")
	assert.Contains(t, res.Warnings[0], "c-abc")

	doc, _ := marshaledDoc(t, res.Data)
	_, hasNote := doc["provisioningNote"]
	assert.False(t, hasNote)
	_, hasStatus := doc["status"]
	assert.False(t, hasStatus)
	_, hasPools := doc["machinePools"]
	assert.False(t, hasPools)
}

// TestInspectClusterRegistrationUnavailable is IC-8: any registration
// read failure — the fetch itself or Root() — omits the section and warns.
func TestInspectClusterRegistrationUnavailable(t *testing.T) {
	t.Parallel()

	assertDegraded := func(t *testing.T, roots fakeRoots) {
		t.Helper()
		mgmtKind := mustKind(t, "cluster")
		provKind := mustKind(t, "provisioning-cluster")
		client := newFakeClient().
			withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
			withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
		cmd := NewInspectCluster(&fakeFactory{client: client}, roots)
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err, "a registration failure degrades, never errors")
		require.Len(t, res.Warnings, 1)
		assert.Contains(t, res.Warnings[0], "registration token unavailable")
		doc, _ := marshaledDoc(t, res.Data)
		_, hasReg := doc["registration"]
		assert.False(t, hasReg, "registration must be ABSENT from the wire on a failed read")
		_, hasStatus := doc["status"]
		assert.True(t, hasStatus, "the provisioning section is independent of the registration read")
	}

	t.Run("fetch failure", func(t *testing.T) {
		t.Parallel()
		assertDegraded(t, fakeRoots{rc: &fakeRoot{err: clierr.New(clierr.CodeNotFound, "default-token not found")}})
	})
	t.Run("root factory failure", func(t *testing.T) {
		t.Parallel()
		assertDegraded(t, fakeRoots{err: errors.New("no server configured")})
	})
}

// TestInspectClusterContextCancellationPropagates pins the degrade
// boundary: only ordinary availability failures degrade the optional
// sections. A canceled or deadline-expired context during either optional
// lookup IS the command's error — execution stops (no further requests),
// nothing degrades to a warning, and the exit is non-zero via the
// standard toClierr path.
func TestInspectClusterContextCancellationPropagates(t *testing.T) {
	t.Parallel()
	ctxErrs := []error{context.Canceled, context.DeadlineExceeded}

	t.Run("provisioning list", func(t *testing.T) {
		t.Parallel()
		for _, ctxErr := range ctxErrs {
			mgmtKind := mustKind(t, "cluster")
			provKind := mustKind(t, "provisioning-cluster")
			client := newFakeClient().
				withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
				withListErr(provKind.GVR, fmt.Errorf("list provisioning clusters: %w", ctxErr))
			root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}

			cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
			res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
			require.Error(t, err, "%v during the provisioning LIST must propagate, not degrade", ctxErr)
			assert.ErrorIs(t, err, ctxErr)
			assert.Equal(t, "", root.path, "no registration fetch after cancellation")
			assert.Empty(t, res.Warnings, "cancellation is the command's error, never a section warning")
		}
	})

	t.Run("registration fetch", func(t *testing.T) {
		t.Parallel()
		for _, ctxErr := range ctxErrs {
			mgmtKind := mustKind(t, "cluster")
			provKind := mustKind(t, "provisioning-cluster")
			client := newFakeClient().
				withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
				withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
			root := &fakeRoot{err: fmt.Errorf("fetch default-token: %w", ctxErr)}

			cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
			res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
			require.Error(t, err, "%v during the registration fetch must propagate, not degrade", ctxErr)
			assert.ErrorIs(t, err, ctxErr)
			assert.Empty(t, res.Warnings, "cancellation is the command's error, never a section warning")
		}
	})
}

// TestInspectClusterCancellationThroughRealProviderClient re-proves the
// cancellation boundary through the PRODUCTION adapter. The provider's List
// passes every failure through mapErr, which rebuilds a *clierr.Error from
// err.Error() — the context sentinel does not survive the chain, so an
// errors.Is check alone would degrade a canceled LIST to a warning and keep
// issuing requests. The fake-based test above wraps the sentinel directly
// and cannot catch that regression; this one drives Execute with a real
// provider client against an httptest server whose LIST handler cancels the
// request context mid-flight.
func TestInspectClusterCancellationThroughRealProviderClient(t *testing.T) {
	t.Parallel()
	const (
		mgmtGetPath  = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-abc"
		provListPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case mgmtGetPath:
			_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"},"status":{"state":"active"}}`))
		case provListPath:
			// Abort the caller mid-LIST; the client tears the request down,
			// which unblocks the handler via the server-side request context.
			cancel()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: string(certPEM), Token: "id:secret", Cluster: engine.LocalCluster})
	require.NoError(t, err)
	realClient, err := provider.NewClusterClient(rc)
	require.NoError(t, err)

	root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}
	cmd := NewInspectCluster(&fakeFactory{client: realClient}, fakeRoots{rc: root})
	rcmd, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok, "command must be a ReadCommand")
	res, err := rcmd.Execute(ctx, input{args: []string{"c-abc"}}, nil)
	require.Error(t, err, "a canceled provisioning LIST through the real provider client must propagate, not degrade")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "the provider's structured error propagates")
	assert.Empty(t, res.Warnings, "cancellation is the command's error, never a section warning")
	assert.Equal(t, "", root.path, "no registration fetch after cancellation")
}

// TestInspectClusterProviderTimeoutDegradesWithActiveCallerContext pins the
// OTHER side of the cancellation boundary through the production adapter: the
// provider's own per-request child deadline (opCtx) expiring while the
// CALLER's context is still active is an ordinary availability failure, not a
// cancellation — the command succeeds, the provisioning sections degrade to a
// warning that carries the deadline wording (so agents can tell timeout from
// RBAC), and the registration lookup still proceeds. Not run with
// t.Parallel(): it shrinks the provider's shared request timeout via
// SetRequestTimeoutForTest, and this package's parallel tests must not
// observe a shrunk timeout mid-run.
func TestInspectClusterProviderTimeoutDegradesWithActiveCallerContext(t *testing.T) {
	const (
		mgmtGetPath  = "/k8s/clusters/local/apis/management.cattle.io/v3/clusters/c-abc"
		provListPath = "/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters"
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case mgmtGetPath:
			_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"Cluster","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"},"status":{"state":"active"}}`))
		case provListPath:
			// Outlast the provider's (shrunk) child deadline; the client
			// aborts the request at the deadline, which unblocks the handler
			// via the server-side request context.
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	restore := provider.SetRequestTimeoutForTest(200 * time.Millisecond)
	defer restore()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	rc, err := kube.RESTConfig(config.Effective{URL: srv.URL, CA: string(certPEM), Token: "id:secret", Cluster: engine.LocalCluster})
	require.NoError(t, err)
	realClient, err := provider.NewClusterClient(rc)
	require.NoError(t, err)

	root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}
	cmd := NewInspectCluster(&fakeFactory{client: realClient}, fakeRoots{rc: root})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err, "an adapter-owned timeout with an active caller context degrades, never errors")

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "provisioning cluster lookup unavailable")
	assert.Contains(t, strings.ToLower(res.Warnings[0]), "deadline",
		"the warning carries the adapter's timeout wording so an agent can tell timeout from RBAC")

	doc, _ := marshaledDoc(t, res.Data)
	_, hasStatus := doc["status"]
	assert.False(t, hasStatus, "status stays omitted on the degraded lookup")
	_, hasPools := doc["machinePools"]
	assert.False(t, hasPools, "machinePools stays omitted on the degraded lookup")
	reg, hasReg := doc["registration"]
	assert.True(t, hasReg, "the registration lookup still proceeds after the degraded provisioning lookup")
	assert.NotEmpty(t, reg)
	assert.Equal(t, "/v1/management.cattle.io.clusterregistrationtokens/c-abc/default-token", root.path,
		"the registration fetch was issued despite the provisioning timeout")
}

// TestInspectClusterRegistrationPlaceholderFieldDropped is the IC-8
// placeholder policy over the IC-4a reporter: the affected field
// DISAPPEARS from the wire (individually-omitempty), the others survive,
// a warning names the field, and the literal placeholder never reaches
// output as data.
func TestInspectClusterRegistrationPlaceholderFieldDropped(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
	root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", map[string]any{"command": "{token}"})}

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "command")
	assert.Contains(t, res.Warnings[0], "not formatted by the server")

	doc, raw := marshaledDoc(t, res.Data)
	reg, ok := doc["registration"].(map[string]any)
	require.True(t, ok, "the section survives; only the affected field is dropped")
	_, hasCommand := reg["command"]
	assert.False(t, hasCommand, "the placeholder-bearing field must DISAPPEAR, not marshal \"\"")
	assert.Contains(t, reg["nodeCommand"], "tok-abc", "the healthy fields survive untouched")
	assert.NotContains(t, raw, "{token}", "the placeholder never reaches output as data")
}

// TestInspectClusterRegistrationNothingUsableOmitsSection pins the
// absent-is-never-{} contract for the registration section: when NO
// surfaced field survives — every field placeholder-dropped, or the
// server returned all five empty — the registration key is ABSENT from
// the wire, never "registration":{}. The per-field placeholder warnings
// are unchanged; an all-empty successful response adds no warning. Text
// mode renders the existing "registration: not available" marker.
func TestInspectClusterRegistrationNothingUsableOmitsSection(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, overrides map[string]any) (runtime.Result, map[string]any, string) {
		t.Helper()
		mgmtKind := mustKind(t, "cluster")
		provKind := mustKind(t, "provisioning-cluster")
		client := newFakeClient().
			withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
			withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
		root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", overrides)}
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)
		doc, raw := marshaledDoc(t, res.Data)
		return res, doc, raw
	}

	surfaced := []string{"nodeCommand", "insecureNodeCommand", "command", "insecureCommand", "manifestUrl"}

	t.Run("every surfaced field placeholder-dropped", func(t *testing.T) {
		t.Parallel()
		overrides := map[string]any{}
		for _, field := range surfaced {
			overrides[field] = "{token}"
		}
		res, doc, raw := run(t, overrides)

		require.Len(t, res.Warnings, len(surfaced), "the per-field placeholder warnings are unchanged")
		for _, field := range surfaced {
			assert.Contains(t, strings.Join(res.Warnings, "\n"), field)
		}
		_, hasReg := doc["registration"]
		assert.False(t, hasReg, "registration must be ABSENT from the wire, never {}")
		assert.NotContains(t, raw, "{token}", "the placeholder never reaches output as data")
		assert.Contains(t, res.Text, "registration: not available")
	})

	t.Run("successful response with all fields empty", func(t *testing.T) {
		t.Parallel()
		overrides := map[string]any{}
		for _, field := range surfaced {
			overrides[field] = ""
		}
		res, doc, _ := run(t, overrides)

		assert.Empty(t, res.Warnings, "server-empty is not a placeholder — no new warning")
		_, hasReg := doc["registration"]
		assert.False(t, hasReg, "registration must be ABSENT from the wire, never {}")
		assert.Contains(t, res.Text, "registration: not available")
	})
}

// TestInspectClusterRegistrationWindowsPlaceholderSuppressed pins the phantom-
// warning fix: windowsNodeCommand and insecureWindowsNodeCommand are Steve-
// guarded CRT fields that are NOT surfaced in inspect cluster's registration
// envelope. A CRT whose only placeholder-bearing fields are those Windows
// fields must produce NO warnings and a complete registration section with all
// five surfaced fields present.
func TestInspectClusterRegistrationWindowsPlaceholderSuppressed(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
	root := &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", map[string]any{
		"windowsNodeCommand":         "{token}",
		"insecureWindowsNodeCommand": "{token}",
	})}

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	assert.Empty(t, res.Warnings, "Windows-only placeholders must NOT produce warnings (they are not surfaced in the envelope)")

	doc, _ := marshaledDoc(t, res.Data)
	reg, ok := doc["registration"].(map[string]any)
	require.True(t, ok, "registration section must be present — the five surfaced fields are clean")
	for _, field := range []string{"nodeCommand", "insecureNodeCommand", "command", "insecureCommand", "manifestUrl"} {
		_, ok := reg[field]
		assert.True(t, ok, "registration.%s must be present", field)
	}
}

// TestInspectClusterResolutionErrors pins IC-2/IC-14's argument failures
// through the provider's REAL error vocabulary (clientFactory.ResolveCluster
// delegates to projects.MatchOneOrError — the same contract get cluster's
// named path uses): an unknown name-or-id is the structured NOT_FOUND
// ("cluster not found: ..."), an ambiguous display name is the structured
// AMBIGUOUS_SELECTION carrying candidates. Both fail BEFORE any read —
// zero management Gets, zero provisioning LISTs, zero CRT fetches.
func TestInspectClusterResolutionErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		resolveErr *clierr.Error
		wantCode   string
	}{
		{
			name: "unknown cluster propagates the provider's NOT_FOUND",
			resolveErr: clierr.New(clierr.CodeNotFound, "cluster not found: ghost").
				WithHint("pass a cluster id or display name"),
			wantCode: clierr.CodeNotFound,
		},
		{
			name: "ambiguous display name propagates the provider's AMBIGUOUS_SELECTION with candidates",
			resolveErr: clierr.New(clierr.CodeAmbiguousSelection, `2 clusters have display name "prod"`).
				WithCandidates([]clierr.Candidate{{ID: "c-aaa", Name: "prod"}, {ID: "c-bbb", Name: "prod"}}).
				WithHint("pass the cluster id"),
			wantCode: clierr.CodeAmbiguousSelection,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newFakeClient()
			root := &fakeRoot{}
			cmd := NewInspectCluster(&fakeFactory{client: client, resolveErr: tc.resolveErr}, fakeRoots{rc: root})
			_, err := execRead(t, cmd, input{args: []string{"prod"}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, tc.wantCode, ce.Code)
			assert.Empty(t, client.getCalls, "no management Get after a failed resolution")
			assert.Empty(t, client.listSelectors, "no provisioning LIST after a failed resolution")
			assert.Equal(t, "", root.path, "no CRT fetch after a failed resolution")
		})
	}
}

// TestInspectClusterMgmtGetErrorIsCommandError is IC-6: the management
// Cluster read is the only hard requirement — failure there IS the
// command's error, and no further read is attempted.
func TestInspectClusterMgmtGetErrorIsCommandError(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	client := newFakeClient().
		withGetErr(mgmtKind.GVR, "", "c-gone", clierr.New(clierr.CodeNotFound, "c-gone not found"))
	root := &fakeRoot{body: crtBodyFixture(t, "c-gone", "tok", nil)}

	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
	_, err := execRead(t, cmd, input{args: []string{"c-gone"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "", root.path, "no registration read after the hard failure")
}

func TestInspectClusterRejectsProject(t *testing.T) {
	t.Parallel()
	cmd := NewInspectCluster(&fakeFactory{client: newFakeClient()}, fakeRoots{rc: &fakeRoot{}})
	_, err := execRead(t, cmd, input{args: []string{"c-abc"}, s: map[string]string{"project": "p-abc"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--project does not apply to inspect cluster")
}

// TestInspectClusterTextSections pins the IC-1a multi-section text view
// section by section: order, content, the trouble-first condition
// ordering, sane message truncation, and the NEVER-truncated join
// commands.
func TestInspectClusterTextSections(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	longMessage := strings.Repeat("m", 200)
	prov := provClusterFixture("my-cluster", "c-abc")
	_ = unstructured.SetNestedField(prov.Object, true, "status", "ready")
	_ = unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{"type": "Ready", "status": "True"},
		map[string]any{"type": "Provisioned", "status": "False", "reason": "Waiting", "message": longMessage},
	}, "status", "conditions")
	_ = unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{
			"name":             "pool1",
			"quantity":         int64(3),
			"etcdRole":         true,
			"controlPlaneRole": true,
			"workerRole":       true,
			"machineConfigRef": map[string]any{"kind": "Amazonec2Config", "name": "mycfg"},
		},
	}, "spec", "rkeConfig", "machinePools")

	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "v1.30.4+rke2r1")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
	longToken := "tok-" + strings.Repeat("x", 160)
	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", longToken, nil)}})

	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)
	text := res.Text
	require.NotEmpty(t, text)

	iCluster := strings.Index(text, "CLUSTER\n")
	iConds := strings.Index(text, "\nCONDITIONS\n")
	iPools := strings.Index(text, "\nMACHINE POOLS\n")
	iReg := strings.Index(text, "\nREGISTRATION\n")
	require.Equal(t, 0, iCluster, "CLUSTER is the first section")
	require.True(t, iConds > iCluster && iPools > iConds && iReg > iPools,
		"sections render in the IC-1a order: CLUSTER, CONDITIONS, MACHINE POOLS, REGISTRATION")

	assert.Contains(t, text, "id: c-abc")
	assert.Contains(t, text, "name: prod")
	assert.Contains(t, text, "kubernetesVersion: v1.30.4+rke2r1")
	assert.Contains(t, text, "state: active")

	troublePos := strings.Index(text, "Provisioned=False")
	readyPos := strings.Index(text, "Ready=True")
	require.True(t, troublePos > 0 && readyPos > 0)
	assert.Less(t, troublePos, readyPos,
		"False conditions with a reason or message list FIRST (the derived trouble indicator, text-only)")
	assert.Contains(t, text, strings.Repeat("m", 120)+"…", "condition messages truncate sanely")
	assert.NotContains(t, text, longMessage, "the full 200-rune message never renders in text")

	assert.Contains(t, text, "pool1: Amazonec2Config/mycfg quantity=3 roles=controlplane,etcd,worker")

	assert.Contains(t, text,
		"sudo docker run -d --privileged --net=host --name=cattle-agent rancher/rancher-agent --server https://rancher.example --token "+longToken,
		"join commands render FULL-WIDTH and are never truncated — they are meant to be copy-pasted")
	assert.Contains(t, text, "https://rancher.example/v3/import/"+longToken+".yaml")
}

// TestInspectClusterTextAvailabilityMarkers pins the CONCISE in-place
// markers for omitted sections (IC-1a): the marker on stdout, the
// detailed cause ONLY in Warnings.
func TestInspectClusterTextAvailabilityMarkers(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	t.Run("confirmed absence renders the note text in place", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().
			withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
			withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{})
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{err: clierr.New(clierr.CodeNotFound, "no token")}})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)
		assert.Contains(t, res.Text, "no provisioning.cattle.io Cluster for this cluster in fleet-default")
		assert.Contains(t, res.Text, "registration: not available")
		assert.NotContains(t, res.Text, "no token", "the detailed cause lives ONLY in Warnings")
	})

	t.Run("unavailable renders the concise marker, never the cause", func(t *testing.T) {
		t.Parallel()
		client := newFakeClient().
			withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
			withListErr(provKind.GVR, errors.New("forbidden"))
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)
		assert.Contains(t, res.Text, "status: unavailable")
		assert.Contains(t, res.Text, "machine pools: unavailable")
		assert.NotContains(t, res.Text, "forbidden", "the detailed cause lives ONLY in Warnings")
	})
}

// TestInspectClusterStdoutStderrSplit drives the REAL pipeline
// (runtime.Run's Text branch) and pins stdout and stderr separately
// (IC-1a): sections + markers on stdout, "# warning: ..." detail on
// stderr, no duplication of the same text across the two streams.
func TestInspectClusterStdoutStderrSplit(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*provClusterFixture("my-cluster", "c-abc")}})
	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{err: clierr.New(clierr.CodeNotFound, "default-token vanished")}})

	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(context.Background(), cmd, input{args: []string{"c-abc"}},
		runtime.IO{Out: &out, Err: &errBuf, Format: ""}, safety.Binding{})
	require.Nil(t, ce)

	stdout, stderr := out.String(), errBuf.String()
	assert.Contains(t, stdout, "CLUSTER\n")
	assert.Contains(t, stdout, "registration: not available")
	assert.NotContains(t, stdout, "# warning:", "warnings never print to stdout")
	assert.NotContains(t, stdout, "default-token vanished", "the cause never prints to stdout")
	assert.Contains(t, stderr, "# warning: registration token unavailable")
	assert.Contains(t, stderr, "default-token vanished")
}
