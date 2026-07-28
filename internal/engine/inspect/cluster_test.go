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
	"github.com/rancher/rancher-cli/internal/engine/kinds"
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
		crtbListPath = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/clusterroletemplatebindings"
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
		case crtbListPath:
			// Return an empty crtb list so members lookup succeeds silently
			// and does not add a second warning to the timeout-degrade case.
			_, _ = w.Write([]byte(`{"apiVersion":"management.cattle.io/v3","kind":"ClusterRoleTemplateBindingList","items":[]}`))
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

func TestClusterMembersTriState(t *testing.T) {
	t.Parallel()
	// nil pointer → field omitted entirely (key absent).
	b, err := json.Marshal(clusterResult{Cluster: clusterIdentity{ID: "c"}})
	require.NoError(t, err)
	var nilDoc map[string]any
	require.NoError(t, json.Unmarshal(b, &nilDoc))
	_, hasMembers := nilDoc["members"]
	assert.False(t, hasMembers, "nil members pointer: key must be absent from the wire")

	// non-nil empty → key present, empty array.
	empty := []clusterMember{}
	b, err = json.Marshal(clusterResult{Cluster: clusterIdentity{ID: "c"}, Members: &empty})
	require.NoError(t, err)
	var emptyDoc map[string]any
	require.NoError(t, json.Unmarshal(b, &emptyDoc))
	raw, hasMembers := emptyDoc["members"]
	require.True(t, hasMembers, "non-nil empty members: key must be present")
	slice, ok := raw.([]any)
	require.True(t, ok, "members value must be an array")
	assert.Empty(t, slice, "non-nil empty members: array must be empty")
}

func TestRenderClusterTextEscapeStripping(t *testing.T) {
	t.Parallel()
	members := []clusterMember{
		{Principal: "okta_user://evil", DisplayName: "Evil\x1b[2Jname", Role: "cluster-member"},
	}
	r := clusterResult{Cluster: clusterIdentity{ID: "c-x"}, Members: &members}
	text := renderClusterText(r)
	assert.NotContains(t, text, "\x1b", "ANSI escape sequences from IdP display names must be stripped")
	assert.Contains(t, text, "Evil[2Jname", "printable chars around the escape are preserved")

	bidi := []clusterMember{
		{Principal: "okta_user://evil", DisplayName: "abc\u202edef\u200bghi", Role: "cluster-member"},
	}
	r.Members = &bidi
	text = renderClusterText(r)
	assert.NotContains(t, text, "\u202e", "bidi-override format characters must be stripped")
	assert.NotContains(t, text, "\u200b", "zero-width format characters must be stripped")
	assert.Contains(t, text, "abcdefghi", "visible runes around format characters are preserved")
}

func TestRenderClusterTextMembersSection(t *testing.T) {
	t.Parallel()
	r := clusterResult{Cluster: clusterIdentity{ID: "c-x"}}
	assert.Contains(t, renderClusterText(r), "MEMBERS\n  members: unavailable\n")

	empty := []clusterMember{}
	r.Members = &empty
	assert.Contains(t, renderClusterText(r), "MEMBERS\n  none\n")

	members := []clusterMember{
		{Principal: "github_org://8341", DisplayName: "DevOps Team", Role: "cluster-member"},
		{Principal: "okta_user://never", Role: "cluster-member"},
	}
	r.Members = &members
	text := renderClusterText(r)
	assert.Contains(t, text, "MEMBERS\n  DevOps Team (github_org://8341)  cluster-member\n  okta_user://never  cluster-member\n")
}

// TestClusterMembersLookupTriState pins the LOOKUP → tri-state mapping —
// not just the serialization of hand-built pointers: failed LIST → nil +
// warning; successful empty LIST → non-nil empty; canceled → error.
func TestClusterMembersLookupTriState(t *testing.T) {
	t.Parallel()
	crtbKind, _ := kinds.Lookup("clusterroletemplatebinding")

	failed := newFakeClient().withListErr(crtbKind.GVR, errors.New("forbidden"))
	members, warns, err := clusterMembers(context.Background(), failed, "c-x")
	require.NoError(t, err)
	assert.Nil(t, members, "failed LIST → nil pointer (JSON field omitted)")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "clusterroletemplatebindings unavailable")

	empty := newFakeClient().withListNS(crtbKind.GVR, "c-x", &unstructured.UnstructuredList{})
	members, warns, err = clusterMembers(context.Background(), empty, "c-x")
	require.NoError(t, err)
	require.NotNil(t, members, "verified empty LIST → non-nil pointer")
	assert.Empty(t, *members)
	assert.Empty(t, warns)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := newFakeClient().withListErr(crtbKind.GVR, context.Canceled)
	_, _, err = clusterMembers(ctx, canceled, "c-x")
	require.Error(t, err, "caller cancellation aborts the command")
}

// TestClusterMachinesLookupTriState pins the LOOKUP → tri-state mapping —
// not just the serialization of hand-built pointers: failed LIST → nil +
// warning; successful empty LIST → non-nil empty; canceled → error.
// Mirrors TestClusterMembersLookupTriState.
func TestClusterMachinesLookupTriState(t *testing.T) {
	t.Parallel()
	machineKind, _ := kinds.Lookup("capimachine")

	failed := newFakeClient().withListErr(machineKind.GVR, errors.New("forbidden"))
	machines, warns, err := clusterMachines(context.Background(), failed, "my-cluster")
	require.NoError(t, err)
	assert.Nil(t, machines, "failed LIST → nil pointer (JSON field omitted)")
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "machines unavailable")

	empty := newFakeClient().withListNS(machineKind.GVR, machineKind.DefaultNamespace, &unstructured.UnstructuredList{})
	machines, warns, err = clusterMachines(context.Background(), empty, "my-cluster")
	require.NoError(t, err)
	require.NotNil(t, machines, "verified empty LIST → non-nil pointer")
	assert.Empty(t, *machines)
	assert.Empty(t, warns)
	require.Len(t, empty.listSelectors, 1)
	assert.Equal(t, capiClusterNameLabel+"=my-cluster", empty.listSelectors[0],
		"machines LIST must use the cluster-name label selector")
	assert.Equal(t, machineKind.DefaultNamespace, empty.listNamespaces[0],
		"machines LIST must be scoped to machineKind.DefaultNamespace, never a hardcoded string")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := newFakeClient().withListErr(machineKind.GVR, context.Canceled)
	_, _, err = clusterMachines(ctx, canceled, "my-cluster")
	require.Error(t, err, "caller cancellation aborts the command")
}

// TestClusterMachinesWithCreateError is the live-exercise scenario: a CAPI
// Machine carrying a CreateError condition surfaces the message in both text
// and JSON — the DO 422 that was otherwise invisible to every rancher command.
func TestClusterMachinesWithCreateError(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")
	machineKind, _ := kinds.Lookup("capimachine")

	prov := provClusterFixture("my-cluster", "c-abc")
	machine := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "my-cluster-pool1-abc", "namespace": "fleet-default"},
		"status": map[string]any{
			"phase": "Failed",
			"conditions": []any{
				map[string]any{
					"type": "Ready", "status": "False", "reason": "CreateError",
					"message": "POST https://api.digitalocean.com/v2/droplets: 422 invalid image",
				},
			},
		},
	}}
	machineList := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*machine}}

	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "v1.30.4+rke2r1")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}}).
		withListNS(machineKind.GVR, "fleet-default", machineList)
	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)
	assert.Empty(t, res.Warnings)

	doc, _ := marshaledDoc(t, res.Data)
	machinesRaw, ok := doc["machines"].([]any)
	require.True(t, ok, "machines key must be present and an array")
	require.Len(t, machinesRaw, 1)
	m, ok := machinesRaw[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "my-cluster-pool1-abc", m["name"])
	assert.Equal(t, "Failed", m["phase"])
	assert.Equal(t, "POST https://api.digitalocean.com/v2/droplets: 422 invalid image", m["error"],
		"CreateError condition message must surface in the JSON envelope")

	assert.Contains(t, res.Text, "my-cluster-pool1-abc  Failed")
	assert.Contains(t, res.Text, "POST https://api.digitalocean.com/v2/droplets: 422 invalid image",
		"CreateError message must appear in the text card")
}

// TestRenderClusterTextMachinesSection pins the MACHINES section text
// rendering: unavailable marker, none, and machines with error messages.
// Mirrors TestRenderClusterTextMembersSection.
func TestRenderClusterTextMachinesSection(t *testing.T) {
	t.Parallel()
	r := clusterResult{Cluster: clusterIdentity{ID: "c-x"}}
	assert.Contains(t, renderClusterText(r), "MACHINES\n  machines: unavailable\n")

	empty := []clusterMachine{}
	r.Machines = &empty
	assert.Contains(t, renderClusterText(r), "MACHINES\n  none\n")

	machines := []clusterMachine{
		{Name: "pool1-abc", Phase: "Running"},
		{Name: "pool1-def", Phase: "Failed", Error: "CreateError: invalid image"},
	}
	r.Machines = &machines
	text := renderClusterText(r)
	assert.Contains(t, text,
		"MACHINES\n  pool1-abc  Running\n  pool1-def  Failed\n    CreateError: invalid image\n")
}

// TestReadyConditionOverridesStatusReady pins the live-exercise fix: when
// the Ready condition is present, its status drives the ready field rather
// than status.ready, which was observed flipping true ~90s before the
// condition left Unknown.
func TestReadyConditionOverridesStatusReady(t *testing.T) {
	t.Parallel()
	mgmtKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioning-cluster")

	prov := provClusterFixture("my-cluster", "c-abc")
	// status.ready has already flipped true but the Ready condition is False.
	_ = unstructured.SetNestedField(prov.Object, true, "status", "ready")
	_ = unstructured.SetNestedSlice(prov.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "Waiting", "message": "waiting for nodes"},
	}, "status", "conditions")

	client := newFakeClient().
		withObject(mgmtKind.GVR, "", "c-abc", mgmtClusterFixture("c-abc", "prod", "active", "")).
		withListNS(provKind.GVR, "fleet-default", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*prov}})
	cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: &fakeRoot{body: crtBodyFixture(t, "c-abc", "tok-abc", nil)}})
	res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
	require.NoError(t, err)

	doc, _ := marshaledDoc(t, res.Data)
	status, ok := doc["status"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, status["ready"],
		"Ready condition (False) must override status.ready (true)")
	assert.Contains(t, res.Text, "ready: false")
}

// TestMachineConditionMessageSanitized pins the printable sanitizer on
// machine error messages: escape sequences in provider-sourced condition
// text must be stripped before they reach the text card.
func TestMachineConditionMessageSanitized(t *testing.T) {
	t.Parallel()
	machines := []clusterMachine{
		{Name: "pool1-abc", Phase: "Failed", Error: "Error\x1b[2J from provider"},
	}
	r := clusterResult{Cluster: clusterIdentity{ID: "c-x"}, Machines: &machines}
	text := renderClusterText(r)
	assert.NotContains(t, text, "\x1b", "ANSI escape sequences in machine error messages must be stripped")
	assert.Contains(t, text, "Error[2J from provider", "printable chars around the escape are preserved")
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

func TestAgentFrom(t *testing.T) {
	t.Parallel()

	obj := mgmtClusterFixture("c-1", "one", "active", "")
	assert.Nil(t, agentFrom(obj), "no conditions → no agent section")

	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Provisioned", "status": "True"},
	}, "status", "conditions")
	assert.Nil(t, agentFrom(obj), "unrelated conditions → no agent section")

	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Connected", "status": "True"},
		map[string]any{"type": "Ready", "status": "True"},
	}, "status", "conditions")
	ag := agentFrom(obj)
	require.NotNil(t, ag)
	require.NotNil(t, ag.Connected)
	require.NotNil(t, ag.Ready)
	assert.True(t, *ag.Connected)
	assert.True(t, *ag.Ready)
	assert.Empty(t, ag.Message)

	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "Disconnected", "message": "not ready"},
		map[string]any{"type": "Connected", "status": "False", "reason": "Disconnected", "message": "Cluster agent is not connected"},
	}, "status", "conditions")
	ag = agentFrom(obj)
	require.NotNil(t, ag)
	assert.False(t, *ag.Connected)
	assert.False(t, *ag.Ready)
	assert.Equal(t, "Cluster agent is not connected", ag.Message, "Connected's message wins over Ready's")

	_ = unstructured.SetNestedSlice(obj.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "message": "waiting"},
	}, "status", "conditions")
	ag = agentFrom(obj)
	require.NotNil(t, ag)
	assert.Nil(t, ag.Connected, "absent Connected condition stays nil, never fabricated")
	require.NotNil(t, ag.Ready)
	assert.False(t, *ag.Ready)
	assert.Equal(t, "waiting", ag.Message)
}

func TestInspectClusterAgentConnectivity(t *testing.T) {
	t.Parallel()

	withMgmtConditions := func(t *testing.T, conds []any) (*fakeClient, *fakeRoot) {
		t.Helper()
		client, root := inspectClusterFixture(t)
		mgmtKind := mustKind(t, "cluster")
		mgmt := mgmtClusterFixture("c-abc", "prod", "active", "v1.30.4+rke2r1")
		_ = unstructured.SetNestedSlice(mgmt.Object, conds, "status", "conditions")
		client.withObject(mgmtKind.GVR, "", "c-abc", mgmt)
		return client, root
	}

	t.Run("disconnected agent surfaces in envelope, text, and warnings", func(t *testing.T) {
		t.Parallel()
		client, root := withMgmtConditions(t, []any{
			map[string]any{"type": "Connected", "status": "False", "reason": "Disconnected", "message": "Cluster agent is not connected"},
			map[string]any{"type": "Ready", "status": "False", "reason": "Disconnected", "message": "Cluster agent is not connected"},
		})
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)

		doc, _ := marshaledDoc(t, res.Data)
		agent, ok := doc["agent"].(map[string]any)
		require.True(t, ok, "agent section present when conditions exist")
		assert.Equal(t, false, agent["connected"])
		assert.Equal(t, false, agent["ready"])
		assert.Equal(t, "Cluster agent is not connected", agent["message"])

		assert.Contains(t, res.Text, "agent: disconnected — Cluster agent is not connected")
		require.NotEmpty(t, res.Warnings)
		assert.Equal(t, "cluster agent is not connected: Cluster agent is not connected", res.Warnings[0])
	})

	t.Run("connected agent renders healthy with no warning", func(t *testing.T) {
		t.Parallel()
		client, root := withMgmtConditions(t, []any{
			map[string]any{"type": "Connected", "status": "True"},
			map[string]any{"type": "Ready", "status": "True"},
		})
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)

		doc, _ := marshaledDoc(t, res.Data)
		agent, ok := doc["agent"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, true, agent["connected"])
		assert.Equal(t, true, agent["ready"])
		_, hasMsg := agent["message"]
		assert.False(t, hasMsg, "no message when healthy")

		assert.Contains(t, res.Text, "agent: connected\n")
		for _, w := range res.Warnings {
			assert.NotContains(t, w, "agent")
		}
	})

	t.Run("no conditions omits the section entirely", func(t *testing.T) {
		t.Parallel()
		client, root := inspectClusterFixture(t)
		cmd := NewInspectCluster(&fakeFactory{client: client}, fakeRoots{rc: root})
		res, err := execRead(t, cmd, input{args: []string{"c-abc"}})
		require.NoError(t, err)

		doc, _ := marshaledDoc(t, res.Data)
		_, ok := doc["agent"]
		assert.False(t, ok, "agent key absent, never null/{}")
		assert.NotContains(t, res.Text, "agent:")
	})
}
