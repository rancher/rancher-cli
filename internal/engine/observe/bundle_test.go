package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func execBundle(t *testing.T, clients engine.ClusterClientFactory, in input) (runtime.Result, error) {
	t.Helper()
	cmd := NewSupportBundle(clients, func() any { return map[string]string{"cliVersion": "v9.9.9"} }).(runtime.ReadCommand)
	return cmd.Execute(context.Background(), in, nil)
}

func manifestFrom(t *testing.T, path string) bundleManifest {
	t.Helper()
	contents := readArchive(t, path)
	raw, ok := contents["manifest.json"]
	require.True(t, ok, "archive must contain manifest.json")
	var m bundleManifest
	require.NoError(t, json.Unmarshal([]byte(raw), &m))
	return m
}

func findItem(t *testing.T, m bundleManifest, name, cluster string) manifestItem {
	t.Helper()
	for _, it := range m.Items {
		if it.Name == name && it.Cluster == cluster {
			return it
		}
	}
	t.Fatalf("manifest has no item {name:%q cluster:%q}; items: %+v", name, cluster, m.Items)
	return manifestItem{}
}

func TestSupportBundleManifestTriState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newFakeClient() // no app=rancher pods, no rancher Deployment: server degrades to the Docker message
	downstream := newFakeClient()
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagFilename: path,
		flagTargets:  "server,agent",
		flagClusters: "local,c-1",
	}})
	require.NoError(t, err, "a degraded target must never fail the whole command — partial bundles ship")

	m := manifestFrom(t, path)
	assert.Contains(t, findItem(t, m, "server", engine.LocalCluster).Status, "error:")
	assert.Equal(t, statusSkippedNotApplicable, findItem(t, m, "agent", engine.LocalCluster).Status)
	assert.Equal(t, statusSkippedNotApplicable, findItem(t, m, "server", "c-1").Status)
	assert.Equal(t, statusCollected, findItem(t, m, "agent", "c-1").Status)

	data, ok := res.Data.(bundleResult)
	require.True(t, ok)
	assert.Equal(t, path, data.Path)
	assert.Greater(t, data.Errors, 0)
	assert.Greater(t, data.Skipped, 0)
}

func TestSupportBundleRBACFailingCollectorStillShipsBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	podKind := mustKind(t, "pod")
	clusterKind := mustKind(t, "cluster")
	local := newFakeClient().withObject(clusterKind.GVR, "", "c-1", &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream := newFakeClient().withListErr(podKind.GVR, "cattle-system", errors.New("rbac denied"))
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagFilename: path,
		flagTargets:  "agent",
		flagClusters: "c-1",
	}})
	require.NoError(t, err)

	m := manifestFrom(t, path)
	assert.Contains(t, findItem(t, m, "agent", "c-1").Status, "rbac denied")

	data := res.Data.(bundleResult)
	assert.Equal(t, 1, data.Errors)

	contents := readArchive(t, path)
	_, hasManifest := contents["manifest.json"]
	assert.True(t, hasManifest, "the bundle must still be written despite the collector failure")
}

func TestSupportBundleSettingsAllowlist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	settingKind := mustKind(t, "setting")
	local := newFakeClient().
		withObject(settingKind.GVR, "", "server-version", &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "server-version"},
			"value":    "v2.9.0",
		}}).
		withObject(settingKind.GVR, "", "cacerts", &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "cacerts"},
			"value":    "-----BEGIN CERTIFICATE-----super-secret",
		}})
	factory := &fakeFactory{local: local}

	path := filepath.Join(dir, "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagFilename: path, flagTargets: "", flagClusters: "local"}})
	require.NoError(t, err)

	contents := readArchive(t, path)
	settingsJSON, ok := contents["rancher/settings.json"]
	require.True(t, ok)
	assert.Contains(t, settingsJSON, "server-version")
	assert.Contains(t, settingsJSON, "v2.9.0")
	assert.NotContains(t, settingsJSON, "cacerts")
	assert.NotContains(t, settingsJSON, "super-secret")

	for _, body := range contents {
		assert.NotContains(t, body, "super-secret", "a non-allowlisted setting must never appear anywhere in the bundle")
	}
}

func TestSupportBundleEventsPerNamespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	podKind := mustKind(t, "pod")
	clusterKind := mustKind(t, "cluster")
	provKind := mustKind(t, "provisioningcluster")
	fleetKind := mustKind(t, "fleetcluster")
	eventKind := mustKind(t, "event")

	mgmtCluster := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c-1"}, "spec": map[string]any{}}}
	provList := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}, "status": map[string]any{"clusterName": "c-1"}}},
	}}
	fleetCluster := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1"},
		"spec":     map[string]any{"agentNamespace": "custom-ns"},
	}}
	local := newFakeClient().
		withObject(clusterKind.GVR, "", "c-1", mgmtCluster).
		withListNS(provKind.GVR, "fleet-default", provList).
		withObject(fleetKind.GVR, "fleet-default", "p-1", fleetCluster)
	downstream := newFakeClient().
		withListNS(podKind.GVR, "custom-ns", podList("fleet-agent-1")).
		withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{}).
		withListNS(eventKind.GVR, "custom-ns", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "ev-1"}, "message": "crashlooping"}},
		}})
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagFilename: path, flagTargets: "fleet-agent", flagClusters: "c-1"}})
	require.NoError(t, err)

	contents := readArchive(t, path)
	_, hasCattleSystem := contents["clusters/c-1/events-cattle-system.json"]
	assert.True(t, hasCattleSystem, "cattle-system events must always be collected")
	customEvents, hasCustom := contents["clusters/c-1/events-custom-ns.json"]
	require.True(t, hasCustom, "the fleet-agent target's resolved namespace must also get an events file")
	assert.Contains(t, customEvents, "crashlooping")
}

func TestSupportBundleDefaultFilenameUsesClock(t *testing.T) {
	// Not t.Parallel(): mutates the package-level nowFunc and the process cwd.
	origNow := nowFunc
	defer func() { nowFunc = origNow }()
	fixed := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return fixed }

	dir := t.TempDir()
	origWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	defer func() { _ = os.Chdir(origWD) }()

	factory := &fakeFactory{local: newFakeClient()}
	res, err := execBundle(t, factory, input{s: map[string]string{flagClusters: "local", flagTargets: "webhook"}})
	require.NoError(t, err)
	data := res.Data.(bundleResult)
	assert.Equal(t, fmt.Sprintf("rancher-support-%d.tar.gz", fixed.Unix()), data.Path)
}
