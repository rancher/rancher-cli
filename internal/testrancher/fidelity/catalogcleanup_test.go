package fidelity

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catalogRun is what one run of a catalog write probe against the fake
// did.
type catalogRun struct {
	results  []ObservationResult
	err      error
	logs     []string
	captured map[string]string
}

// runCatalog runs p against tgt with a log function and a last teardown
// step that copies the captures, then asserts that no error, log line or
// stored cleanup value holds the canary, and that the fake saw no refused
// Secret read and no DELETE outside the rules.
func runCatalog(t *testing.T, f *catalogFake, tgt Target, p Probe) catalogRun {
	t.Helper()
	log := &recordingLog{}
	run := catalogRun{}
	p.Teardown = append(slices.Clone(p.Teardown), Step{Name: "copy the captures", Do: func(_ context.Context, _ Target, captured map[string]string) error {
		run.captured = maps.Clone(captured)
		return nil
	}})
	run.results, run.err = runProbe(withLog(t.Context(), log.log), tgt, p, 5*time.Second)
	run.logs = log.all()
	assertNoCanary(t, run)
	assert.Empty(t, f.problems(), "the fake refused no Secret read and saw no DELETE outside the rules")
	return run
}

// assertNoCanary asserts the run's error, log lines and stored cleanup
// values hold no canary; the cluster values the install sends are request
// input and read from fields that hold no canary.
func assertNoCanary(t *testing.T, run catalogRun) {
	t.Helper()
	if run.err != nil {
		assert.NotContains(t, run.err.Error(), catalogCanary, "the error holds no body, label or annotation value")
	}
	for _, line := range run.logs {
		assert.NotContains(t, line, catalogCanary, "a log line holds no body, label or annotation value")
	}
	for k, v := range run.captured {
		if strings.HasPrefix(k, catalogPrefix) && !strings.HasPrefix(k, catalogValuePrefix) {
			assert.NotContains(t, v, catalogCanary, "stored value %s holds no body, label or annotation value", k)
		}
	}
}

// requireNoWrite asserts the run sent no POST and no DELETE, and that the
// setup's first request, the read of the App, was sent.
func requireNoWrite(t *testing.T, f *catalogFake) {
	t.Helper()
	require.NotZero(t, f.count(http.MethodGet, "/apps/harvester-rbac"), "the setup's first request was sent")
	assert.Zero(t, f.count(http.MethodPost, "/"), "no POST")
	assert.Zero(t, f.count(http.MethodDelete, "/"), "no DELETE")
}

// cleanupSteps returns the setup and teardown steps of the cleanup of the
// local probe, with short bounds.
func cleanupSteps(cluster string) (catalogCleanup, []Step, []Step) {
	installOp, uninstallOp := "installop", "uninstallop"
	if cluster != "local" {
		cluster, installOp, uninstallOp = "{downstream}", "failop", ""
	}
	c := newCatalogCleanup(cluster, installOp, uninstallOp)
	fastCleanup(&c)
	setup, teardown := c.steps()
	return c, setup, teardown
}

func stepNamed(t *testing.T, steps []Step, name string) Step {
	t.Helper()
	for _, s := range steps {
		if s.Name == name {
			return s
		}
	}
	require.FailNow(t, "no step named "+name)
	return Step{}
}

// sentBody decodes the body of the first request of method whose path
// ends with suffix.
func sentBody(t *testing.T, f *catalogFake, method, suffix string) map[string]any {
	t.Helper()
	for _, r := range f.sent() {
		if r.method == method && strings.HasSuffix(r.path, suffix) {
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(r.body), &body))
			return body
		}
	}
	require.FailNow(t, "no "+method+" request to "+suffix)
	return nil
}

// TestCatalogInstallUninstallLocalRecordsAndCleansUp asserts the local probe
// against the fake: the six observations and their rules, the install body
// with the request id and the six cluster values, every object of both
// Operations deleted by name with its uid and logged, and the comparison
// passing.
func TestCatalogInstallUninstallLocalRecordsAndCleansUp(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
	require.NoError(t, run.err)

	require.Len(t, run.results, 6)
	statuses := []int{}
	for _, r := range run.results {
		statuses = append(statuses, r.Status)
	}
	assert.Equal(t, []int{201, 200, 200, 201, 200, 404}, statuses)
	install := run.results[0].Body
	assert.Equal(t, "<links>", install["links"])
	assert.Equal(t, "{installop}", install["operationName"])
	assert.Equal(t, "{uninstallop}", run.results[3].Body["operationName"])

	op := run.results[1].Body
	assert.Equal(t, map[string]any{"name": "{installop}", "namespace": "default", "generation": float64(1),
		"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "ClusterRole"}}}, op["metadata"])
	status := op["status"].(map[string]any)
	assert.ElementsMatch(t, []string{"action", "conditions", "namespace", "observedGeneration", "podCreated", "podName", "podNamespace", "releaseName"}, slices.Collect(maps.Keys(status)))
	assert.Equal(t, "{installop}", status["podName"])
	conditions := status["conditions"].([]any)
	require.Len(t, conditions, 2)
	assert.Equal(t, "Reconciling", conditions[0].(map[string]any)["type"], "the conditions are sorted by type")
	assert.Equal(t, "<ts>", conditions[0].(map[string]any)["lastUpdateTime"])

	app := run.results[2].Body
	assert.Equal(t, map[string]any{"name": "harvester-rbac", "namespace": "default",
		"ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Secret"}}}, app["metadata"])
	spec := app["spec"].(map[string]any)
	assert.Equal(t, "<value>", spec["values"])
	assert.Equal(t, map[string]any{"metadata": map[string]any{"name": "harvester-rbac", "version": "110.0.0+up0.1.1",
		"annotations": map[string]any{catalogRequestIDAnnotation: "{requestid}"}}}, spec["chart"])
	assert.Equal(t, map[string]any{"summary": map[string]any{"state": "deployed"}}, app["status"])

	body := sentBody(t, f, http.MethodPost, "/v1/catalog.cattle.io.clusterrepos/rancher-charts")
	charts := body["charts"].([]any)
	chart := charts[0].(map[string]any)
	assert.Equal(t, map[string]any{"global": map[string]any{"cattle": map[string]any{
		"clusterId": "local", "clusterName": "display-local", "systemProjectId": "p-aa", "url": "https://172.30.1.2",
		"rkePathPrefix": "/opt/rke", "rkeWindowsPathPrefix": "c:/rke",
	}}}, chart["values"])
	annotations := chart["annotations"].(map[string]any)
	assert.Regexp(t, `^[0-9a-f]{32}$`, annotations[catalogRequestIDAnnotation])
	assert.Equal(t, annotations[catalogRequestIDAnnotation], run.captured[requestIDCapture])
	assert.Equal(t, "default", body["namespace"])
	assert.Equal(t, "10m0s", body["timeout"])
	assert.Equal(t, true, body["wait"])

	for _, r := range f.sent() {
		if r.method == http.MethodPost {
			assert.Contains(t, r.query, "fieldManager=cli.rancher.io", "%s sends the CLI's field manager", r.path)
			assert.Equal(t, "application/json", r.accept, "%s sends the CLI's Accept", r.path)
		}
	}
	assert.Empty(t, f.ours(), "every object of both Operations is gone")
	assert.Equal(t, 1, f.count(http.MethodPost, "catalog.cattle.io.apps"), "the cleanup sent no second uninstall")
	assert.Contains(t, run.logs, "created: operations default/helm-operation-o2")
	assert.Contains(t, run.logs, "created: clusterroles pod-impersonation-helm-op-r2")
	assert.Contains(t, run.logs, "deleted: pods cattle-system/helm-operation-o2")
	assert.Contains(t, run.logs, "deleted: clusterroles pod-impersonation-helm-op-r2")
	assert.Contains(t, run.logs, "deleted: clusterrolebindings helm-op-pod-impersonation-helm-op-r2-b")
	for _, r := range f.sent() {
		if r.method == http.MethodDelete {
			var pre map[string]map[string]string
			require.NoError(t, json.Unmarshal([]byte(r.body), &pre))
			assert.NotEmpty(t, pre["preconditions"]["uid"], r.path)
		}
	}
}

// TestCatalogFailingInstallDownstreamLeavesNothing asserts the downstream
// probe end to end: the Stalled Operation and the App's 404 are recorded,
// and after the teardown no pod, Operation or Secret of the run remains.
func TestCatalogFailingInstallDownstreamLeavesNothing(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, testDownstream)
	run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
	require.NoError(t, run.err)
	require.Len(t, run.results, 3)
	assert.Equal(t, http.StatusCreated, run.results[0].Status)
	assert.Equal(t, "{failop}", run.results[0].Body["operationName"])
	stalled := ""
	for _, c := range run.results[1].Body["status"].(map[string]any)["conditions"].([]any) {
		if c.(map[string]any)["type"] == "Stalled" {
			stalled = c.(map[string]any)["status"].(string)
		}
	}
	assert.Equal(t, "True", stalled)
	assert.Equal(t, http.StatusNotFound, run.results[2].Status)
	assert.Empty(t, f.ours(), "no pod, Operation or Secret of the run remains")
	assert.Equal(t, 1, f.count(http.MethodPost, "/k8s/clusters/"+testDownstream+"/v1/catalog.cattle.io.clusterrepos/rancher-charts"))
	assert.Contains(t, run.logs, "deleted: operations default/helm-operation-o1")
}

// TestCatalogPrecheckRefusesLeftovers asserts that an App, an Operation in
// the release namespace or a virt- RoleTemplate on the cluster fails the
// setup before any write, and that the refusal names each leftover and the
// steps that remove it.
func TestCatalogPrecheckRefusesLeftovers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seed  func(f *catalogFake)
		wants []string
	}{
		{"an app with a request id", func(f *catalogFake) {
			f.add(&fakeObject{set: "apps", namespace: "default", name: "harvester-rbac",
				annotations: map[string]string{catalogRequestIDAnnotation: "0123456789abcdef0123456789abcdef"}})
		}, []string{"App default/harvester-rbac", "send the App's uninstall action once and wait until the App answers 404"}},
		{"an app without a request id", func(f *catalogFake) {
			f.add(&fakeObject{set: "apps", namespace: "default", name: "harvester-rbac"})
		}, []string{"App default/harvester-rbac", "Reconciling condition True"}},
		{"an operation in the release namespace", func(f *catalogFake) {
			f.add(&fakeObject{set: "operations", namespace: "default", name: "helm-operation-left"})
		}, []string{"Operation default/helm-operation-left", "with its uid as the delete precondition", forRoleLabel, "then the ClusterRole"}},
		{"a virt- roletemplate", func(f *catalogFake) {
			f.add(&fakeObject{set: "roletemplates", name: "virt-cluster-manage"})
		}, []string{"RoleTemplate virt-cluster-manage", "check that no App harvester-rbac exists on the cluster"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, "local")
			f.with(tc.seed)
			run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
			require.ErrorContains(t, run.err, "setup "+stepPrecheck)
			require.ErrorContains(t, run.err, "holds objects an earlier run left behind, and this run wrote nothing")
			for _, want := range tc.wants {
				require.ErrorContains(t, run.err, want)
			}
			requireNoWrite(t, f)
		})
	}
}

// TestCatalogIndexWait asserts the index wait: a listed chart ends it with
// the marker stored and later steps send nothing; a chart never listed
// fails the setup at the index deadline before any write.
func TestCatalogIndexWait(t *testing.T) {
	t.Parallel()
	t.Run("listed", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.NoError(t, run.err)
		assert.Equal(t, "true", run.captured[catalogIndexListed])
		assert.Equal(t, 1, f.count(http.MethodGet, "/v1/catalog.cattle.io.clusterrepos/rancher-charts"), "the later index steps send nothing")
	})
	t.Run("never listed", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.with(func(f *catalogFake) { f.indexListed = false })
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.ErrorContains(t, run.err, "does not list harvester-rbac 110.0.0+up0.1.1")
		require.ErrorContains(t, run.err, "(3 of 3)", "the step that finds the deadline passed fails")
		assert.Greater(t, f.count(http.MethodGet, "/v1/catalog.cattle.io.clusterrepos/rancher-charts"), 3)
		requireNoWrite(t, f)
	})
}

// TestCatalogClusterValuesReadFailureStopsBeforeAnyWrite asserts that a
// refused read of a cluster value fails the setup before any write.
func TestCatalogClusterValuesReadFailureStopsBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	f.answer = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/management.cattle.io.projects/local" {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": catalogCanary})
			return true
		}
		return false
	}
	run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
	require.ErrorContains(t, run.err, "setup "+stepValues)
	require.ErrorContains(t, run.err, "/v1/management.cattle.io.projects/local: status 403")
	requireNoWrite(t, f)
}

// TestCatalogWaitDeadlineIsStoredLast asserts the wait deadline is stored by
// the last setup step, after the before snapshot.
func TestCatalogWaitDeadlineIsStoredLast(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	_, setup, _ := cleanupSteps("local")
	require.Equal(t, stepDeadline, setup[len(setup)-1].Name)
	captured := map[string]string{}
	for _, s := range setup[:len(setup)-1] {
		require.NoError(t, s.Do(t.Context(), f.target(), captured), s.Name)
	}
	assert.Equal(t, "true", captured[catalogSnapshotTaken])
	assert.NotContains(t, captured, waitDeadlineCapture)
	before := time.Now()
	require.NoError(t, setup[len(setup)-1].Do(t.Context(), f.target(), captured))
	deadline, err := time.Parse(time.RFC3339Nano, captured[waitDeadlineCapture])
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(3*time.Second), deadline, time.Second)
}

// seedDecoys adds objects of another run before the probe runs: an
// Operation, pod and ClusterRole of another helm operation, a
// ClusterRoleBinding whose for-role label names that ClusterRole, and a
// ConfigMap with an operation prefix and no owner.
func seedDecoys(f *catalogFake) {
	role := f.add(&fakeObject{set: "clusterroles", name: "pod-impersonation-helm-op-decoy"})
	owner := []ownerRef{{kind: "ClusterRole", name: role.name, uid: role.uid}}
	f.add(&fakeObject{set: "operations", namespace: "cattle-system", name: "helm-operation-decoy", owners: owner})
	f.add(&fakeObject{set: "pods", namespace: "cattle-system", name: "helm-operation-decoy", owners: owner,
		annotations: map[string]string{podClusterRoleAnnotation: role.name}})
	f.add(&fakeObject{set: "clusterrolebindings", name: "helm-op-pod-impersonation-helm-op-decoy-b",
		labels: map[string]string{forRoleLabel: role.name}})
	f.add(&fakeObject{set: "configmaps", namespace: "cattle-system", name: "impersonation-helm-op-stray"})
}

// TestCatalogCleanupLeavesDecoys asserts no object of another run is
// deleted, whatever its name.
func TestCatalogCleanupLeavesDecoys(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	f.with(seedDecoys)
	run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
	require.NoError(t, run.err)
	for _, ref := range [][3]string{
		{"clusterroles", "", "pod-impersonation-helm-op-decoy"},
		{"pods", "cattle-system", "helm-operation-decoy"},
		{"clusterrolebindings", "", "helm-op-pod-impersonation-helm-op-decoy-b"},
		{"configmaps", "cattle-system", "impersonation-helm-op-stray"},
	} {
		assert.NotNil(t, f.lookup(ref[0], ref[1], ref[2]), "%v stays", ref)
	}
	assert.Empty(t, f.ours())
}

// TestCatalogUninstallStep asserts the teardown's uninstall: one uninstall
// for an App with this run's id when the probe's own uninstall did not run,
// and nothing sent in every case it cannot prove is this run's.
func TestCatalogUninstallStep(t *testing.T) {
	t.Parallel()
	t.Run("the probe's uninstall refused, the app with this request id is uninstalled", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		var refused atomic.Bool
		f.answer = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Query().Get("action") == "uninstall" && refused.CompareAndSwap(false, true) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": catalogCanary})
				return true
			}
			return false
		}
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.ErrorContains(t, run.err, "observation uninstall")
		assert.Equal(t, 2, f.count(http.MethodPost, "catalog.cattle.io.apps"), "one uninstall from the probe, one from the cleanup")
		assert.NotEmpty(t, run.captured[catalogCleanupOp])
		assert.Contains(t, run.logs, "created: operations default/"+run.captured[catalogCleanupOp])
		assert.Nil(t, f.lookup("apps", "default", "harvester-rbac"))
		assert.Empty(t, f.ours())
	})
	t.Run("no request id sends nothing", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		_, _, teardown := cleanupSteps("local")
		require.NoError(t, stepNamed(t, teardown, stepUninstall).Do(t.Context(), f.target(), map[string]string{"installop": "helm-operation-x"}))
		assert.Empty(t, f.sent())
	})
	t.Run("an install operation still running fails and sends no uninstall", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.with(func(f *catalogFake) { f.readsBeforeEnd = 1 << 30 })
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, func(c *catalogCleanup) { c.waitBudget = 300 * time.Millisecond }))
		require.ErrorContains(t, run.err, "teardown "+stepUninstall)
		require.ErrorContains(t, run.err, "the install Operation default/helm-operation-o")
		require.ErrorContains(t, run.err, "did not end within")
		assert.Zero(t, f.count(http.MethodPost, "catalog.cattle.io.apps"))
	})
	t.Run("the recorded uninstall left the app", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.with(func(f *catalogFake) { f.add(&fakeObject{set: "apps", namespace: "default", name: "harvester-rbac"}) })
		_, _, teardown := cleanupSteps("local")
		err := stepNamed(t, teardown, stepUninstall).Do(t.Context(), f.target(), map[string]string{
			requestIDCapture: "0123456789abcdef0123456789abcdef", "installop": "helm-operation-a", "uninstallop": "helm-operation-b",
		})
		require.ErrorContains(t, err, "App default/harvester-rbac still answers status 200 after the recorded uninstall")
		assert.Zero(t, f.count(http.MethodPost, "/"))
	})
	t.Run("an uninstall whose answer was lost", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.answer = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Query().Get("action") != "uninstall" {
				return false
			}
			f.with(func(f *catalogFake) { f.uninstall() })
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": catalogCanary})
			return true
		}
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.ErrorContains(t, run.err, "is an uninstall this run did not record; nothing was sent")
		assert.Equal(t, 1, f.count(http.MethodPost, "catalog.cattle.io.apps"))
	})
	for _, outcome := range []string{"otherid", "noid"} {
		t.Run("an app with another request id or none is left in place: "+outcome, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, "local")
			f.with(func(f *catalogFake) { f.installOutcome = outcome })
			run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, func(c *catalogCleanup) { c.waitBudget = 300 * time.Millisecond }))
			require.ErrorContains(t, run.err, "App default/harvester-rbac does not carry this run's request id; it is left in place")
			assert.Zero(t, f.count(http.MethodPost, "catalog.cattle.io.apps"))
			assert.NotNil(t, f.lookup("apps", "default", "harvester-rbac"))
		})
	}
}

// TestCatalogTimedOutWaitLeavesTheApp asserts that an App that stays
// pending-install ends the wait at the budget, the teardown sends no
// uninstall and deletes the Operation's objects, and the comparison fails
// naming the App, which stays.
func TestCatalogTimedOutWaitLeavesTheApp(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	f.with(func(f *catalogFake) { f.installOutcome = "pending" })
	run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, func(c *catalogCleanup) { c.waitBudget = 300 * time.Millisecond }))
	require.ErrorContains(t, run.err, "observation install-app: still App with spec.info.status pending-install")
	require.ErrorContains(t, run.err, "App default/harvester-rbac is still pending; it is left in place")
	require.ErrorContains(t, run.err, "teardown "+stepCompare)
	require.ErrorContains(t, run.err, "apps default/harvester-rbac")
	assert.Zero(t, f.count(http.MethodPost, "catalog.cattle.io.apps"))
	assert.NotNil(t, f.lookup("apps", "default", "harvester-rbac"))
	assert.Empty(t, f.ours(), "the Operation's objects are deleted")
}

// TestCatalogFailedLocalInstallCleansUp asserts a Stalled install Operation
// on local fails the probe, and the teardown deletes the operation's
// objects.
func TestCatalogFailedLocalInstallCleansUp(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, "local")
	f.with(func(f *catalogFake) { f.installOutcome = "stall" })
	run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
	require.ErrorContains(t, run.err, "observation install-operation: the Operation is Stalled")
	assert.NotContains(t, run.err.Error(), "teardown")
	assert.Empty(t, f.ours())
}

// TestCatalogRecordStep asserts the record step: an Operation whose pod or
// ClusterRole does not match records the Operation and the pod alone and
// names the Operation; an Operation that answers 404 is named while the
// other Operation's objects are still recorded and deleted.
func TestCatalogRecordStep(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(f *catalogFake, op string)
		want   string
	}{
		{"the pod names another cluster role", func(f *catalogFake, op string) {
			f.find("pods", "cattle-system", op).annotations[podClusterRoleAnnotation] = "another-role"
		}, "does not name the Operation's ClusterRole owner; only the Operation and the pod are recorded"},
		{"the operation and the pod name another uid than the cluster role's", func(f *catalogFake, op string) {
			f.find("operations", "default", op).owners[0].uid = "11111111-1111-4111-8111-111111111111"
			f.find("pods", "cattle-system", op).owners[0].uid = "11111111-1111-4111-8111-111111111111"
		}, "the owner uids of Operation default/"},
		{"the pod's owner uid differs from the operation's", func(f *catalogFake, op string) {
			o := f.find("pods", "cattle-system", op)
			o.owners[0].uid = "22222222-2222-4222-8222-222222222222"
		}, "the owner uids of Operation default/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, testDownstream)
			const op = "helm-operation-o1"
			f.afterInstall = func(f *catalogFake, name string) { tc.change(f, name) }
			run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
			require.ErrorContains(t, run.err, "teardown "+stepRecord)
			require.ErrorContains(t, run.err, tc.want)
			assert.Nil(t, f.lookup("operations", "default", op))
			assert.Nil(t, f.lookup("pods", "cattle-system", op))
			roles := 0
			for _, r := range f.ours() {
				if r.set == "clusterroles" {
					roles++
				}
			}
			assert.Equal(t, 1, roles, "the ClusterRole it cannot prove stays")
			assert.Zero(t, f.count(http.MethodDelete, "/clusterroles/"))
		})
	}
	t.Run("an operation that answers 404", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		var real string
		f.with(func(f *catalogFake) { real = f.operation("uninstall") })
		_, _, teardown := cleanupSteps("local")
		captured := map[string]string{"installop": "helm-operation-missing", "uninstallop": real}
		log := &recordingLog{}
		ctx := withLog(t.Context(), log.log)
		err := stepNamed(t, teardown, stepRecord).Do(ctx, f.target(), captured)
		require.ErrorContains(t, err, "Operation default/helm-operation-missing answered status 404; nothing of it is recorded")
		require.NoError(t, stepNamed(t, teardown, stepRemove).Do(ctx, f.target(), captured))
		require.NoError(t, stepNamed(t, teardown, stepGone).Do(ctx, f.target(), captured))
		assert.Empty(t, f.ours(), "the other Operation's objects are deleted")
		assert.Contains(t, log.all(), "created: operations default/"+real)
	})
}

// TestCatalogRemoveStepCollectsEveryFailure asserts one delete that answers
// 500 and one that answers 409 do not stop the others, and the error names
// both with their status and no body.
func TestCatalogRemoveStepCollectsEveryFailure(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, testDownstream)
	f.afterInstall = func(f *catalogFake, op string) {
		f.deleteStatus["operations/"+op] = http.StatusInternalServerError
		f.deleteStatus["clusterrolebindings/helm-op-pod-impersonation-helm-op-r1-b"] = http.StatusConflict
	}
	run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
	require.ErrorContains(t, run.err, "deleting operations default/helm-operation-o1: status 500")
	require.ErrorContains(t, run.err, "deleting clusterrolebindings helm-op-pod-impersonation-helm-op-r1-b: status 409")
	assert.Nil(t, f.lookup("pods", "cattle-system", "helm-operation-o1"), "the deletes after a failure are sent")
	assert.Nil(t, f.lookup("clusterroles", "", "pod-impersonation-helm-op-r1"))
}

// TestCatalogGoneStepNamesWhatStays asserts the wait for the recorded
// objects names one that never goes away, and reads each object as a
// Table.
func TestCatalogGoneStepNamesWhatStays(t *testing.T) {
	t.Parallel()
	f := newCatalogFake(t, testDownstream)
	f.afterInstall = func(f *catalogFake, op string) {
		f.undeletable["helm-op-pod-impersonation-helm-op-r1-b"] = true
	}
	run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
	require.ErrorContains(t, run.err, "teardown "+stepGone)
	require.ErrorContains(t, run.err, "still present after 500ms: clusterrolebindings helm-op-pod-impersonation-helm-op-r1-b")
	lastDelete := -1
	sent := f.sent()
	for i, r := range sent {
		if r.method == http.MethodDelete {
			lastDelete = i
		}
	}
	require.Positive(t, lastDelete)
	reads := 0
	for _, r := range sent[lastDelete+1:] {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/helm-op-pod-impersonation-helm-op-r1-b") {
			reads++
			assert.Equal(t, tableOnlyAccept, r.accept, "every read of a recorded object is a Table read")
		}
	}
	assert.Positive(t, reads)
}

// TestCatalogCompareStepFailsOnEveryOperationPrefix asserts, for every row
// of operationPrefixes, that a new object with that prefix and no owner
// fails the comparison and is not deleted.
func TestCatalogCompareStepFailsOnEveryOperationPrefix(t *testing.T) {
	t.Parallel()
	for _, prefix := range operationPrefixes {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, testDownstream)
			name := prefix + "stray"
			f.afterInstall = func(f *catalogFake, _ string) {
				f.add(&fakeObject{set: "configmaps", namespace: "cattle-system", name: name})
			}
			run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
			require.ErrorContains(t, run.err, "teardown "+stepCompare)
			require.ErrorContains(t, run.err, "left in place: configmaps cattle-system/"+name)
			assert.NotNil(t, f.lookup("configmaps", "cattle-system", name))
		})
	}
}

// TestCatalogCompareStep asserts the comparison's other rules: a new object
// tied to an Operation in the release namespace fails; a prefixed object
// tied to an Operation in cattle-system and a new impersonation ClusterRole
// are logged as not ours and stay; a RoleTemplate listed with a
// deletionTimestamp after the uninstall passes once a read no longer lists
// it and fails when it stays; with no before snapshot the step sends
// nothing.
func TestCatalogCompareStep(t *testing.T) {
	t.Parallel()
	t.Run("tied to an operation in the release namespace", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, testDownstream)
		f.afterInstall = func(f *catalogFake, _ string) {
			role := f.add(&fakeObject{set: "clusterroles", name: "pod-impersonation-helm-op-other"})
			owner := []ownerRef{{kind: "ClusterRole", name: role.name, uid: role.uid}}
			f.add(&fakeObject{set: "operations", namespace: "kube-system", name: "unrelated"})
			f.add(&fakeObject{set: "operations", namespace: "default", name: "helm-operation-other", owners: owner})
			f.add(&fakeObject{set: "secrets", namespace: "cattle-system", name: "helm-operation-other-s", owners: owner})
		}
		run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
		require.ErrorContains(t, run.err, "secrets cattle-system/helm-operation-other-s")
		require.ErrorContains(t, run.err, "clusterroles pod-impersonation-helm-op-other")
		require.ErrorContains(t, run.err, "operations default/helm-operation-other")
		assert.NotNil(t, f.lookup("secrets", "cattle-system", "helm-operation-other-s"))
	})
	t.Run("tied to an operation in cattle-system", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, testDownstream)
		f.afterInstall = func(f *catalogFake, _ string) {
			role := f.add(&fakeObject{set: "clusterroles", name: "pod-impersonation-helm-op-sys"})
			owner := []ownerRef{{kind: "ClusterRole", name: role.name, uid: role.uid}}
			f.add(&fakeObject{set: "operations", namespace: "cattle-system", name: "helm-operation-sys", owners: owner})
			pod := f.add(&fakeObject{set: "pods", namespace: "cattle-system", name: "helm-operation-sys", owners: owner})
			f.add(&fakeObject{set: "serviceaccounts", namespace: "cattle-system", name: "pod-impersonation-helm-op-sys",
				owners: []ownerRef{{kind: "Pod", name: pod.name, uid: pod.uid}}})
			f.add(&fakeObject{set: "clusterrolebindings", name: "helm-op-pod-impersonation-helm-op-sys-b",
				labels: map[string]string{forRoleLabel: role.name}})
			f.add(&fakeObject{set: "clusterroles", name: "cattle-impersonation-u-abcde"})
		}
		run := runCatalog(t, f, f.target(), catalogProbe(t, downstreamWriteProbe, nil))
		require.NoError(t, run.err)
		for _, line := range []string{
			"not ours: pods cattle-system/helm-operation-sys",
			"not ours: serviceaccounts cattle-system/pod-impersonation-helm-op-sys",
			"not ours: clusterroles pod-impersonation-helm-op-sys",
			"not ours: clusterrolebindings helm-op-pod-impersonation-helm-op-sys-b",
			"not ours: clusterroles cattle-impersonation-u-abcde",
		} {
			assert.Contains(t, run.logs, line)
		}
		assert.NotNil(t, f.lookup("pods", "cattle-system", "helm-operation-sys"))
		assert.NotNil(t, f.lookup("clusterroles", "", "pod-impersonation-helm-op-sys"))
	})
	t.Run("a roletemplate gone on a later read", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.with(func(f *catalogFake) { f.templateReadsAfterUninstall = 2 })
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.NoError(t, run.err)
		assert.Nil(t, f.lookup("roletemplates", "", "virt-cluster-manage"))
	})
	t.Run("a roletemplate that stays", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		f.with(func(f *catalogFake) { f.templateReadsAfterUninstall = -1 })
		run := runCatalog(t, f, f.target(), catalogProbe(t, localWriteProbe, nil))
		require.ErrorContains(t, run.err, "teardown "+stepCompare)
		require.ErrorContains(t, run.err, "roletemplates virt-cluster-manage")
		assert.NotNil(t, f.lookup("roletemplates", "", "virt-cluster-manage"))
	})
	t.Run("no before snapshot", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		_, _, teardown := cleanupSteps("local")
		require.NoError(t, stepNamed(t, teardown, stepCompare).Do(t.Context(), f.target(), map[string]string{}))
		assert.Empty(t, f.sent())
	})
}

// panicTransport panics on every request match selects, as a step's code
// could, and sends the others through base.
type panicTransport struct {
	base  http.RoundTripper
	match func(r *http.Request) bool
}

func (p panicTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.match(r) {
		panic(catalogCanary)
	}
	return p.base.RoundTrip(r)
}

// panicTarget is the fake's own-pair target whose requests match selects
// panic.
func panicTarget(f *catalogFake, match func(r *http.Request) bool) *OracleTarget {
	tgt := f.target()
	tgt.client = &http.Client{Transport: panicTransport{base: f.srv.Client().Transport, match: match}}
	return tgt
}

// TestCatalogStepPanicsAreErrors asserts a step that panics returns an
// error naming the step, with no panic value from the server side, and the
// teardown steps after it still run.
func TestCatalogStepPanicsAreErrors(t *testing.T) {
	t.Parallel()
	t.Run("a teardown step", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		var recorded []objRef
		f.with(func(f *catalogFake) {
			f.operation("install")
			for _, o := range f.objects {
				if o.ours && o.set == "configmaps" {
					recorded = append(recorded, o.ref())
				}
			}
		})
		require.Len(t, recorded, 2)
		tgt := panicTarget(f, func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/operations/") })
		_, _, teardown := cleanupSteps("local")
		captured := map[string]string{"installop": "helm-operation-o1", catalogRecorded: encodeRefs(recorded)}
		err := runTeardown(t.Context(), tgt, Probe{Name: "p", Teardown: teardown}, captured, 5*time.Second)
		require.ErrorContains(t, err, `teardown record each operation's objects: the step "record each operation's objects" panicked`)
		assert.NotContains(t, err.Error(), catalogCanary)
		assert.Equal(t, 2, f.count(http.MethodDelete, "/configmaps/"), "the deletes after the panic are sent")
	})
	t.Run("a setup step", func(t *testing.T) {
		t.Parallel()
		f := newCatalogFake(t, "local")
		tgt := panicTarget(f, func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/apps/harvester-rbac") })
		run := runCatalog(t, f, tgt, catalogProbe(t, localWriteProbe, nil))
		require.ErrorContains(t, run.err, `setup `+stepPrecheck+`: the step "`+stepPrecheck+`" panicked`)
		assert.Zero(t, f.count(http.MethodPost, "/"))
	})
}

// TestAdminTableAnswers asserts how adminTable reads an answer: a 404 is
// not found, another status outside 2xx fails, an answer of another kind
// fails, rows: null is an empty list, and the application/json content type
// is accepted.
func TestAdminTableAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		rows    int
		wantErr string
		missing bool
	}{
		{name: "not found", status: 404, body: `{"kind":"Status","reason":"NotFound"}`, missing: true},
		{name: "another status", status: 500, body: `{"message":"` + catalogCanary + `"}`, wantErr: "status 500"},
		{name: "another kind", status: 200, body: `{"kind":"List","items":[]}`, wantErr: "not a Table"},
		{name: "not json", status: 200, body: catalogCanary, wantErr: "not a JSON Table"},
		{name: "rows null", status: 200, body: `{"kind":"Table","rows":null}`},
		{name: "rows", status: 200, body: `{"kind":"Table","rows":[{"object":{"metadata":{"name":"a","uid":"u"}}}]}`, rows: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var accept string
			srv := newJSONServer(t, func(w http.ResponseWriter, r *http.Request) {
				accept = r.Header.Get("Accept")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			rows, err := adminTable(t.Context(), adminTarget(srv), "/x", "pods")
			assert.Equal(t, tableOnlyAccept, accept)
			switch {
			case tc.missing:
				require.ErrorIs(t, err, errTableNotFound)
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), catalogCanary)
			default:
				require.NoError(t, err)
				assert.Len(t, rows, tc.rows)
			}
		})
	}
}

// TestOwnPairCheckErrorsHoldNoServerText asserts the own-pair check's
// failure and skip hold no body text and no install id.
func TestOwnPairCheckErrorsHoldNoServerText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a failed read", http.StatusInternalServerError, `{"message":"` + catalogCanary + `"}`},
		{"another install id", http.StatusOK, `{"value":"another-` + catalogCanary + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, "local")
			f.answer = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != installUUIDPath {
					return false
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
				return true
			}
			_, err := runProbe(t.Context(), f.target(), catalogProbe(t, localWriteProbe, nil), 5*time.Second)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), catalogCanary)
			assert.NotContains(t, err.Error(), catalogFakeUUID)
			assert.Equal(t, 1, f.count(http.MethodGet, installUUIDPath), "one read of the install id")
			assert.Len(t, f.sent(), 2, "the version and the install id reads only")
		})
	}
}

// timeDependentPatterns are the values no catalog write golden may hold,
// each with what it finds.
var timeDependentPatterns = []struct {
	what    string
	pattern *regexp.Regexp
}{
	{"a generated operation name", regexp.MustCompile(`helm-operation-[a-z0-9]{5}`)},
	{"an impersonation name", regexp.MustCompile(`pod-impersonation-helm-op-`)},
	{"a request id", regexp.MustCompile(`\b[0-9a-f]{32}\b`)},
	{"an RFC 3339 timestamp", regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)},
	{"a uid", regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)},
	{"a downstream cluster id", regexp.MustCompile(`\bc-m-[a-z0-9]+`)},
	{"a URL with a host", regexp.MustCompile(`[a-z]+://[^\s"/<{]+`)},
}

// timeDependentKeys are the keys no catalog write golden may hold.
var timeDependentKeys = []string{"token", "command"}

// timeDependentValues lists each value of the golden's observations that
// differs between runs or instances. It reads the observations only, never
// the oracle image.
func timeDependentValues(g Golden) []string {
	var found []string
	for _, o := range g.Observations {
		raw, _ := json.Marshal(o.Body)
		text := string(raw) + "\n" + o.Text
		for _, p := range timeDependentPatterns {
			for _, m := range p.pattern.FindAllString(text, -1) {
				found = append(found, fmt.Sprintf("%s: %s %q", o.Name, p.what, m))
			}
		}
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				for k, child := range t {
					if slices.Contains(timeDependentKeys, k) {
						found = append(found, fmt.Sprintf("%s: the key %q", o.Name, k))
					}
					walk(child)
				}
			case []any:
				for _, child := range t {
					walk(child)
				}
			}
		}
		walk(o.Body)
		if o.Body["kind"] == "App" {
			meta, _ := o.Body["metadata"].(map[string]any)
			status, _ := o.Body["status"].(map[string]any)
			if _, ok := meta["generation"]; ok {
				found = append(found, o.Name+": the App's metadata.generation")
			}
			if _, ok := status["observedGeneration"]; ok {
				found = append(found, o.Name+": the App's status.observedGeneration")
			}
		}
	}
	return found
}

// TestTimeDependentValuesFindsEachKind asserts the golden scan finds each
// kind of value that differs between runs and passes a golden whose only
// hex is an image digest.
func TestTimeDependentValuesFindsEachKind(t *testing.T) {
	t.Parallel()
	body := func(raw string) Golden {
		var b map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &b))
		return Golden{OracleImage: "rancher/rancher@sha256:" + strings.Repeat("ab", 32), Observations: []ObservationResult{{Name: "o", Body: b}}}
	}
	for _, tc := range []struct {
		name  string
		body  string
		found bool
	}{
		{"placeholders only", `{"operationName":"{installop}","metadata":{"uid":"<uid>"},"links":"<links>"}`, false},
		{"an image digest", `{"image":"rancher/rancher@sha256:` + strings.Repeat("0f", 32) + `"}`, false},
		{"an operation name", `{"operationName":"helm-operation-pcdqf"}`, true},
		{"an impersonation name", `{"metadata":{"ownerReferences":[{"name":"pod-impersonation-helm-op-dbkb9"}]}}`, true},
		{"a request id", `{"id":"ebb42960f960ac19a08a8d9ad7ca1d8e"}`, true},
		{"a timestamp", `{"lastUpdateTime":"2026-11-11T06:15:00Z"}`, true},
		{"a uid", `{"uid":"85395a4d-9058-4047-befc-c10df8cb4e2d"}`, true},
		{"a cluster id", `{"clusterId":"c-m-mghjb2gp"}`, true},
		{"a url", `{"links":{"self":"https://127.0.0.1:8443/v1/x"}}`, true},
		{"a token key", `{"status":{"token":"<secret>"}}`, true},
		{"a command key", `{"status":{"command":["helm"]}}`, true},
		{"an app generation", `{"kind":"App","metadata":{"generation":2}}`, true},
		{"an app observed generation", `{"kind":"App","status":{"observedGeneration":2}}`, true},
		{"an operation generation", `{"kind":"Operation","metadata":{"generation":1},"status":{"observedGeneration":1}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found := timeDependentValues(body(tc.body))
			if tc.found {
				assert.NotEmpty(t, found)
			} else {
				assert.Empty(t, found)
			}
		})
	}
	assert.Empty(t, timeDependentValues(Golden{OracleImage: "x@sha256:" + strings.Repeat("ab", 32) + " 2026-11-11T00:00:00Z"}), "the oracle image is never read")
}

// TestEveryCatalogWriteGoldenHoldsNoTimeDependentValue asserts the recorded
// golden of every catalog write probe exists and holds no value that
// differs between runs or instances.
func TestEveryCatalogWriteGoldenHoldsNoTimeDependentValue(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, p := range All() {
		if !isCatalogWriteProbe(p) {
			continue
		}
		checked++
		dir := "oracle"
		if p.Downstream {
			dir = "downstream"
		}
		g, err := LoadGolden(GoldenPath("testdata/"+dir, p.Name))
		require.NoError(t, err, "the golden of %s is recorded", p.Name)
		assert.Empty(t, timeDependentValues(g), p.Name)
	}
	assert.GreaterOrEqual(t, checked, 2)
}

// TestEveryCatalogWriteWaitEndsWithTheBudget asserts every Until
// observation of every catalog write probe fails before its first read when
// the wait deadline has passed, and that each such probe's last setup step
// stores the wait deadline.
func TestEveryCatalogWriteWaitEndsWithTheBudget(t *testing.T) {
	t.Parallel()
	waits := 0
	for _, p := range All() {
		if !isCatalogWriteProbe(p) {
			continue
		}
		require.NotEmpty(t, p.Setup, p.Name)
		captured := map[string]string{}
		require.NoError(t, p.Setup[len(p.Setup)-1].Do(t.Context(), nil, captured), p.Name)
		assert.Contains(t, captured, waitDeadlineCapture, "the last setup step of %s stores the wait deadline", p.Name)
		for _, o := range p.Observations {
			if o.Until == nil {
				continue
			}
			waits++
			srv := newJSONServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			past := map[string]string{"downstream": testDownstream, waitDeadlineCapture: time.Now().Add(-time.Second).Format(time.RFC3339Nano)}
			ct := &countingTransport{next: srv.Client().Transport}
			counting := &fakeTarget{srv: srv, bearers: map[Identity]string{IdentityAdmin: "a"}, client: &http.Client{Transport: ct}}
			_, _, err := runObservation(t.Context(), counting, o, past, 5*time.Second, RunOptions{KeepLastAnswer: true})
			require.ErrorContains(t, err, "the wait deadline in unrecorded.waitdeadline passed before the first read", "%s %s", p.Name, o.Name)
			assert.Zero(t, ct.n.Load(), "%s %s sends no request", p.Name, o.Name)
		}
	}
	assert.GreaterOrEqual(t, waits, 4)
}

// newJSONServer starts a test server that answers with handler.
func newJSONServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// deleteSites checks the cleanup's source: the DELETE method appears in
// exactly one function, which takes the recorded object it deletes, and
// nothing calls deleteFixture. It returns one problem per rule broken.
func deleteSites(src string) []string {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "catalogcleanup.go", src, 0)
	if err != nil {
		return []string{"parsing: " + err.Error()}
	}
	var problems, deleters []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		deletes := false
		ast.Inspect(fn, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "http" && n.Sel.Name == "MethodDelete" {
					deletes = true
				}
			case *ast.BasicLit:
				if n.Kind == token.STRING && strings.Contains(n.Value, "DELETE") {
					deletes = true
				}
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "deleteFixture" {
					problems = append(problems, fn.Name.Name+" calls deleteFixture")
				}
			}
			return true
		})
		if !deletes {
			continue
		}
		deleters = append(deleters, fn.Name.Name)
		takesRef := slices.ContainsFunc(fn.Type.Params.List, func(f *ast.Field) bool {
			id, ok := f.Type.(*ast.Ident)
			return ok && id.Name == "objRef"
		})
		if !takesRef {
			problems = append(problems, fn.Name.Name+" deletes without taking a recorded objRef")
		}
	}
	if len(deleters) != 1 {
		problems = append(problems, fmt.Sprintf("the DELETE method appears in %d functions %v, want exactly one", len(deleters), deleters))
	}
	return problems
}

// TestCatalogCleanupDeletesOnlyRecordedObjects asserts the cleanup's source
// sends a DELETE from one function only, which takes the recorded object,
// and never calls deleteFixture; and that the check finds a second DELETE
// site and a deleteFixture call.
func TestCatalogCleanupDeletesOnlyRecordedObjects(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("catalogcleanup.go")
	require.NoError(t, err)
	src := string(raw)
	assert.Empty(t, deleteSites(src))
	for _, tc := range []struct{ name, add, want string }{
		{"a second delete site", "\nfunc another(ctx context.Context, tgt Target) { _, _ = adminDo(ctx, tgt, http.MethodDelete, \"/x\", \"\") }\n", "want exactly one"},
		{"a literal delete", "\nfunc another(ctx context.Context, tgt Target) { _, _ = adminDo(ctx, tgt, \"DELETE\", \"/x\", \"\") }\n", "want exactly one"},
		{"a fixture delete", "\nfunc another() Step { return deleteFixture(\"x\", \"/x\") }\n", "calls deleteFixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := deleteSites(src + tc.add)
			assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }), "%v", problems)
		})
	}
}

// runStepRecovering runs one step and returns its error, or an error that
// says the panic reached the caller when the step did not recover it.
func runStepRecovering(ctx context.Context, s Step, tgt Target, captured map[string]string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the panic of step %q reached its caller", s.Name)
		}
	}()
	return s.Do(ctx, tgt, captured)
}

// TestEveryCleanupStepRecoversItsPanic asserts, for every setup and teardown
// step the cleanup of each probe returns, that a panic in the step is
// returned as an error naming the step and holding no panic value from the
// server side, and that each teardown step still runs after the one before
// it panicked. Every request panics; a step that sends none is run again with
// a nil capture map, whose first write panics.
func TestEveryCleanupStepRecoversItsPanic(t *testing.T) {
	t.Parallel()
	seeded := func() map[string]string {
		return map[string]string{
			"downstream": testDownstream, requestIDCapture: "0123456789abcdef0123456789abcdef",
			"installop": "helm-operation-a", "uninstallop": "helm-operation-b", "failop": "helm-operation-c",
			catalogRecorded:      encodeRefs([]objRef{{set: "configmaps", namespace: "cattle-system", name: "impersonation-helm-op-x", uid: "u"}}),
			catalogSnapshotTaken: "true",
		}
	}
	for _, cluster := range []string{"local", testDownstream} {
		t.Run(cluster, func(t *testing.T) {
			t.Parallel()
			f := newCatalogFake(t, cluster)
			tgt := panicTarget(f, func(*http.Request) bool { return true })
			_, setup, teardown := cleanupSteps(cluster)
			for _, s := range slices.Concat(setup, teardown) {
				err := runStepRecovering(t.Context(), s, tgt, seeded())
				if err == nil {
					err = runStepRecovering(t.Context(), s, tgt, nil)
				}
				require.ErrorContains(t, err, fmt.Sprintf("the step %q panicked", s.Name), "step %q", s.Name)
				assert.NotContains(t, err.Error(), catalogCanary, "step %q", s.Name)
			}
			err := runTeardown(t.Context(), tgt, Probe{Name: "p", Teardown: teardown}, seeded(), 5*time.Second)
			for _, s := range teardown {
				require.ErrorContains(t, err, fmt.Sprintf("teardown %s: the step %q panicked", s.Name, s.Name), "every teardown step runs after the one before it panicked")
			}
			assert.Empty(t, f.sent(), "every request panicked before it was sent")
		})
	}
}

// TestCatalogAppObservationsRecordNoGeneration asserts that the rules of
// every App observation of a catalog write probe drop the App's
// metadata.generation and status.observedGeneration: the CLI reads neither,
// and their value depends on the timing of Rancher's app controller. The
// observations that record the App's 404 have no rules; the golden scan
// (timeDependentValues) fails on either field in any recorded App.
func TestCatalogAppObservationsRecordNoGeneration(t *testing.T) {
	t.Parallel()
	apps := 0
	for _, p := range All() {
		if !isCatalogWriteProbe(p) {
			continue
		}
		for _, o := range p.Observations {
			if !strings.Contains(o.Request.Path, "/apps/") || len(o.Normalize) == 0 {
				continue
			}
			apps++
			body := map[string]any{
				"kind":     "App",
				"metadata": map[string]any{"name": "harvester-rbac", "namespace": "default", "generation": float64(2)},
				"spec":     map[string]any{"info": map[string]any{"status": "deployed"}},
				"status":   map[string]any{"observedGeneration": float64(2), "summary": map[string]any{"state": "deployed"}},
			}
			Normalize(body, slices.Concat(DefaultNormalizeRules(), p.Normalize, o.Normalize))
			meta, _ := body["metadata"].(map[string]any)
			status, _ := body["status"].(map[string]any)
			assert.NotContains(t, meta, "generation", "%s %s", p.Name, o.Name)
			assert.NotContains(t, status, "observedGeneration", "%s %s", p.Name, o.Name)
		}
	}
	assert.Positive(t, apps, "the install-app observation is checked")
}
