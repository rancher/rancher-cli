package catalog

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const otherRepo = "repo2"

// repoPathFor is the path of a ClusterRepo's action link with query.
func repoPathFor(repo, query string) string {
	return "/k8s/clusters/" + testCluster + "/v1/catalog.cattle.io.clusterrepos/" + repo + "?" + query
}

// addOtherRepo adds the ClusterRepo repo2, whose filtered index lists the
// chart at the given versions and whose info answer for each names the
// annotations.
func addOtherRepo(f *appFake, annotations map[string]string, versions ...string) {
	f.put("clusterrepos", "", otherRepo, map[string]any{"spec": map[string]any{"url": "https://other.example.test"}})
	list := make([]ChartVersion, 0, len(versions))
	for _, v := range versions {
		list = append(list, ChartVersion{Version: v, Digest: "sha256:other-" + v, Annotations: annotations})
		f.root.answers[repoPathFor(otherRepo, "link=info&chartName="+testChart+"&version="+v)] = rootAnswer{status: 200,
			body: `{"values":{"replicaCount":1},"chart":{"annotations":{},"version":"` + v + `"}}`}
	}
	f.root.answers[repoPathFor(otherRepo, "link=index")] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: list})}
}

// TestUpgradeVersionTheAppRunsIsKeptOnlyInItsSourceRepo asserts the version
// the App runs gets the exemptions of a kept version (no namespace or
// release-name check, no index entry needed) only for the ClusterRepo the App
// came from: with another --repo the version must be in that repository's
// filtered index and every check runs.
func TestUpgradeVersionTheAppRunsIsKeptOnlyInItsSourceRepo(t *testing.T) {
	t.Parallel()
	t.Run("a namespace annotation of another namespace refuses", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil)
		addOtherRepo(f, map[string]string{annotationNamespace: "elsewhere"}, "1.0.0")
		ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagRepo, otherRepo)))
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "installs only in namespace 'elsewhere'")
	})
	t.Run("a release-name annotation of another release refuses", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil)
		addOtherRepo(f, map[string]string{annotationReleaseName: "fleet"}, "1.0.0")
		ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagRepo, otherRepo), withFlag(flagVersion, "1.0.0")))
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "installs only as release 'fleet'")
	})
	t.Run("an index that hides the version refuses and reads no info", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil)
		addOtherRepo(f, nil, "2.0.0")
		for _, edit := range []func(*appInput){withFlag(flagRepo, otherRepo), withFlag(flagVersion, "1.0.0")} {
			in := upgradeInput(withFlag(flagRepo, otherRepo), edit)
			ce := upgradeRefusal(t, f, nil, in)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Contains(t, ce.Message, "version '1.0.0' of chart 'nginx' is not in the index ClusterRepo 'repo2'")
		}
		assert.Empty(t, requestsOf(f, "link=info"))
	})
	t.Run("an index that lists the version plain is an upgrade with no kept-version warning", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil)
		addOtherRepo(f, nil, "1.0.0")
		eff, exec, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagRepo, otherRepo)))
		require.NoError(t, err)
		for _, w := range eff.Warnings {
			assert.NotContains(t, w, "is no longer listed in the index for this cluster")
		}
		assert.Contains(t, eff.Warnings, "the app will record ClusterRepo 'repo2' as the one it came from; a later uninstall of this app runs Helm as the service account in spec.serviceAccount of that ClusterRepo, when it sets one")
		f.root.answers[repoPathFor(otherRepo, "link=index")] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0", Digest: "sha256:changed"}}})}
		_, err = exec(t.Context())
		require.ErrorContains(t, err, "the content of chart 'nginx' '1.0.0' changed in the index since the preview")
	})
}

// TestUpgradeRegistryRefusalNamesWhereTheValuesCameFrom asserts the registry
// messages of an upgrade name the release's stored values for a path that
// came from there, and the values files for a path a file set.
func TestUpgradeRegistryRefusalNamesWhereTheValuesCameFrom(t *testing.T) {
	t.Parallel()
	t.Run("a stored parent that is not a mapping", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, map[string]any{"global": "x"})
		declareRegistry(f, "")
		ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagSystemDefaultRegistry, newRegistry)))
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "--system-default-registry cannot set values.global.cattle.systemDefaultRegistry: the release's values set values.global to a value that is not a mapping")
		assert.NotContains(t, ce.Message, "values files")
	})
	t.Run("a file parent that is not a mapping", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, storedValues(nil))
		declareRegistry(f, "")
		src := files{"g.yaml": "global: x\n"}
		in := upgradeInput(withFlag(flagSystemDefaultRegistry, newRegistry), func(in *appInput) { in.strs[flagValues] = []string{"g.yaml"} })
		ce := upgradeRefusal(t, f, src, in)
		assert.Contains(t, ce.Message, "the values files set values.global to a value that is not a mapping")
	})
	t.Run("a stored registry left out", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, storedValues(registryTree(storedRegistry)))
		declareRegistry(f, "")
		eff, _, err := planUpgrade(t, f, nil, upgradeInput(withBool(flagNoSystemDefaultRegistry, true)))
		require.NoError(t, err)
		assert.Contains(t, eff.Warnings, "--no-system-default-registry leaves out values.global.cattle.systemDefaultRegistry, values.global.systemDefaultRegistry set in the release's values")
	})
	t.Run("a registry left out that a file and the stored values set", func(t *testing.T) {
		t.Parallel()
		stored := storedValues(map[string]any{"global": map[string]any{"cattle": map[string]any{"systemDefaultRegistry": storedRegistry}}})
		f := upgradeFake(t, stored)
		declareRegistry(f, "")
		src := files{"r.yaml": "global:\n  systemDefaultRegistry: file.example.test\n"}
		in := upgradeInput(withBool(flagNoSystemDefaultRegistry, true), func(in *appInput) { in.strs[flagValues] = []string{"r.yaml"} })
		eff, _, err := planUpgrade(t, f, src, in)
		require.NoError(t, err)
		assert.Contains(t, eff.Warnings, "--no-system-default-registry leaves out values.global.systemDefaultRegistry set in the values files and values.global.cattle.systemDefaultRegistry set in the release's values")
	})
}

// TestUpgradeNullInAValuesFile asserts a null in a values file removes the
// key: for a stored key the preview lists a removal, for a key the release
// does not have it lists nothing and counts nothing, and an upgrade whose only
// change is such a null is refused as nothing to change.
func TestUpgradeNullInAValuesFile(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, stored map[string]any, file string, edits ...func(*appInput)) (safety.Effect, error) {
		t.Helper()
		f := upgradeFake(t, stored)
		in := upgradeInput(append(edits, func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} })...)
		eff, _, err := planUpgrade(t, f, files{"v.yaml": file}, in)
		return eff, err
	}
	t.Run("a stored key is listed as removed", func(t *testing.T) {
		t.Parallel()
		eff, err := run(t, storedValues(map[string]any{"gone": "x-value"}), "gone: null\n")
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{entryRemoved("values.gone")}, valueEntriesOf(eff))
	})
	t.Run("a stored nested key is listed as removed", func(t *testing.T) {
		t.Parallel()
		eff, err := run(t, storedValues(map[string]any{"a": map[string]any{"b": "x-value", "c": "kept-value"}}), "a:\n  b: null\n")
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{entryRemoved("values.a.b")}, valueEntriesOf(eff))
	})
	t.Run("a key the release lacks is neither listed nor counted", func(t *testing.T) {
		t.Parallel()
		stored := storedValues(map[string]any{"kept": "x-value"})
		eff, err := run(t, stored, "nothere: null\nnew: added-value\n", withFlag(flagHelmTimeout, "10m"))
		require.NoError(t, err)
		assert.Equal(t, []safety.DiffEntry{entryAdded("values.new")}, valueEntriesOf(eff))
		assert.Contains(t, eff.Warnings, fmt.Sprintf("%d of the release's values are sent unchanged", leafCount(stored)), "the null is not counted")
	})
	t.Run("a null for a key the release lacks is nothing to change", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, storedValues(nil))
		in := upgradeInput(func(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} })
		ce := upgradeRefusal(t, f, files{"v.yaml": "nothere: null\na:\n  b: null\n"}, in)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "nothing to change")
	})
	t.Run("a null for a stored key is a change", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, storedValues(map[string]any{"gone": "x-value"}), "gone: null\n")
		require.NoError(t, err)
	})
}

// TestUpgradeOtherChartInstallCommandCarriesTheTypedRepo asserts the install
// command of the refusal of another chart names the --repo that was typed,
// and a placeholder when none was.
func TestUpgradeOtherChartInstallCommandCarriesTheTypedRepo(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagChart, "other"), withFlag(flagRepo, testRepo)))
	assert.Contains(t, ce.Message, ", run "+clierr.Rancher+" app install <release> --cluster 'prod' --namespace 'web' --repo 'repo1' --chart 'other' --dry-run")
	ce = upgradeRefusal(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagChart, "other")))
	assert.Contains(t, ce.Message, "--repo <repo> --chart 'other' --dry-run")
}

// TestKeptVersionWarningIsOneText asserts the warning of a kept version the
// index does not list reads the same in the preview and in the result.
func TestKeptVersionWarningIsOneText(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "version 1.0.0 is no longer listed in the index for this cluster; the upgrade stays on it, and its chart content is not compared between the preview and the request",
		keptVersionWarning("1.0.0"))
}

// leafCount is the number of leaves of a values tree.
func leafCount(tree map[string]any) int {
	n := 0
	Values{tree: tree}.leaves(func(string, any) { n++ })
	return n
}
