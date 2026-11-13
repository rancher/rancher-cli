package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	storedRegistry      = "stored.example.test"
	newRegistry         = "new.example.test"
	testClusterRegistry = "cluster.example.test"
)

// clusterWithRegistry is a management Cluster that names a registry.
const clusterWithRegistry = `{"id":"c-1","metadata":{"name":"c-1"},"spec":{"displayName":"Production",` +
	`"importedConfig":{"privateRegistryURL":"` + testClusterRegistry + `"}},"status":{"linuxWorkerCount":2}}`

// declareRegistry makes the info of both versions of the fixture chart declare
// the two registry paths in its default values, and more of the global
// values when extra (a JSON member list starting with a comma) is not empty.
func declareRegistry(f *appFake, extra string) {
	for _, v := range []string{"1.0.0", "2.0.0"} {
		f.root.answers[infoPath(testChart, v)] = rootAnswer{status: 200, body: fmt.Sprintf(
			`{"values":{"replicaCount":1,"global":{"cattle":{"systemDefaultRegistry":""},"systemDefaultRegistry":""%s}},"chart":{"annotations":{},"version":%q}}`, extra, v)}
	}
}

// registryTree is a values tree whose two registry paths hold host.
func registryTree(host string) map[string]any {
	return map[string]any{"global": map[string]any{"cattle": map[string]any{"systemDefaultRegistry": host}, "systemDefaultRegistry": host}}
}

// valueEntriesOf lists the entries of the Diff that are about values.
func valueEntriesOf(eff safety.Effect) []safety.DiffEntry {
	var out []safety.DiffEntry
	for _, d := range eff.Diff {
		if strings.HasPrefix(d.Path, "values") {
			out = append(out, d)
		}
	}
	return out
}

func entryChanged(path string) safety.DiffEntry {
	return safety.DiffEntry{Path: path, From: hiddenText, To: hiddenText}
}

func entryAdded(path string) safety.DiffEntry { return safety.DiffEntry{Path: path, To: hiddenText} }

func entryRemoved(path string) safety.DiffEntry {
	return safety.DiffEntry{Path: path, From: hiddenText}
}

// cattleOnly is the global values an upgrade adds to a release that has none.
func cattleOnly() map[string]any { return map[string]any{"global": testCattleValues()} }

// TestUpgradeValuesSent asserts the values an upgrade POST carries: the
// stored values with the files merged over them in order and the values the
// CLI adds set where the result has none, with null kept as a value; with
// --reset-values the files with the values the CLI adds, which with no file
// are only those values. Without --reset-values a release with stored values
// never sends an absent or empty values field, and resetValues says which.
func TestUpgradeValuesSent(t *testing.T) {
	t.Parallel()
	src := files{
		"a.yaml":    "a:\n  b: 2\n  d: new\ngone: null\nlist: [3]\n",
		"b.yaml":    "a:\n  b: 3\n",
		"c.yaml":    "b: three\nc: 4\n",
		"cattle":    "global:\n  cattle:\n    clusterName: from-file\n",
		"empty.yml": "{}\n",
	}
	stored := storedValues(map[string]any{
		"a": map[string]any{"b": float64(1), "c": "keep"}, "gone": "x", "list": []any{float64(1), float64(2)},
	})
	cases := []struct {
		name  string
		files []string
		edit  func(*appInput)
		want  map[string]any
		reset bool
	}{
		{name: "stored values alone", edit: withFlag(flagHelmTimeout, "10m"), want: stored},
		{name: "stored and a file", files: []string{"a.yaml"}, want: storedValues(map[string]any{
			"a": map[string]any{"b": float64(2), "c": "keep", "d": "new"}, "gone": nil, "list": []any{float64(3)},
		})},
		{name: "stored and two files in order", files: []string{"a.yaml", "b.yaml"}, want: storedValues(map[string]any{
			"a": map[string]any{"b": float64(3), "c": "keep", "d": "new"}, "gone": nil, "list": []any{float64(3)},
		})},
		{name: "a file value at a cluster value path", files: []string{"cattle"}, want: storedValues(map[string]any{
			"a": map[string]any{"b": float64(1), "c": "keep"}, "gone": "x", "list": []any{float64(1), float64(2)},
			"global": map[string]any{"cattle": map[string]any{"clusterName": "from-file"}},
		})},
		{name: "reset with files", files: []string{"c.yaml"}, reset: true,
			want: map[string]any{"b": "three", "c": float64(4), "global": testCattleValues()}},
		{name: "reset with an empty file", files: []string{"empty.yml"}, reset: true, want: cattleOnly()},
		{name: "reset with no file", reset: true, want: cattleOnly()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, stored)
			in := upgradeInput(func(in *appInput) {
				if len(tc.files) > 0 {
					in.strs[flagValues] = tc.files
				}
				if tc.reset {
					in.b[flagResetValues] = true
				}
				if tc.edit != nil {
					tc.edit(in)
				}
			})
			body := upgradeBodyOf(t, f, src, in)
			chart := upgradeChartOf(t, body)
			assert.Equal(t, tc.want, chart["values"])
			assert.NotEmpty(t, chart["values"])
			if tc.reset {
				assert.Equal(t, true, chart["resetValues"])
			} else {
				assert.NotContains(t, chart, "resetValues")
			}
		})
	}
}

// TestUpgradeValuesPreviewEntries asserts the entries of the preview: a path
// only in the sent values is added, only in the stored values is removed, and
// in both with another value is changed, every value hidden; and the count of
// the stored values resent unchanged.
func TestUpgradeValuesPreviewEntries(t *testing.T) {
	t.Parallel()
	stored := storedValues(map[string]any{
		"a": map[string]any{"b": float64(1), "c": "keep"}, "gone": "x", "list": []any{float64(1), float64(2)},
	})
	src := files{"a.yaml": "a:\n  b: 2\n  d: new\ngone: null\nlist: [3]\n", "c.yaml": "b: three\nc: 4\n", "same.yaml": "a:\n  c: keep\n"}
	t.Run("stored and a file", func(t *testing.T) {
		t.Parallel()
		eff, _, err := planUpgrade(t, upgradeFake(t, stored), src, upgradeInput(func(in *appInput) { in.strs[flagValues] = []string{"a.yaml"} }))
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{entryChanged("values.a.b"), entryAdded("values.a.d"), entryRemoved("values.gone"), entryChanged("values.list")}, valueEntriesOf(eff))
		assert.Contains(t, eff.Warnings, "7 of the release's values are sent unchanged")
	})
	t.Run("reset with files lists the paths it removes", func(t *testing.T) {
		t.Parallel()
		eff, _, err := planUpgrade(t, upgradeFake(t, stored), src, upgradeInput(withBool(flagResetValues, true), func(in *appInput) { in.strs[flagValues] = []string{"c.yaml"} }))
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{
			entryRemoved("values.a.b"), entryRemoved("values.a.c"), entryAdded("values.b"), entryAdded("values.c"), entryRemoved("values.gone"), entryRemoved("values.list"),
		}, valueEntriesOf(eff))
		assert.Contains(t, eff.Warnings, "6 values in the request are the same as the release's values")
		assert.Equal(t, true, diffPaths(eff)["resetValues"])
	})
	t.Run("reset with no file", func(t *testing.T) {
		t.Parallel()
		eff, _, err := planUpgrade(t, upgradeFake(t, stored), nil, upgradeInput(withBool(flagResetValues, true)))
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{
			entryRemoved("values.a.b"), entryRemoved("values.a.c"), entryRemoved("values.gone"), entryRemoved("values.list"),
		}, valueEntriesOf(eff))
	})
	t.Run("a file that changes nothing lists nothing", func(t *testing.T) {
		t.Parallel()
		eff, _, err := planUpgrade(t, upgradeFake(t, stored), src, upgradeInput(withFlag(flagHelmTimeout, "10m"), func(in *appInput) { in.strs[flagValues] = []string{"same.yaml"} }))
		require.NoError(t, err)
		assert.Empty(t, valueEntriesOf(eff))
		assert.Contains(t, eff.Warnings, "10 of the release's values are sent unchanged")
	})
}

// TestUpgradeValuesUnchangedCount asserts the count of the values resent as
// stored reads in the singular for one value and in the plural for any other
// number, and with --reset-values says the sent values equal the stored
// ones.
func TestUpgradeValuesUnchangedCount(t *testing.T) {
	t.Parallel()
	one := map[string]any{"global": map[string]any{"cattle": map[string]any{"clusterId": testCluster}}}
	cases := []struct {
		name   string
		stored map[string]any
		file   string
		reset  bool
		want   string
	}{
		{name: "one, kept", stored: one, want: "1 of the release's values is sent unchanged"},
		{name: "several, kept", stored: storedValues(nil), want: "6 of the release's values are sent unchanged"},
		{name: "one, reset", stored: one, file: "global:\n  cattle:\n    clusterId: " + testCluster + "\n", reset: true, want: "1 value in the request is the same as the release's value"},
		{name: "several, reset", stored: storedValues(nil), reset: true, want: "6 values in the request are the same as the release's values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := files{"v.yaml": tc.file}
			eff, _, err := planUpgrade(t, upgradeFake(t, tc.stored), src, upgradeInput(func(in *appInput) {
				if tc.reset {
					in.b[flagResetValues] = true
				}
				if tc.file != "" {
					in.strs[flagValues] = []string{"v.yaml"}
				}
				in.s[flagHelmTimeout] = "10m"
			}))
			require.NoError(t, err)
			var counts []string
			for _, w := range eff.Warnings {
				if strings.Contains(w, "sent unchanged") || strings.Contains(w, "the same as the release's value") {
					counts = append(counts, w)
				}
			}
			assert.Equal(t, []string{tc.want}, counts)
		})
	}
}

// TestUpgradeClusterValuesAreNotAddedAgain asserts a cluster value the stored
// values hold is sent as stored, listed in no entry, and a cluster value they
// lack is added with its value shown.
func TestUpgradeClusterValuesAreNotAddedAgain(t *testing.T) {
	t.Parallel()
	stored := map[string]any{"global": map[string]any{"cattle": map[string]any{"clusterId": "stored-id", "url": "https://stored.example.test"}}}
	f := upgradeFake(t, stored)
	eff, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
	cattle := upgradeValuesOf(t, f)["global"].(map[string]any)["cattle"].(map[string]any)
	assert.Equal(t, "stored-id", cattle["clusterId"])
	assert.Equal(t, "https://stored.example.test", cattle["url"])
	assert.Equal(t, "Production", cattle["clusterName"])
	paths := diffPaths(eff)
	assert.NotContains(t, paths, "values.global.cattle.clusterId")
	assert.NotContains(t, paths, "values.global.cattle.url")
	assert.Equal(t, "Production", paths["values.global.cattle.clusterName"])
	assert.Equal(t, testSystemProject, paths["values.global.cattle.systemProjectId"])
	assert.Contains(t, eff.Warnings, "2 of the release's values are sent unchanged")
}

// TestUpgradeRegistryValues asserts the registry values of an upgrade of a
// chart that declares both registry paths: the stored values with no registry
// get the cluster's, with the host shown and the CLI-added warning; a stored
// registry is sent as stored; --system-default-registry replaces it and shows
// both values; --no-system-default-registry drops it, shows the dropped host
// and warns, and for a chart that names an image pull secret path says Rancher
// deletes the secrets it manages.
func TestUpgradeRegistryValues(t *testing.T) {
	t.Parallel()
	pullSecrets := `,"imagePullSecrets":[]`
	addedWarning := "rancher-cli adds the values the Rancher UI adds when it installs or upgrades a chart: " +
		"values.global.cattle.systemDefaultRegistry, values.global.systemDefaultRegistry"
	pullWarning := "--no-system-default-registry sends no registry value; Rancher then deletes the image pull secrets it manages for this release"
	both := func(from, to any) []safety.DiffEntry {
		return []safety.DiffEntry{
			{Path: "values.global.cattle.systemDefaultRegistry", From: from, To: to},
			{Path: "values.global.systemDefaultRegistry", From: from, To: to},
		}
	}
	cases := []struct {
		name    string
		stored  map[string]any
		extra   string
		edit    func(*appInput)
		entries []safety.DiffEntry
		sent    any
		warns   []string
		noWarns []string
	}{
		{name: "stored tree with no registry gets the cluster's", stored: storedValues(nil), edit: withFlag(flagHelmTimeout, "10m"),
			entries: both(nil, testClusterRegistry), sent: testClusterRegistry, warns: []string{addedWarning}},
		{name: "stored registry is sent as stored", stored: storedValues(registryTree(storedRegistry)), edit: withFlag(flagHelmTimeout, "10m"),
			sent: storedRegistry, noWarns: []string{addedWarning}},
		{name: "--system-default-registry replaces the stored registry", stored: storedValues(registryTree(storedRegistry)), edit: withFlag(flagSystemDefaultRegistry, newRegistry),
			entries: both(storedRegistry, newRegistry), sent: newRegistry},
		{name: "--system-default-registry on a tree with no registry", stored: storedValues(nil), edit: withFlag(flagSystemDefaultRegistry, newRegistry),
			entries: both(nil, newRegistry), sent: newRegistry},
		{name: "--no-system-default-registry drops the stored registry", stored: storedValues(registryTree(storedRegistry)), edit: withBool(flagNoSystemDefaultRegistry, true),
			entries: both(storedRegistry, nil), warns: []string{"--no-system-default-registry leaves out values.global.cattle.systemDefaultRegistry, values.global.systemDefaultRegistry set in the release's values"},
			noWarns: []string{pullWarning}},
		{name: "--no-system-default-registry on a chart with an image pull secret path", stored: storedValues(registryTree(storedRegistry)), extra: pullSecrets,
			edit: withBool(flagNoSystemDefaultRegistry, true), entries: both(storedRegistry, nil), warns: []string{pullWarning}},
		{name: "--no-system-default-registry on a tree with no registry", stored: storedValues(nil), edit: withBool(flagNoSystemDefaultRegistry, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, tc.stored)
			f.root.answers[clusterPath] = rootAnswer{status: 200, body: clusterWithRegistry}
			declareRegistry(f, tc.extra)
			eff, exec, err := planUpgrade(t, f, nil, upgradeInput(tc.edit))
			require.NoError(t, err)
			assert.Equal(t, tc.entries, registryEntries(valueEntriesOf(eff)))
			for _, w := range tc.warns {
				assert.Contains(t, eff.Warnings, w)
			}
			for _, w := range tc.noWarns {
				assert.NotContains(t, eff.Warnings, w)
			}
			_, err = exec(t.Context())
			require.NoError(t, err)
			values := upgradeValuesOf(t, f)
			for _, path := range registryPaths {
				got, ok := sentAt(values, path)
				if tc.sent == nil {
					assert.False(t, ok, "%s is sent", path)
					continue
				}
				assert.Equal(t, tc.sent, got, path.String())
			}
		})
	}
}

// registryEntries keeps the entries about the two registry paths.
func registryEntries(entries []safety.DiffEntry) []safety.DiffEntry {
	var out []safety.DiffEntry
	for _, e := range entries {
		if strings.HasSuffix(e.Path, "systemDefaultRegistry") {
			out = append(out, e)
		}
	}
	return out
}

// TestUpgradeRegistryFlagsRefusedForAChartWithNoRegistryPath asserts both
// registry flags are refused for a chart whose default values declare neither
// path, and given together.
func TestUpgradeRegistryFlagsRefusedForAChartWithNoRegistryPath(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*appInput){
		"host": withFlag(flagSystemDefaultRegistry, newRegistry),
		"none": withBool(flagNoSystemDefaultRegistry, true),
		"both": func(in *appInput) {
			in.s[flagSystemDefaultRegistry] = newRegistry
			in.b[flagNoSystemDefaultRegistry] = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce := upgradeRefusal(t, upgradeFake(t, nil), nil, upgradeInput(edit))
			assert.Equal(t, clierr.CodeValidation, ce.Code)
		})
	}
}

// diskFiles reads the files of the disk, as the command does.
type diskFiles struct{}

func (diskFiles) Read(p string) ([]byte, error) { return os.ReadFile(p) }

// TestUpgradeValuesFileWithCRLFAndByteOrderMark asserts a values file with
// CRLF line ends and a byte order mark, written to a temporary directory and
// named with a path built for the platform, merges over the stored values as
// a file with LF line ends does.
func TestUpgradeValuesFileWithCRLFAndByteOrderMark(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "values.yaml")
	require.NoError(t, os.WriteFile(path, []byte("\xef\xbb\xbfa: 1\r\nb:\r\n  c: two\r\n"), 0o600))
	f := upgradeFake(t, storedValues(map[string]any{"b": map[string]any{"d": "keep"}}))
	body := upgradeBodyOf(t, f, diskFiles{}, upgradeInput(func(in *appInput) { in.strs[flagValues] = []string{path} }))
	assert.Equal(t, storedValues(map[string]any{"a": float64(1), "b": map[string]any{"c": "two", "d": "keep"}}), upgradeChartOf(t, body)["values"])
}
