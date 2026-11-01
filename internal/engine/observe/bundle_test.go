package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func execBundle(t *testing.T, clients *fakeFactory, in input, opts ...func(*supportBundleCmd)) (runtime.Result, error) {
	t.Helper()
	cmd := NewSupportBundle(clients, clients, func() any { return map[string]string{"cliVersion": "v9.9.9"} }).(*supportBundleCmd)
	for _, opt := range opts {
		opt(cmd)
	}
	res, err := cmd.Execute(t.Context(), in, nil)
	if path := in.s[flagOutputFile]; err == nil && path != "" {
		requireLocalNames(t, tarEntryNames(t, path))
	}
	return res, err
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
		flagOutputFile: path,
		flagTargets:    "server,agent",
		flagClusters:   "local,c-1",
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
	root := (&fakeRoot{}).withCluster(nonACECluster)
	downstream := newFakeClient()
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagOutputFile: path,
		flagTargets:    "kube-api-auth",
		flagClusters:   "c-1",
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
	root := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream := newFakeClient().withListErr(podKind.GVR, "cattle-system", errors.New("rbac denied"))
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagOutputFile: path,
		flagTargets:    "agent",
		flagClusters:   "c-1",
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
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "", flagClusters: "local"}})
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
		withListNS(provKind.GVR, "fleet-default", provList).
		withObject(fleetKind.GVR, "fleet-default", "p-1", fleetCluster)
	downstream := newFakeClient().
		withListNS(podKind.GVR, "custom-ns", podList("fleet-agent-1")).
		withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{}).
		withListNS(eventKind.GVR, "custom-ns", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "ev-1"}, "message": "crashlooping"}},
		}})
	factory := &fakeFactory{local: local, root: (&fakeRoot{}).withCluster(mgmtCluster), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "fleet-agent", flagClusters: "c-1"}})
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
	root := (&fakeRoot{}).withError(steve.Request{Type: steve.ClustersType}.Path(), errors.New("rbac denied listing clusters"))
	factory := &fakeFactory{local: newFakeClient(), root: root}

	path := filepath.Join(dir, "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "webhook"}})
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

	root := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream := newFakeClient().withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{Items: items})
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	path := filepath.Join(dir, "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "", flagClusters: "c-1"}})
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
	root := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream.withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagOutputFile: path,
		flagTargets:    "agent",
		flagClusters:   "c-1",
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
	_, hasLog := contents["logs/c-1/agent/agent-1/main.log"]
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
	assert.Equal(t, "hello\n", contents["logs/c-1/agent/agent-1/main.log"])
	_, hasPrevious := contents["logs/c-1/agent/agent-1/previous/main.log"]
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

// TestSupportBundleClusterOwnerCollectsClusterJSON asserts that
// cluster.json for a cluster the caller owns is built from the steve
// object while the local client refuses every management Cluster read.
func TestSupportBundleClusterOwnerCollectsClusterJSON(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory, local := ownerFactory(downstream)

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent", flagClusters: "prod"}})
	require.NoError(t, err)

	assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "cluster.json", "c-abc").Status)
	assert.JSONEq(t, `{"state":"active","conditions":null}`, readArchive(t, path)["clusters/c-abc/cluster.json"])
	assert.NotContains(t, local.kindReads, clusterKind.GVR, "no management Cluster read may reach the local client")
}

// TestSupportBundleClusterJSONSteveFailureIsManifestError asserts that a
// failed steve read of the Cluster object writes an error entry for
// cluster.json and the bundle still ships.
func TestSupportBundleClusterJSONSteveFailureIsManifestError(t *testing.T) {
	t.Parallel()
	factory, _ := ownerFactory(newFakeClient())
	factory.root.withError(clusterPath("c-abc"), rootStatusError(500, clusterPath("c-abc")))

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent", flagClusters: "prod"}})
	require.NoError(t, err)

	assert.Contains(t, findItem(t, manifestFrom(t, path), "cluster.json", "c-abc").Status, "error: ")
	assert.NotContains(t, readArchive(t, path), "clusters/c-abc/cluster.json")
}

// TestSupportBundleDiscoveryListsSteveClusters asserts that default
// discovery collects local plus every cluster steve lists, with no
// management Cluster read on the local client.
func TestSupportBundleDiscoveryListsSteveClusters(t *testing.T) {
	t.Parallel()
	factory, local := ownerFactory(newFakeClient())

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent"}})
	require.NoError(t, err)

	m := manifestFrom(t, path)
	assert.Equal(t, statusCollected, findItem(t, m, "cluster.json", "c-abc").Status)
	assert.NotContains(t, local.kindReads, clusterKind.GVR)
}

// TestParseTargetsFlag asserts what --targets accepts and how it refuses:
// the whole value empty or all selects every target; a list is split on
// commas with each token trimmed and empty tokens dropped; an unknown token
// is refused quoted, and a token all inside a list is refused with its own
// text.
func TestParseTargetsFlag(t *testing.T) {
	t.Parallel()
	names := func(ts []Target) []string {
		var out []string
		for _, tg := range ts {
			out = append(out, tg.Name)
		}
		return out
	}
	for _, raw := range []string{"", "all"} {
		got, err := parseTargetsFlag(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, Names(), names(got), raw)
	}
	got, err := parseTargetsFlag("agent, server")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent", "server"}, names(got))
	got, err = parseTargetsFlag(",")
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = parseTargetsFlag("agent,agent")
	require.NoError(t, err)
	assert.Equal(t, []string{"agent"}, names(got), "a target named twice is used once")
	got, err = parseTargetsFlag("server,agent,server, agent")
	require.NoError(t, err)
	assert.Equal(t, []string{"server", "agent"}, names(got), "the order of first appearance is kept")

	oneOf := "all|" + runtime.OneOf(Names())
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"x", `--targets="x" is not one of ` + oneOf},
		{"agent,x", `--targets="x" is not one of ` + oneOf},
		{"x,all", `--targets="x" is not one of ` + oneOf},
		{"all,x", "--targets: pass all by itself, or list targets without all"},
		{"all,agent", "--targets: pass all by itself, or list targets without all"},
		{"agent,all", "--targets: pass all by itself, or list targets without all"},
		{"all,all", "--targets: pass all by itself, or list targets without all"},
		{" all", "--targets: pass all by itself, or list targets without all"},
		{"all,", "--targets: pass all by itself, or list targets without all"},
	} {
		_, err := parseTargetsFlag(tc.raw)
		require.EqualError(t, err, tc.want, tc.raw)
		assert.Equal(t, clierr.CodeValidation, clierrCode(t, err), tc.raw)
	}
}

// TestSupportBundleRefusesOutputFileDash asserts that --output-file - is
// a VALIDATION error with the exact message, refused before any cluster
// client is built, and that no file named - is created.
func TestSupportBundleRefusesOutputFileDash(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	t.Chdir(t.TempDir())

	factory := &fakeFactory{local: newFakeClient()}
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: "-", flagTargets: "webhook", flagClusters: "local"}})
	require.EqualError(t, err, `--output-file "-": support-bundle writes the archive to a file, not to stdout; pass a file path`)
	assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
	assert.Empty(t, factory.clusterCalls)
	assert.NoFileExists(t, "-")
}

// TestSupportBundleWritesOutputFileDotDash asserts that ./- is a file path
// like any other: only the exact value - is refused.
func TestSupportBundleWritesOutputFileDotDash(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	t.Chdir(t.TempDir())

	factory := &fakeFactory{local: newFakeClient()}
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: "./-", flagTargets: "webhook", flagClusters: "local"}})
	require.NoError(t, err)
	assert.FileExists(t, "-")
}

// TestSupportBundleRefusesExistingOutputFile asserts that a path that is
// already taken (a file, a directory, a symlink whether or not it dangles)
// is refused with the exact message before any cluster client is built and
// before any request, and that the existing file is left as it was.
func TestSupportBundleRefusesExistingOutputFile(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	dir := t.TempDir()
	t.Chdir(dir)

	require.NoError(t, os.WriteFile("taken.tar.gz", []byte("pre-existing"), 0600))
	require.NoError(t, os.Mkdir("taken-dir", 0700))
	require.NoError(t, os.Symlink("nowhere", "dangling.tar.gz"))
	require.NoError(t, os.Symlink("taken.tar.gz", "link.tar.gz"))

	for _, path := range []string{"taken.tar.gz", "taken-dir", "dangling.tar.gz", "link.tar.gz"} {
		t.Run(path, func(t *testing.T) {
			factory := &fakeFactory{local: newFakeClient(), root: &fakeRoot{}}
			_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "webhook"}})
			require.EqualError(t, err, path+" already exists; choose a different --output-file path, or remove the existing file")
			assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
			assert.Empty(t, factory.clusterCalls)
			assert.Empty(t, factory.root.paths, "discovery must not send a request")
		})
	}

	b, err := os.ReadFile("taken.tar.gz")
	require.NoError(t, err)
	assert.Equal(t, "pre-existing", string(b))
}

// TestSupportBundleRefusesExistingDefaultName asserts that the default
// file name gets the same early refusal as a name passed with
// --output-file.
func TestSupportBundleRefusesExistingDefaultName(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	t.Chdir(t.TempDir())
	fixed := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	name := fmt.Sprintf("rancher-support-%d.tar.gz", fixed.Unix())
	require.NoError(t, os.WriteFile(name, []byte("pre-existing"), 0600))

	factory := &fakeFactory{local: newFakeClient(), root: &fakeRoot{}}
	_, err := execBundle(t, factory, input{s: map[string]string{flagTargets: "webhook"}},
		func(cmd *supportBundleCmd) { cmd.now = func() time.Time { return fixed } })
	require.EqualError(t, err, name+" already exists; choose a different --output-file path, or remove the existing file")
	assert.Equal(t, clierr.CodeValidation, clierrCode(t, err))
	assert.Empty(t, factory.clusterCalls)
	assert.Empty(t, factory.root.paths)
}

// TestSupportBundleMissingOutputDirIsNotRefusedEarly asserts that a path
// the early check cannot stat (a missing parent directory, or a parent
// that is a file) passes that check, so the error comes from opening the
// file, not from the existing-file refusal.
func TestSupportBundleMissingOutputDirIsNotRefusedEarly(t *testing.T) {
	// Not t.Parallel(): changes the process cwd.
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("plain-file", []byte("x"), 0600))

	for _, path := range []string{"missing/bundle.tar.gz", "plain-file/bundle.tar.gz"} {
		t.Run(path, func(t *testing.T) {
			factory := &fakeFactory{local: newFakeClient()}
			_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "webhook", flagClusters: "local"}})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "already exists")
			if path == "missing/bundle.tar.gz" {
				assert.ErrorIs(t, err, fs.ErrNotExist)
			}
		})
	}
}

// recordedClusterObservation reads the cluster object a Rancher v2.15.2
// served as the observation named name in the recorded golden, with every
// value that varies per run replaced by a placeholder.
func recordedClusterObservation(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testrancher", "fidelity", "testdata", "downstream",
		"access-downstream", "admin-steve-cluster-shape.json"))
	require.NoError(t, err)
	var golden struct {
		Observations []struct {
			Name   string         `json:"name"`
			Status int            `json:"status"`
			Body   map[string]any `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	for _, o := range golden.Observations {
		if o.Name == name {
			require.Equal(t, http.StatusOK, o.Status)
			return &unstructured.Unstructured{Object: o.Body}
		}
	}
	require.FailNow(t, "the golden has no observation named "+name)
	return nil
}

// TestSupportBundleClusterJSONStateIsWhatRancherServed asserts cluster.json
// carries the metadata.state name of the cluster object a Rancher served.
func TestSupportBundleClusterJSONStateIsWhatRancherServed(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory, _ := ownerFactory(downstream)
	served := recordedClusterObservation(t, "steve")
	served.SetName("c-abc")
	factory.root = (&fakeRoot{}).withCluster(served)

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	_, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent", flagClusters: "prod"}})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(readArchive(t, path)["clusters/c-abc/cluster.json"]), &doc))
	assert.Equal(t, "active", doc["state"])
}

// requireLocalNames asserts that every entry name of an archive stays inside
// the directory it is extracted to on the machine the test runs on: it is not
// empty, has no backslash, does not start with a slash and is a local path.
func requireLocalNames(t *testing.T, names []string) {
	t.Helper()
	for _, name := range names {
		assert.NotEmpty(t, name)
		assert.NotContains(t, name, `\`, "%q", name)
		assert.False(t, strings.HasPrefix(name, "/"), "%q starts with a slash", name)
		assert.True(t, filepath.IsLocal(filepath.FromSlash(name)), "%q is not a local path", name)
	}
}

func archiveNames(contents map[string]string) []string {
	return slices.Sorted(maps.Keys(contents))
}

func isErrorStatus(status string) bool { return strings.HasPrefix(status, "error:") }

// bundleOfClusters runs support-bundle for the agent target over every
// cluster in ids (all served by the steve root), with downstream pods only
// for c-1, and returns the factory, the result and the archive path.
func bundleOfClusters(t *testing.T, ids ...string) (*fakeFactory, runtime.Result, string) {
	t.Helper()
	root := &fakeRoot{}
	for _, id := range ids {
		root.withCluster(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": id}}})
	}
	downstream := newFakeClient().withLog("main", "hello\n").withListNS(podKind.GVR, "cattle-system", podList("agent-1"))
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "agent"}})
	require.NoError(t, err)
	return factory, res, path
}

// TestSupportBundleLeavesOutClusterWithUnsafeID asserts that a cluster id
// that is not a plain file name is left out whole: no request for it, no
// entry named after it, an error item named cluster, one warning that quotes
// the id and one more error in the count. The other cluster is collected.
func TestSupportBundleLeavesOutClusterWithUnsafeID(t *testing.T) {
	t.Parallel()
	_, baseline, _ := bundleOfClusters(t, "c-1")

	for _, tc := range []struct{ name, id, shown, absent string }{
		{"parent directory", "../x", `"../x"`, ""},
		{"slash", "a/b", `"a/b"`, ""},
		{"NUL", "a\x00b", `"a\x00b"`, "\x00"},
		{"dot dot", "..", `".."`, ""},
		{"newline", "a\nb", `"a\nb"`, "\n"},
		{"right-to-left override", "a\u202eb", `"a\u202eb"`, "\u202e"},
		{"word joiner", "a\u2060b", `"a\u2060b"`, "\u2060"},
		{"over 64 bytes", strings.Repeat("a", 99) + "/b", `"` + strings.Repeat("a", 64) + `"...`, strings.Repeat("a", 65)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			factory, res, path := bundleOfClusters(t, "c-1", tc.id)

			contents := readArchive(t, path)
			for _, name := range archiveNames(contents) {
				assert.NotContains(t, name, tc.id, "an entry is named after the left-out cluster id")
			}
			requireLocalNames(t, archiveNames(contents))

			m := manifestFrom(t, path)
			item := findItem(t, m, "cluster", tc.id)
			assert.True(t, isErrorStatus(item.Status), item.Status)
			for _, it := range m.Items {
				if it.Cluster == tc.id {
					assert.Equal(t, "cluster", it.Name, "the left-out cluster has no other item")
				}
			}
			assert.Equal(t, statusCollected, findItem(t, m, "cluster.json", "c-1").Status)
			assert.Equal(t, statusCollected, findItem(t, m, "nodes.json", "c-1").Status)
			assert.Equal(t, statusCollected, findItem(t, m, "agent", "c-1").Status)
			assert.Contains(t, contents, "logs/c-1/agent/agent-1/main.log")

			assert.Equal(t, baseline.Data.(bundleResult).Errors+1, res.Data.(bundleResult).Errors)
			var shown []string
			for _, w := range res.Warnings {
				if strings.Contains(w, tc.shown) {
					shown = append(shown, w)
				}
			}
			require.Len(t, shown, 1, "one warning shows the cluster id escaped and cut at 64 bytes: %q", res.Warnings)
			if tc.absent != "" {
				assert.NotContains(t, shown[0], tc.absent, "the warning holds the escaped or cut text, not the raw value")
			}
			assert.NotContains(t, factory.clusterCalls, tc.id, "no request is made for a left-out cluster")
		})
	}
}

// fleetAgentBundleFactory serves one downstream cluster c-1 whose fleet
// Cluster p-1 names agentNamespace, with a pod and an event list in that
// namespace and an empty event list in cattle-system.
func fleetAgentBundleFactory(agentNamespace string) (*fakeFactory, *fakeClient) {
	mgmtCluster := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c-1"}, "spec": map[string]any{}}}
	provList := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "p-1"}, "status": map[string]any{"clusterName": "c-1"}}},
	}}
	fleetCluster := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-1"},
		"spec":     map[string]any{"agentNamespace": agentNamespace},
	}}
	local := newFakeClient().
		withListNS(provKind.GVR, "fleet-default", provList).
		withObject(fleetKind.GVR, "fleet-default", "p-1", fleetCluster)
	downstream := newFakeClient().
		withListNS(podKind.GVR, agentNamespace, podList("fleet-agent-1")).
		withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{}).
		withListNS(eventKind.GVR, agentNamespace, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "ev-1"}, "message": "crashlooping"}},
		}})
	factory := &fakeFactory{local: local, root: (&fakeRoot{}).withCluster(mgmtCluster), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	return factory, downstream
}

// TestSupportBundleLeavesOutEventsOfUnsafeNamespace asserts that a namespace
// the fleet-agent target resolved to, which is not a plain file name inside
// the events file name, gets no entry and no events request, shows as an
// error item and one warning, and leaves the other namespaces collected.
func TestSupportBundleLeavesOutEventsOfUnsafeNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, ns, shown, absent string }{
		{"slash", "a/b", `"events-a/b.json"`, ""},
		{"NUL", "a\x00b", `"events-a\x00b.json"`, "\x00"},
		{"newline", "a\nb", `"events-a\nb.json"`, "\n"},
		{"right-to-left override", "a\u202eb", `"events-a\u202eb.json"`, "\u202e"},
		{"word joiner", "a\u2060b", `"events-a\u2060b.json"`, "\u2060"},
		{"over 64 bytes", strings.Repeat("a", 99) + "/b", `"events-` + strings.Repeat("a", 57) + `"...`, strings.Repeat("a", 58)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			factory, downstream := fleetAgentBundleFactory(tc.ns)
			path := filepath.Join(t.TempDir(), "bundle.tar.gz")
			res, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "fleet-agent", flagClusters: "c-1"}})
			require.NoError(t, err)

			contents := readArchive(t, path)
			for _, name := range archiveNames(contents) {
				assert.NotContains(t, name, tc.ns, "an entry is named after the left-out namespace")
			}
			requireLocalNames(t, archiveNames(contents))
			assert.Contains(t, contents, "clusters/c-1/events-cattle-system.json")

			m := manifestFrom(t, path)
			assert.True(t, isErrorStatus(findItem(t, m, "events-"+tc.ns, "c-1").Status))
			assert.Equal(t, statusCollected, findItem(t, m, "events-cattle-system", "c-1").Status)
			assert.Equal(t, 1, res.Data.(bundleResult).Errors)

			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], tc.shown)
			if tc.absent != "" {
				assert.NotContains(t, res.Warnings[0], tc.absent, "the warning holds the escaped or cut text, not the raw value")
			}

			assert.NotContains(t, downstream.listedNamespaces(eventKind.GVR), tc.ns, "no events request for a left-out namespace")
		})
	}
}

// TestSupportBundleWritesEventsOfDotDotNamespace asserts that the rule
// applies to the whole file name: a namespace of two dots inside
// events-<namespace>.json is a plain name and is written.
func TestSupportBundleWritesEventsOfDotDotNamespace(t *testing.T) {
	t.Parallel()
	factory, _ := fleetAgentBundleFactory("..")
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: "fleet-agent", flagClusters: "c-1"}})
	require.NoError(t, err)

	contents := readArchive(t, path)
	assert.Contains(t, contents["clusters/c-1/events-...json"], "crashlooping")
	requireLocalNames(t, archiveNames(contents))
	assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "events-..", "c-1").Status)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	assert.Empty(t, res.Warnings)
}

// bundleWithAgentPods runs support-bundle for the agent target on one
// downstream cluster c-1 whose cattle-system lists pods, and returns the
// result and the archive path.
func bundleWithAgentPods(t *testing.T, downstream *fakeClient, pods ...unstructured.Unstructured) (runtime.Result, string) {
	t.Helper()
	root := (&fakeRoot{}).withCluster(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c-1"},
	}})
	downstream.withListNS(podKind.GVR, "cattle-system", &unstructured.UnstructuredList{Items: pods})
	factory := &fakeFactory{local: newFakeClient(), root: root, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{
		flagOutputFile: path,
		flagTargets:    "agent",
		flagClusters:   "c-1",
	}})
	require.NoError(t, err)
	return res, path
}

// TestSupportBundleLeavesOutLogsOfUnsafePodOrContainerName asserts that a
// container whose pod or container name makes an entry name that is not a
// plain file name gets neither its current nor its previous log, and no log
// request. It counts once as a failed container and one warning names it;
// the other container is collected.
func TestSupportBundleLeavesOutLogsOfUnsafePodOrContainerName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		pods    []unstructured.Unstructured
		hostile string
		shown   string
		absent  string
	}{
		{"container parent directory", []unstructured.Unstructured{unstructuredPodWithContainers("agent-1", "main", "../x")}, "../x", `"../x.log"`, ""},
		{"container newline", []unstructured.Unstructured{unstructuredPodWithContainers("agent-1", "main", "a\nb")}, "a\nb", `"a\nb.log"`, "\n"},
		{"container word joiner", []unstructured.Unstructured{unstructuredPodWithContainers("agent-1", "main", "a\u2060b")}, "a\u2060b", `"a\u2060b.log"`, "\u2060"},
		{"pod parent directory", []unstructured.Unstructured{unstructuredPod("agent-1"), unstructuredPod("../x")}, "../x", `"../x"`, ""},
		{"pod newline", []unstructured.Unstructured{unstructuredPod("agent-1"), unstructuredPod("a\nb")}, "a\nb", `"a\nb"`, "\n"},
		{"pod right-to-left override", []unstructured.Unstructured{unstructuredPod("agent-1"), unstructuredPod("a\u202eb")}, "a\u202eb", `"a\u202eb"`, "\u202e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			downstream := newFakeClient().withLog("main", "hello\n")
			res, path := bundleWithAgentPods(t, downstream, tc.pods...)

			contents := readArchive(t, path)
			for _, name := range archiveNames(contents) {
				assert.NotContains(t, name, tc.hostile)
			}
			requireLocalNames(t, archiveNames(contents))
			assert.Equal(t, "hello\n", contents["logs/c-1/agent/agent-1/main.log"])
			assert.Contains(t, contents, "logs/c-1/agent/agent-1/previous/main.log")

			require.Len(t, downstream.logOptsCalls, 2, "only the other container's current and previous log are requested")
			for _, call := range downstream.logOptsCalls {
				assert.Equal(t, "main", call.Container)
			}

			item := findItem(t, manifestFrom(t, path), "agent", "c-1")
			assert.True(t, strings.HasPrefix(item.Status, "error: 1 of 2 container logs not collected; "), item.Status)
			assert.Equal(t, 1, res.Data.(bundleResult).Errors)
			require.Len(t, res.Warnings, 1)
			assert.Contains(t, res.Warnings[0], "left out")
			assert.Contains(t, res.Warnings[0], tc.shown)
			assert.Contains(t, item.Status, tc.shown)
			if tc.absent != "" {
				assert.NotContains(t, res.Warnings[0], tc.absent, "the warning holds the escaped text, not the raw value")
				assert.NotContains(t, item.Status, tc.absent)
			}
		})
	}
}

// TestSupportBundleLeavesOutPodWithReservedWindowsName asserts that a pod
// name that starts with a device name Windows reserves is left out, with the
// reason in the item.
func TestSupportBundleLeavesOutPodWithReservedWindowsName(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLog("main", "hello\n")
	res, path := bundleWithAgentPods(t, downstream, unstructuredPod("aux.example"))

	assert.Empty(t, downstream.logOptsCalls)
	item := findItem(t, manifestFrom(t, path), "agent", "c-1")
	assert.Contains(t, item.Status, "reserved on Windows")
	assert.Equal(t, 1, res.Data.(bundleResult).Errors)
}

// TestSupportBundleNULInPodNameKeepsTheBundle asserts that a NUL byte in a
// pod name leaves the bundle written: the command succeeds and the archive
// has its manifest.
func TestSupportBundleNULInPodNameKeepsTheBundle(t *testing.T) {
	t.Parallel()
	downstream := newFakeClient().withLog("main", "hello\n")
	res, path := bundleWithAgentPods(t, downstream, unstructuredPod("agent-1"), unstructuredPod("a\x00b"))

	contents := readArchive(t, path)
	assert.Contains(t, contents, "manifest.json")
	assert.Contains(t, contents, "logs/c-1/agent/agent-1/main.log")
	requireLocalNames(t, archiveNames(contents))
	assert.Equal(t, 1, res.Data.(bundleResult).Errors)
}

// TestSupportBundleReadmeExplainsLeftOutEntries asserts that the archive's
// README.txt says an entry with an unsafe name is left out and where that
// shows.
func TestSupportBundleReadmeExplainsLeftOutEntries(t *testing.T) {
	t.Parallel()
	_, _, path := bundleOfClusters(t, "c-1")

	readme := readArchive(t, path)["README.txt"]
	assert.Contains(t, readme, "is left out")
	assert.Contains(t, readme, "listed as the item")
	assert.Contains(t, readme, "manifest.json shows each as an error")
}

// TestSupportBundleReadmeExplainsTheLogLayout asserts that the archive's
// README.txt names the file of a container's current log and of its previous
// log, and says an entry whose name was already written is left out.
func TestSupportBundleReadmeExplainsTheLogLayout(t *testing.T) {
	t.Parallel()
	_, _, path := bundleOfClusters(t, "c-1")

	readme := strings.Join(strings.Fields(readArchive(t, path)["README.txt"]), " ")
	assert.Contains(t, readme, "Each pod has its own directory under logs/<cluster>/<target>/.")
	assert.Contains(t, readme, "The current log of a container is <pod>/<container>.log")
	assert.Contains(t, readme, "the log of its previous run is <pod>/previous/<container>.log")
	assert.Contains(t, readme, "An entry whose name was already written is left out")
}

// failingPreviousLogClient fails the archive write that follows a previous
// log read by breaking the writer when that log is requested.
type failingPreviousLogClient struct {
	*fakeClient
	bw *ArchiveWriter
}

func (c *failingPreviousLogClient) Logs(ctx context.Context, ns, pod string, opts engine.LogOptions) (string, error) {
	if opts.Previous {
		c.bw.err = errors.New("disk full")
	}
	return c.fakeClient.Logs(ctx, ns, pod, opts)
}

// TestCollectTargetLogsRecordsFailedPreviousLogWrite asserts that a previous
// log the archive could not take is a failed container: the target status is
// an error item and a warning names the write failure.
func TestCollectTargetLogsRecordsFailedPreviousLogWrite(t *testing.T) {
	t.Parallel()
	bw, err := CreateArchive(filepath.Join(t.TempDir(), "bundle.tar.gz"))
	require.NoError(t, err)
	t.Cleanup(bw.Discard)
	client := &failingPreviousLogClient{fakeClient: newFakeClient().withLog("main", "hello\n"), bw: bw}
	pod := unstructuredPod("agent-1")
	resolved := Resolved{Client: client, ClusterID: "c-1", Namespace: "cattle-system", Pods: []unstructured.Unstructured{pod}}

	outcome := collectTargetLogs(t.Context(), Target{Name: "agent"}, resolved, nil, nil, bw)

	assert.True(t, strings.HasPrefix(outcome.status(), "error: 1 of 1 container logs not collected; agent-1/main: archive write failed: disk full"), outcome.status())
	warnings := outcome.warnings("c-1", "agent")
	require.Len(t, warnings, 1)
	assert.Equal(t, "support-bundle: c-1/agent: log not collected: agent-1/main: archive write failed: disk full", warnings[0])
}

// TestArchiveEventsRecordsFailedWrite asserts that an events file the archive
// could not take is an error item with a warning, and that a namespace whose
// events were read but not written is not reported as collected.
func TestArchiveEventsRecordsFailedWrite(t *testing.T) {
	t.Parallel()
	bw, err := CreateArchive(filepath.Join(t.TempDir(), "bundle.tar.gz"))
	require.NoError(t, err)
	t.Cleanup(bw.Discard)
	bw.err = errors.New("disk full")
	downstream := newFakeClient().withListNS(eventKind.GVR, "cattle-system", &unstructured.UnstructuredList{})
	factory := &fakeFactory{local: newFakeClient(), perCluster: map[string]engine.ClusterClient{"c-1": downstream}}

	items, warnings := archiveEvents(t.Context(), factory, "c-1", []string{"cattle-system"}, bw)

	require.Len(t, items, 1)
	assert.Equal(t, "events-cattle-system", items[0].Name)
	assert.Equal(t, "error: disk full", items[0].Status)
	assert.Equal(t, []string{`support-bundle: events of cluster "c-1" namespace "cattle-system" not written: disk full`}, warnings)
}

// downstreamWithPodLogs returns a downstream fake that serves, for every
// container of every pod, the current log "log of <pod>/<container>" and the
// previous log "previous log of <pod>/<container>".
func downstreamWithPodLogs(pods ...unstructured.Unstructured) *fakeClient {
	f := newFakeClient()
	for i := range pods {
		pod := pods[i].GetName()
		for _, container := range engine.ContainerNames(pods[i].Object) {
			f.withPodLog(pod, container, "log of "+pod+"/"+container).
				withPreviousPodLog(pod, container, "previous log of "+pod+"/"+container)
		}
	}
	return f
}

// logEntryNames returns the names under logs/ in the order the archive holds
// them, a repeated name included.
func logEntryNames(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, name := range tarEntryNames(t, path) {
		if strings.HasPrefix(name, "logs/") {
			out = append(out, name)
		}
	}
	return out
}

// requireNoRepeat asserts that no name appears twice in names.
func requireNoRepeat(t *testing.T, names []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, name := range names {
		assert.False(t, seen[strings.ToLower(name)], "the archive holds %q twice (ignoring letter case)", name)
		seen[strings.ToLower(name)] = true
	}
}

// wantLog is one log entry an archive must hold: its name and its text.
type wantLog struct{ name, text string }

// wantLogs lists, for one pod and its containers, the entries
// downstreamWithPodLogs serves: per container the current log, then the
// previous log.
func wantLogs(pod string, containers ...string) []wantLog {
	var out []wantLog
	for _, c := range containers {
		out = append(out,
			wantLog{"logs/c-1/agent/" + pod + "/" + c + ".log", "log of " + pod + "/" + c},
			wantLog{"logs/c-1/agent/" + pod + "/previous/" + c + ".log", "previous log of " + pod + "/" + c})
	}
	return out
}

// TestSupportBundleNamesLogsByPodDirectory asserts the entry names of the
// logs of the pods "a" and "a-b": each pod has a directory, the current log
// is <container>.log in it and the previous log is previous/<container>.log.
func TestSupportBundleNamesLogsByPodDirectory(t *testing.T) {
	t.Parallel()
	pods := []unstructured.Unstructured{unstructuredPodWithContainers("a", "b-c"), unstructuredPodWithContainers("a-b", "c")}
	res, path := bundleWithAgentPods(t, downstreamWithPodLogs(pods...), pods...)

	assert.Equal(t, []string{
		"logs/c-1/agent/a/b-c.log",
		"logs/c-1/agent/a/previous/b-c.log",
		"logs/c-1/agent/a-b/c.log",
		"logs/c-1/agent/a-b/previous/c.log",
	}, logEntryNames(t, path))
	contents := readArchive(t, path)
	assert.Equal(t, "log of a/b-c", contents["logs/c-1/agent/a/b-c.log"])
	assert.Equal(t, "previous log of a/b-c", contents["logs/c-1/agent/a/previous/b-c.log"])
	assert.Equal(t, "log of a-b/c", contents["logs/c-1/agent/a-b/c.log"])
	assert.Equal(t, "previous log of a-b/c", contents["logs/c-1/agent/a-b/previous/c.log"])
	assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "agent", "c-1").Status)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	assert.Empty(t, res.Warnings)
	t.Logf("entry names: %q", logEntryNames(t, path))
}

// TestSupportBundleGivesEveryPairOfNamesItsOwnEntry asserts that pods and
// containers whose names, joined with a "-", would make one file name get one
// entry each, with their own text, in pod and container order.
func TestSupportBundleGivesEveryPairOfNamesItsOwnEntry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pods []unstructured.Unstructured
		want []wantLog
	}{
		{
			"pod a with container b-c and pod a-b with container c",
			[]unstructured.Unstructured{unstructuredPodWithContainers("a", "b-c"), unstructuredPodWithContainers("a-b", "c")},
			slices.Concat(wantLogs("a", "b-c"), wantLogs("a-b", "c")),
		},
		{
			"containers x and x-previous of one pod",
			[]unstructured.Unstructured{unstructuredPodWithContainers("p", "x", "x-previous")},
			wantLogs("p", "x", "x-previous"),
		},
		{
			"containers b and b-previous of pod a and container previous of pod a-b",
			[]unstructured.Unstructured{unstructuredPodWithContainers("a", "b", "b-previous"), unstructuredPodWithContainers("a-b", "previous")},
			slices.Concat(wantLogs("a", "b", "b-previous"), wantLogs("a-b", "previous")),
		},
		{
			"pod a.b next to pod a with container b-c",
			[]unstructured.Unstructured{unstructuredPodWithContainers("a", "b-c"), unstructuredPodWithContainers("a.b", "c")},
			slices.Concat(wantLogs("a", "b-c"), wantLogs("a.b", "c")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, path := bundleWithAgentPods(t, downstreamWithPodLogs(tc.pods...), tc.pods...)

			var wantNames []string
			for _, w := range tc.want {
				wantNames = append(wantNames, w.name)
			}
			assert.Equal(t, wantNames, logEntryNames(t, path), "one entry per log, in order, none twice")
			contents := readArchive(t, path)
			for _, w := range tc.want {
				assert.Equal(t, w.text, contents[w.name], w.name)
			}
			assert.Equal(t, statusCollected, findItem(t, manifestFrom(t, path), "agent", "c-1").Status)
			assert.Equal(t, 0, res.Data.(bundleResult).Errors)
			assert.Empty(t, res.Warnings)
		})
	}
}

// TestSupportBundleEveryPodAndContainerPairHasOneEntryName asserts that the
// entry names of the logs are a one-to-one map of the pairs of pod and
// container: for pods and containers whose names join in many ways, the logs
// have as many names as there are current and previous logs, none repeats,
// and every name reads back as exactly one pair.
func TestSupportBundleEveryPodAndContainerPairHasOneEntryName(t *testing.T) {
	t.Parallel()
	podNames := []string{"a", "a-b", "a.b", "b"}
	containers := []string{"b", "c", "b-c", "previous", "b-previous", "x", "x-previous"}
	var pods []unstructured.Unstructured
	for _, name := range podNames {
		pods = append(pods, unstructuredPodWithContainers(name, containers...))
	}
	res, path := bundleWithAgentPods(t, downstreamWithPodLogs(pods...), pods...)

	names := logEntryNames(t, path)
	requireNoRepeat(t, names)
	require.Len(t, names, 2*len(podNames)*len(containers))

	got := map[string]bool{}
	for _, name := range names {
		rest, ok := strings.CutPrefix(name, "logs/c-1/agent/")
		require.True(t, ok, name)
		parts := strings.Split(rest, "/")
		var pod, file string
		previous := false
		switch {
		case len(parts) == 2:
			pod, file = parts[0], parts[1]
		case len(parts) == 3 && parts[1] == "previous":
			pod, file, previous = parts[0], parts[2], true
		default:
			require.Fail(t, "an entry name with an unexpected shape", name)
		}
		container, ok := strings.CutSuffix(file, ".log")
		require.True(t, ok, name)
		got[fmt.Sprintf("%s|%s|%t", pod, container, previous)] = true
	}
	want := map[string]bool{}
	for _, pod := range podNames {
		for _, container := range containers {
			want[fmt.Sprintf("%s|%s|false", pod, container)] = true
			want[fmt.Sprintf("%s|%s|true", pod, container)] = true
		}
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
}

// TestSupportBundleLeavesOutLogWhoseEntryNameRepeats asserts what happens when
// the server sends two pods or containers that make one entry name (an
// altered reply: Kubernetes refuses both): the first entry stays, the second
// is left out and shown, and no previous log is requested for it.
func TestSupportBundleLeavesOutLogWhoseEntryNameRepeats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		pods      []unstructured.Unstructured
		setup     func(*fakeClient)
		wantNames []string
		wantText  string
		container string
		pod       string
		reason    string
	}{
		{
			name:  "two pods of one name",
			pods:  []unstructured.Unstructured{unstructuredPod("p"), unstructuredPod("p")},
			setup: func(f *fakeClient) { f.withLogSequence("p", "main", "first", "second") },
			wantNames: []string{
				"logs/c-1/agent/p/main.log",
				"logs/c-1/agent/p/previous/main.log",
			},
			wantText: "first",
			pod:      "p", container: "main",
			reason: `"logs/c-1/agent/p/main.log": archive entry name appears twice`,
		},
		{
			name:  "one container name twice in one pod",
			pods:  []unstructured.Unstructured{unstructuredPodWithContainers("p", "main", "main")},
			setup: func(f *fakeClient) { f.withLogSequence("p", "main", "first", "second") },
			wantNames: []string{
				"logs/c-1/agent/p/main.log",
				"logs/c-1/agent/p/previous/main.log",
			},
			wantText: "first",
			pod:      "p", container: "main",
			reason: `"logs/c-1/agent/p/main.log": archive entry name appears twice`,
		},
		{
			name:  "pods that differ in letter case",
			pods:  []unstructured.Unstructured{unstructuredPod("Pod-1"), unstructuredPod("pod-1")},
			setup: func(*fakeClient) {},
			wantNames: []string{
				"logs/c-1/agent/Pod-1/main.log",
				"logs/c-1/agent/Pod-1/previous/main.log",
			},
			wantText: "log of Pod-1/main",
			pod:      "pod-1", container: "main",
			reason: `"logs/c-1/agent/pod-1/main.log": archive entry name appears twice ignoring letter case; first written as "logs/c-1/agent/Pod-1/main.log"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			downstream := downstreamWithPodLogs(tc.pods...)
			tc.setup(downstream)
			res, path := bundleWithAgentPods(t, downstream, tc.pods...)

			assert.Equal(t, tc.wantNames, logEntryNames(t, path), "the first entry stays and the second is left out")
			assert.Equal(t, tc.wantText, readArchive(t, path)[tc.wantNames[0]], "the first entry keeps its text")

			left := fmt.Sprintf("pod %q container %q: log entry left out: %s", tc.pod, tc.container, tc.reason)
			item := findItem(t, manifestFrom(t, path), "agent", "c-1")
			assert.Equal(t, "error: 1 of 2 container logs not collected; "+left, item.Status)
			assert.Equal(t, 1, res.Data.(bundleResult).Errors)
			assert.Equal(t, []string{"support-bundle: c-1/agent: log not collected: " + left}, res.Warnings)

			var previous int
			for _, call := range downstream.logOptsCalls {
				if call.Previous {
					previous++
				}
			}
			assert.Equal(t, 1, previous, "a previous log is requested for the container that was kept, not the one left out")
			assert.Len(t, downstream.logOptsCalls, 3, "the repeated log is requested once and refused")
			t.Logf("target status: %q; warning: %q", item.Status, res.Warnings)
		})
	}
}

// TestSupportBundleWritesPodNameOf253Bytes asserts that a pod name of the
// longest length Kubernetes allows is a whole directory name: the current and
// the previous log are written with no error.
func TestSupportBundleWritesPodNameOf253Bytes(t *testing.T) {
	t.Parallel()
	pod := strings.Repeat("a", 253)
	res, path := bundleWithAgentPods(t, downstreamWithPodLogs(unstructuredPod(pod)), unstructuredPod(pod))

	assert.Equal(t, []string{
		"logs/c-1/agent/" + pod + "/main.log",
		"logs/c-1/agent/" + pod + "/previous/main.log",
	}, logEntryNames(t, path))
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	assert.Empty(t, res.Warnings)
}

// TestSupportBundleLeavesOutPodNameOf256Bytes asserts that a pod name over
// 255 bytes is left out with the reason, before any log request.
func TestSupportBundleLeavesOutPodNameOf256Bytes(t *testing.T) {
	t.Parallel()
	pod := strings.Repeat("a", 256)
	downstream := downstreamWithPodLogs(unstructuredPod(pod))
	res, path := bundleWithAgentPods(t, downstream, unstructuredPod(pod))

	assert.Empty(t, logEntryNames(t, path))
	assert.Empty(t, downstream.logOptsCalls, "no log request, current or previous")
	item := findItem(t, manifestFrom(t, path), "agent", "c-1")
	assert.True(t, strings.HasPrefix(item.Status, "error: 1 of 1 container logs not collected; "), item.Status)
	assert.Contains(t, item.Status, "longer than 255 bytes")
	assert.Contains(t, item.Status, `pod "`+strings.Repeat("a", 64)+`"... container "main": log entry left out`)
	assert.Equal(t, 1, res.Data.(bundleResult).Errors)
	require.Len(t, res.Warnings, 1)
}

// TestSupportBundleLeavesOutPodOrContainerNamedLikeAWindowsDevice asserts
// that a pod is a whole directory name and a container a whole file name
// stem: a pod named nul and a container named aux are left out with the
// reason and no log request, on every operating system.
func TestSupportBundleLeavesOutPodOrContainerNamedLikeAWindowsDevice(t *testing.T) {
	t.Parallel()
	t.Run("pod nul", func(t *testing.T) {
		t.Parallel()
		pods := []unstructured.Unstructured{unstructuredPod("nul")}
		downstream := downstreamWithPodLogs(pods...)
		res, path := bundleWithAgentPods(t, downstream, pods...)

		assert.Empty(t, logEntryNames(t, path))
		assert.Empty(t, downstream.logOptsCalls, "no log request for the pod")
		item := findItem(t, manifestFrom(t, path), "agent", "c-1")
		assert.Contains(t, item.Status, `pod "nul" container "main": log entry left out: "nul" is not a plain file name: it starts with a name reserved on Windows`)
		assert.Equal(t, 1, res.Data.(bundleResult).Errors)
	})
	t.Run("container aux", func(t *testing.T) {
		t.Parallel()
		pods := []unstructured.Unstructured{unstructuredPodWithContainers("agent-1", "main", "aux")}
		downstream := downstreamWithPodLogs(pods...)
		res, path := bundleWithAgentPods(t, downstream, pods...)

		assert.Equal(t, []string{"logs/c-1/agent/agent-1/main.log", "logs/c-1/agent/agent-1/previous/main.log"}, logEntryNames(t, path))
		require.Len(t, downstream.logOptsCalls, 2, "only the other container's current and previous log are requested")
		for _, call := range downstream.logOptsCalls {
			assert.Equal(t, "main", call.Container)
		}
		item := findItem(t, manifestFrom(t, path), "agent", "c-1")
		assert.Contains(t, item.Status, `pod "agent-1" container "aux": log entry left out: "aux.log" is not a plain file name: it starts with a name reserved on Windows`)
		assert.Equal(t, 1, res.Data.(bundleResult).Errors)
	})
}

// bundleWithFlags runs support-bundle with the given --clusters and
// --targets over steve clusters ids. Cluster c-1 has the pod agent-1 with its
// logs; resolve maps a cluster name to an id.
func bundleWithFlags(t *testing.T, ids []string, resolve map[string]string, clusters, targets string) (runtime.Result, string) {
	t.Helper()
	root := &fakeRoot{}
	for _, id := range ids {
		root.withCluster(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": id}}})
	}
	pod := unstructuredPod("agent-1")
	downstream := downstreamWithPodLogs(pod).withListNS(podKind.GVR, "cattle-system", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{pod}})
	factory := &fakeFactory{local: newFakeClient(), root: root, resolve: resolve, perCluster: map[string]engine.ClusterClient{"c-1": downstream}}
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	res, err := execBundle(t, factory, input{s: map[string]string{flagOutputFile: path, flagTargets: targets, flagClusters: clusters}})
	require.NoError(t, err)
	return res, path
}

// requireEachItemOnce asserts that no manifest item name and cluster pair
// appears twice.
func requireEachItemOnce(t *testing.T, m bundleManifest) {
	t.Helper()
	seen := map[string]bool{}
	for _, it := range m.Items {
		key := it.Name + " on " + it.Cluster
		assert.False(t, seen[key], "the manifest lists %q twice", key)
		seen[key] = true
	}
}

// TestSupportBundleUsesAClusterNamedTwiceOnce asserts that a cluster named
// twice in --clusters, by id or by name and id, is collected once: each entry
// and each manifest item appears once and nothing is an error.
func TestSupportBundleUsesAClusterNamedTwiceOnce(t *testing.T) {
	t.Parallel()
	for _, clusters := range []string{"c-1,c-1", "prod,c-1", "c-1,prod,c-1"} {
		t.Run(clusters, func(t *testing.T) {
			t.Parallel()
			res, path := bundleWithFlags(t, []string{"c-1"}, map[string]string{"prod": "c-1"}, clusters, "agent")

			names := tarEntryNames(t, path)
			requireNoRepeat(t, names)
			assert.Contains(t, names, "logs/c-1/agent/agent-1/main.log")
			assert.Contains(t, names, "clusters/c-1/cluster.json")
			m := manifestFrom(t, path)
			requireEachItemOnce(t, m)
			assert.Equal(t, statusCollected, findItem(t, m, "agent", "c-1").Status)
			assert.Equal(t, 0, res.Data.(bundleResult).Errors)
			assert.Empty(t, res.Warnings)
			t.Logf("--clusters %s: entries %q; manifest items %+v", clusters, names, m.Items)
		})
	}
}

// TestSupportBundleUsesATargetNamedTwiceOnce asserts that a target named twice
// in --targets is collected once.
func TestSupportBundleUsesATargetNamedTwiceOnce(t *testing.T) {
	t.Parallel()
	res, path := bundleWithFlags(t, []string{"c-1"}, nil, "c-1", "agent,agent")

	names := tarEntryNames(t, path)
	requireNoRepeat(t, names)
	assert.Equal(t, []string{"logs/c-1/agent/agent-1/main.log", "logs/c-1/agent/agent-1/previous/main.log"}, logEntryNames(t, path))
	m := manifestFrom(t, path)
	requireEachItemOnce(t, m)
	assert.Equal(t, statusCollected, findItem(t, m, "agent", "c-1").Status)
	assert.Equal(t, 0, res.Data.(bundleResult).Errors)
	assert.Empty(t, res.Warnings)
}

// TestSupportBundleKeepsTheOrderOfFirstAppearance asserts that a cluster
// named twice keeps the place of its first appearance.
func TestSupportBundleKeepsTheOrderOfFirstAppearance(t *testing.T) {
	t.Parallel()
	_, path := bundleWithFlags(t, []string{"c-1", "c-2"}, nil, "c-2,c-1,c-2", "agent")

	var order []string
	for _, it := range manifestFrom(t, path).Items {
		if it.Cluster != "" && (len(order) == 0 || order[len(order)-1] != it.Cluster) {
			order = append(order, it.Cluster)
		}
	}
	assert.Equal(t, []string{"c-2", "c-1"}, order)
}

// TestSupportBundleUsesTheLocalClusterNamedTwiceOnce asserts that the local
// literal named twice in --clusters is collected once.
func TestSupportBundleUsesTheLocalClusterNamedTwiceOnce(t *testing.T) {
	t.Parallel()
	_, path := bundleWithFlags(t, []string{"c-1"}, nil, "local,local", "agent")

	requireNoRepeat(t, tarEntryNames(t, path))
	m := manifestFrom(t, path)
	requireEachItemOnce(t, m)
	var nodes int
	for _, it := range m.Items {
		if it.Name == "nodes.json" && it.Cluster == engine.LocalCluster {
			nodes++
		}
	}
	assert.Equal(t, 1, nodes)
}

// TestResolveBundleClustersUsesEachClusterOnce asserts that --clusters lists
// each cluster once, in the order of first appearance, whether it is named
// twice by id, by name and id, or as the local literal.
func TestResolveBundleClustersUsesEachClusterOnce(t *testing.T) {
	t.Parallel()
	factory := &fakeFactory{local: newFakeClient(), resolve: map[string]string{"prod": "c-1"}}
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"c-1,c-1", []string{"c-1"}},
		{"prod,c-1", []string{"c-1"}},
		{"c-1,prod", []string{"c-1"}},
		{"c-2,c-1,c-2", []string{"c-2", "c-1"}},
		{"local,local", []string{engine.LocalCluster}},
		{"local,c-1,local", []string{engine.LocalCluster, "c-1"}},
		{" c-1 , c-1 ,", []string{"c-1"}},
	} {
		got, discoveryErr, err := resolveBundleClusters(t.Context(), factory, factory.getter(), tc.raw)
		require.NoError(t, err, tc.raw)
		require.NoError(t, discoveryErr, tc.raw)
		assert.Equal(t, tc.want, got, tc.raw)
	}
}
