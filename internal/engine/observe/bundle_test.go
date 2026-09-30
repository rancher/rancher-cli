package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func execBundle(t *testing.T, clients engine.ClusterClientFactory, in input, opts ...func(*supportBundleCmd)) (runtime.Result, error) {
	t.Helper()
	cmd := NewSupportBundle(clients, func() any { return map[string]string{"cliVersion": "v9.9.9"} }).(*supportBundleCmd)
	for _, opt := range opts {
		opt(cmd)
	}
	return cmd.Execute(t.Context(), in, nil)
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
	require.NoError(t, err, "a degraded target must never fail the whole command - partial bundles ship")

	m := manifestFrom(t, path)
	assert.Contains(t, findItem(t, m, "server", engine.LocalCluster).Status, "error:")
	assert.Equal(t, statusSkippedNotApplicable, findItem(t, m, "agent", engine.LocalCluster).Status)
	assert.Equal(t, statusSkippedNotApplicable, findItem(t, m, "server", "c-1").Status)
	assert.Equal(t, statusCollected, findItem(t, m, "agent", "c-1").Status)

	data, ok := res.Data.(bundleResult)
	require.True(t, ok)
	assert.Equal(t, path, data.Path)
	assert.Positive(t, data.Errors)
	assert.Positive(t, data.Skipped)
	assert.Contains(t, res.Text, "support bundle written: "+path)
	assert.Contains(t, res.Text, fmt.Sprintf("skipped: %d, errors: %d", data.Skipped, data.Errors))
}

// TestFormatIECBytes asserts the IEC byte formatter's exact output at each unit
// boundary - the raw byte count stays in Data for -o json; this is only the
// operator-facing text rendering.
func TestFormatIECBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1023 * 1024, "1023.0 KiB"},
		// One byte under each unit boundary rounds to "1024.0" in the
		// smaller unit at print time - it must roll into the larger unit.
		{1<<20 - 1, "1.0 MiB"},
		{1 << 20, "1.0 MiB"},
		{3355443, "3.2 MiB"},
		{1<<30 - 1, "1.0 GiB"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, formatIECBytes(c.n))
	}
}

// TestRenderBundleText asserts support-bundle's exact default-format text,
// base case and the skipped/errors conditional line.
func TestRenderBundleText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "support bundle written: /tmp/b.tar.gz\n  5 items, 1.0 KiB\n",
		renderBundleText("/tmp/b.tar.gz", 5, 1024, 0, 0).Text)
	assert.Equal(t, "support bundle written: /tmp/b.tar.gz\n  5 items, 1.0 KiB\n  skipped: 2, errors: 1\n",
		renderBundleText("/tmp/b.tar.gz", 5, 1024, 2, 1).Text)
}

// TestSupportBundleNonACEKubeAPIAuthIsSkippedNotError asserts that
// kube-api-auth on a non-ACE cluster is a structural inapplicability
// detected at target resolution time, not an attempted-and-failed
// collection, so it must manifest as skipped, never error.
func TestSupportBundleNonACEKubeAPIAuthIsSkippedNotError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nonACECluster := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
		"spec":     map[string]any{},
	}}
	local := newFakeClient().withObject(clusterKind.GVR, "", "c-1", nonACECluster)
	downstream := newFakeClient()
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagFilename: path,
		flagTargets:  "kube-api-auth",
		flagClusters: "c-1",
	}})
	require.NoError(t, err)

	m := manifestFrom(t, path)
	item := findItem(t, m, "kube-api-auth", "c-1")
	assert.True(t, strings.HasPrefix(item.Status, "skipped:"), "want a skipped status, got %q", item.Status)
	assert.NotContains(t, item.Status, "error:")

	data, ok := res.Data.(bundleResult)
	require.True(t, ok)
	assert.Equal(t, 0, data.Errors, "a non-ACE cluster must not count as an error")
	assert.Positive(t, data.Skipped)
}

func TestSupportBundleRBACFailingCollectorStillShipsBundle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
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

func TestCollectFeaturesSerializesFalseValues(t *testing.T) {
	t.Parallel()
	local := newFakeClient().withListNS(featureKind.GVR, "", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		{Object: map[string]any{
			"metadata": map[string]any{"name": "harvester"},
			"spec":     map[string]any{"value": false},
			"status":   map[string]any{"default": false, "lockedValue": false, "description": "disabled"},
		}},
	}})

	data, err := collectFeatures(t.Context(), local)
	require.NoError(t, err)

	// A dropped key here would be indistinguishable from "collector never
	// populated this field" - omitempty on a bool is exactly that trap.
	assert.Contains(t, string(data), `"value":false`)
	assert.Contains(t, string(data), `"default":false`)
	assert.Contains(t, string(data), `"lockedValue":false`)
}

func TestSupportBundleClusterDiscoveryFailureIsSignaled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newFakeClient().withListErr(clusterKind.GVR, "", errors.New("rbac denied listing clusters"))
	factory := &fakeFactory{local: local}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{flagFilename: path, flagTargets: "webhook"}})
	require.NoError(t, err, "a fleet-discovery failure must degrade to local-only, not abort the command")

	m := manifestFrom(t, path)
	item := findItem(t, m, "cluster-discovery", "")
	assert.Contains(t, item.Status, "rbac denied listing clusters")

	data := res.Data.(bundleResult)
	assert.Positive(t, data.Errors)

	contents := readArchive(t, path)
	_, hasManifest := contents["manifest.json"]
	assert.True(t, hasManifest, "the local-only bundle must still ship")
}

// TestSupportBundleEventsAreCappedNewestFirst proves the per-namespace event
// payload is bounded even when the cluster reports far more events than
// defaultEventCap: the archived file keeps only the newest defaultEventCap
// entries, and the manifest names both the kept and total counts so a
// reader can see truncation happened.
func TestSupportBundleEventsAreCappedNewestFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const total = defaultEventCap + 10
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]unstructured.Unstructured, 0, total)
	for i := range total {
		items = append(items, unstructured.Unstructured{Object: map[string]any{
			"metadata":      map[string]any{"name": fmt.Sprintf("ev-%d", i)},
			"lastTimestamp": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			"message":       fmt.Sprintf("event %d", i),
		}})
	}

	local := newFakeClient().withObject(clusterKind.GVR, "", "c-1", &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream := newFakeClient().withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{Items: items})
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagFilename: path, flagTargets: "", flagClusters: "c-1"}})
	require.NoError(t, err)

	m := manifestFrom(t, path)
	item := findItem(t, m, "events-cattle-system", "c-1")
	assert.Equal(t, fmt.Sprintf("collected (truncated to %d of %d, newest first)", defaultEventCap, total), item.Status)

	contents := readArchive(t, path)
	eventsJSON, ok := contents["clusters/c-1/events-cattle-system.json"]
	require.True(t, ok)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(eventsJSON), &rows))
	assert.Len(t, rows, defaultEventCap)
	assert.Contains(t, eventsJSON, fmt.Sprintf("event %d", total-1), "the newest event must survive truncation")
	assert.NotContains(t, eventsJSON, `"event 0"`, "the oldest event must be dropped by truncation")
}

func TestSupportBundleDefaultFilenameUsesClock(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	fixed := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	t.Chdir(dir)

	factory := &fakeFactory{local: newFakeClient()}
	res, err := execBundle(t, factory,
		input{s: map[string]string{flagClusters: "local", flagTargets: "webhook"}},
		func(cmd *supportBundleCmd) { cmd.now = func() time.Time { return fixed } })
	require.NoError(t, err)
	data := res.Data.(bundleResult)
	assert.Equal(t, fmt.Sprintf("rancher-support-%d.tar.gz", fixed.Unix()), data.Path)
}

// bundleWithAgentPod runs support-bundle for the agent target on one
// downstream whose single pod agent-1 has one container, main, and returns
// the result plus the archive path.
func bundleWithAgentPod(t *testing.T, downstream *fakeClient) (runtime.Result, string) {
	t.Helper()
	local := newFakeClient().withObject(clusterKind.GVR, "", "c-1", &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream.withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory := &fakeFactory{local: local, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagFilename: path,
		flagTargets:  "agent",
		flagClusters: "c-1",
	}})
	require.NoError(t, err)
	return res, path
}

// TestSupportBundleDeniedCurrentLogIsAnError asserts that a current
// container log the server refuses is counted: the target's manifest item
// is an error naming the container, the result's errors count includes it,
// a warning names it, and the archive carries no log file for it. The
// previous log is not requested for a container whose current log failed.
func TestSupportBundleDeniedCurrentLogIsAnError(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLogErr("main", clierr.New(clierr.CodeRBAC, "pods/log is forbidden"))
	res, path := bundleWithAgentPod(t, downstream)

	item := findItem(t, manifestFrom(t, path), "agent", "c-1")
	assert.True(t, strings.HasPrefix(item.Status, "error: 1 of 1 container logs not collected; agent-1/main: "), item.Status)
	assert.Contains(t, item.Status, "pods/log is forbidden")
	assert.Equal(t, 1, res.Data.(bundleResult).Errors)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "support-bundle: c-1/agent: log not collected: agent-1/main: pods/log is forbidden", res.Warnings[0])

	contents := readArchive(t, path)
	_, hasLog := contents["logs/c-1/agent/agent-1-main.log"]
	assert.False(t, hasLog, "a refused log must not appear in the archive")
	require.Len(t, downstream.logOptsCalls, 1, "the previous log is not requested after the current one failed")
	assert.False(t, downstream.logOptsCalls[0].Previous)
}

// TestSupportBundlePreviousLogAbsenceStaysSilent asserts that a previous
// container log the server does not have (any non-refusal error) is the
// common no-restart case: collected status, no error, no warning.
func TestSupportBundlePreviousLogAbsenceStaysSilent(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLog("main", "hello\n").
		withPreviousLogErr("main", clierr.New(clierr.CodeValidation, "previous terminated container \"main\" in pod \"agent-1\" not found"))
	res, path := bundleWithAgentPod(t, downstream)

	assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "agent", "c-1").Status)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	assert.Empty(t, res.Warnings)
	contents := readArchive(t, path)
	assert.Equal(t, "hello\n", contents["logs/c-1/agent/agent-1-main.log"])
	_, hasPrevious := contents["logs/c-1/agent/agent-1-main-previous.log"]
	assert.False(t, hasPrevious)
}

// TestSupportBundlePreviousLogRefusedIsAWarning asserts that a previous
// container log the server refuses (RBAC) is reported as a warning while
// the target, whose current log was collected, stays collected.
func TestSupportBundlePreviousLogRefusedIsAWarning(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLog("main", "hello\n").
		withPreviousLogErr("main", clierr.New(clierr.CodeRBAC, "pods/log is forbidden"))
	res, path := bundleWithAgentPod(t, downstream)

	assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "agent", "c-1").Status)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "support-bundle: c-1/agent: previous container log refused: agent-1/main: pods/log is forbidden", res.Warnings[0])
}
