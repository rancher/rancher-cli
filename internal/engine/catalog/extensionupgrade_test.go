package catalog

import (
	"cmp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

const extInstalledVersion = "1.14.2"

func newTestExtensionUpgrade(f *extFake) *extensionUpgradeCmd {
	return &extensionUpgradeCmd{clients: f, roots: f, actions: f,
		now: func() time.Time { return testNow }, newID: func() string { return testRequestID }, interval: time.Second}
}

// putExtApp stores the App of the extension epinio at the status, the chart
// version and the source annotation (none when repo is empty), as the first
// revision of the release.
func putExtApp(f *extFake, status, version, repo string) {
	putExtAppMeta(f, extChart, status, version, repo, map[string]any{"resourceVersion": "7"})
}

// putExtAppMeta is putExtApp with the metadata of the App given.
func putExtAppMeta(f *extFake, name, status, version, repo string, metadata map[string]any) {
	annotations := map[string]any{}
	if repo != "" {
		annotations[annotationSourceRepoType] = sourceRepoTypeCluster
		annotations["catalog.cattle.io/ui-source-repo"] = repo
	}
	f.put("apps", extUIPlugin, name, map[string]any{
		"metadata": metadata,
		"spec": map[string]any{
			"name": name, "namespace": extUIPlugin, "version": int64(1),
			"info":  map[string]any{"status": status},
			"chart": map[string]any{"metadata": map[string]any{"name": name, "version": version, "annotations": annotations}},
		},
	})
}

// newUpgradeFake is the extension fake with the extension installed at 1.14.2
// from the repository, which offers 1.14.3 and 1.14.2.
func newUpgradeFake(t *testing.T) *extFake {
	t.Helper()
	f := newExtFake(t)
	putExtApp(f, "deployed", extInstalledVersion, extRepo)
	return f
}

// TestExtensionUpgradePreview asserts the Effect of an upgrade: an update of
// the App from server reads with no precondition, destructive, the first
// compatible version, the fixed namespace and release name, the revision the
// request creates and no option the Rancher UI does not send.
func TestExtensionUpgradePreview(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	assert.False(t, eff.ConflictDetection)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.Equal(t, engine.LocalCluster, eff.Cluster)
	assert.Equal(t, map[string]any{"repo": extRepo, "chart": extChart, "version": "1.14.3", "namespace": extUIPlugin, "releaseName": extChart, "revision": int64(2)}, diffPaths(eff))
	for _, d := range eff.Diff {
		if d.Path == "version" {
			assert.Equal(t, extInstalledVersion, d.From)
		}
	}
	assert.Contains(t, eff.Warnings, warnServerReads)
	assert.Contains(t, eff.Warnings, warnUpgradeRisk)
	assert.Zero(t, f.postCount())
}

// TestExtensionUpgradeBody asserts the request: the exact key set of the body
// and of its chart entry, values sent as {} and the local path.
func TestExtensionUpgradeBody(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	_, exec := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
	_, err := exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	p := f.posts[0]
	assert.Equal(t, "/v1/catalog.cattle.io.clusterrepos/"+extRepo+"?action=upgrade", p.path)
	assert.NotContains(t, p.path, "/k8s/clusters/")
	assert.ElementsMatch(t, []string{"namespace", "charts"}, keysOf(p.body))
	assert.Equal(t, extUIPlugin, p.body["namespace"])
	chart := p.body["charts"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"chartName", "version", "releaseName", "values", "annotations"}, keysOf(chart))
	assert.Equal(t, extChart, chart["releaseName"])
	assert.Equal(t, "1.14.3", chart["version"])
	assert.Equal(t, map[string]any{}, chart["values"])
}

// TestExtensionUpgradeRefusals asserts each refusal of the upgrade plan: its
// code, its text and that no request is sent.
func TestExtensionUpgradeRefusals(t *testing.T) {
	t.Parallel()
	getApp := clierr.Rancher + " get app 'epinio' --cluster 'local' --namespace 'cattle-ui-plugin-system'"
	cases := []struct {
		name  string
		setup func(*extFake)
		in    appInput
		code  string
		text  string
		hint  string
	}{
		{name: "not installed", in: extInput(extChart), code: clierr.CodeNotFound,
			setup: func(f *extFake) { f.remove("apps", extUIPlugin, extChart) },
			text:  "extension 'epinio' is not installed: no App 'epinio' in namespace cattle-ui-plugin-system",
			hint:  clierr.Rancher + " extension install 'epinio' --dry-run"},
		{name: "loaded from a URL", in: extInput(extChart), code: clierr.CodeNotFound,
			setup: func(f *extFake) {
				f.remove("apps", extUIPlugin, extChart)
				f.put("uiplugins", "other-ns", "dev-load", map[string]any{"spec": map[string]any{"plugin": map[string]any{"name": extChart, "version": "0.1.0"}}})
			},
			text: "extension 'epinio' has no App in namespace cattle-ui-plugin-system; it was loaded from a URL (UIPlugin 'other-ns/dev-load'), and this command does not upgrade it"},
		{name: "uninstalled with kept history", in: extInput(extChart), code: clierr.CodeConflict,
			setup: func(f *extFake) { putExtApp(f, "uninstalled", extInstalledVersion, extRepo) },
			text:  "App 'epinio' in namespace cattle-ui-plugin-system has the status uninstalled, and Helm upgrades only a deployed, failed or superseded release",
			hint:  getApp},
		{name: "pending install", in: extInput(extChart), code: clierr.CodeConflict,
			setup: func(f *extFake) { putExtApp(f, "pending-install", extInstalledVersion, extRepo) },
			text:  "has the status pending-install", hint: getApp},
		{name: "the App names no repository", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) { putExtApp(f, "deployed", extInstalledVersion, "") },
			text:  "the App of extension 'epinio' does not name the ClusterRepo it came from, and the Rancher UI does not upgrade it either",
			hint:  clierr.Rancher + " extension list"},
		{name: "another repository", in: extInput(extChart, flagRepo, "mirror"), code: clierr.CodeValidation,
			text: "extension 'epinio' came from ClusterRepo 'partner-extensions'; an extension is upgraded from the ClusterRepo it came from",
			hint: clierr.Rancher + " extension upgrade 'epinio' --repo 'partner-extensions' --dry-run"},
		{name: "repository hidden", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					repoItemBody(extRepo, "active", `,"annotations":{"clusterrepo.cattle.io/hidden":"true"}`))}
			},
			text: "ClusterRepo 'partner-extensions' is hidden, and the Rancher UI does not read extensions from it"},
		{name: "repository disabled", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(
					`{"metadata":{"name":"partner-extensions","state":{"name":"active"}},"spec":{"enabled":false}}`)}
			},
			text: "ClusterRepo 'partner-extensions' is disabled"},
		{name: "repository absent", in: extInput(extChart), code: clierr.CodeNotFound,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemAt("other", "active", "https://example.test/other"))}
			},
			text: "ClusterRepo 'partner-extensions' not found", hint: clierr.Rancher + " get clusterrepos --cluster 'local'"},
		{name: "repository not active", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[reposPath] = rootAnswer{status: 200, body: collectionBody(repoItemAt(extRepo, "downloading", "https://example.test/slow"))}
			},
			text: "ClusterRepo 'partner-extensions' is not ready: its state is downloading", hint: clierr.Rancher + " get clusterrepo 'partner-extensions' --cluster 'local'"},
		{name: "index still downloading", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) { f.root.answers[extIndexPath] = rootAnswer{status: 503} },
			text:  "ClusterRepo 'partner-extensions' is not ready: Rancher is still downloading it"},
		{name: "index forbidden", in: extInput(extChart), code: clierr.CodeRBAC,
			setup: func(f *extFake) { f.root.answers[extIndexPath] = rootAnswer{status: 403} }},
		{name: "chart is not an extension", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) {
				f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[{"name":"epinio","version":"1.14.3"}]}}`}
			},
			text: "chart 'epinio' in ClusterRepo 'partner-extensions' is not a Rancher UI extension",
			hint: clierr.Rancher + " charts --cluster 'local' --repo 'partner-extensions'"},
		{name: "version not in the index", in: extInput(extChart, flagVersion, "9.9.9"), code: clierr.CodeValidation,
			text: "version '9.9.9' of extension 'epinio' is not in the index ClusterRepo 'partner-extensions' serves; --version names one exact version; the index has 1.14.3, 1.14.2"},
		{name: "the installed version by default", in: extInput(extChart), code: clierr.CodeValidation,
			setup: func(f *extFake) { putExtApp(f, "deployed", "1.14.3", extRepo) },
			text:  "version '1.14.3' of extension 'epinio' is already installed", hint: clierr.Rancher + " extension list"},
		{name: "the installed version by --version", in: extInput(extChart, flagVersion, extInstalledVersion), code: clierr.CodeValidation,
			text: "version '1.14.2' of extension 'epinio' is already installed", hint: clierr.Rancher + " extension list"},
		{name: "operation running", in: extInput(extChart), code: clierr.CodeConflict,
			setup: func(f *extFake) {
				f.put("operations", extUIPlugin, "helm-operation-a", operation(extChart, testNow.Add(-time.Minute), "True", 1, 1))
			},
			text: "operation 'helm-operation-a' for release 'epinio' in namespace 'cattle-ui-plugin-system' may still be running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), tc.in)
			require.Error(t, err)
			ce := clierr.From(err)
			assert.Equal(t, tc.code, ce.Code, ce.Message)
			if tc.text != "" {
				require.ErrorContains(t, err, tc.text)
			}
			if tc.hint != "" {
				assert.Equal(t, tc.hint, ce.Hint)
			}
			assert.Zero(t, f.postCount())
		})
	}
}

// TestExtensionUpgradeRepositoryOfTheApp asserts the repository an upgrade
// uses: the App's source annotation, else its label, and that a typed --repo
// equal to it is accepted.
func TestExtensionUpgradeRepositoryOfTheApp(t *testing.T) {
	t.Parallel()
	t.Run("annotation", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
		assert.Equal(t, extRepo, diffPaths(eff)["repo"])
	})
	t.Run("label", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		putExtAppMeta(f, extChart, "deployed", extInstalledVersion, "", map[string]any{"resourceVersion": "7",
			"labels": map[string]any{"catalog.cattle.io/cluster-repo-name": extRepo}})
		eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
		assert.Equal(t, extRepo, diffPaths(eff)["repo"])
	})
	t.Run("the same repository typed", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart, flagRepo, extRepo))
		assert.Equal(t, "1.14.3", diffPaths(eff)["version"])
	})
}

// TestExtensionUpgradeBundleRefusal asserts the bundle rule at every position
// of the annotation: first version, a later one, an empty value and none.
func TestExtensionUpgradeBundleRefusal(t *testing.T) {
	t.Parallel()
	member := `"catalog.cattle.io/ui-extensions-bundle-members":"a,b"`
	ver := func(v, extra string) string {
		return `{"name":"epinio","version":"` + v + `","annotations":{` + extCompatible + extra + `}}`
	}
	cases := []struct {
		name    string
		first   string
		second  string
		refused bool
	}{
		{name: "first version", first: "," + member, refused: true},
		{name: "a later version", second: "," + member, refused: true},
		{name: "empty value", first: `,"catalog.cattle.io/ui-extensions-bundle-members":""`},
		{name: "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + ver("2.0.0", tc.first) + `,` + ver("1.0.0", tc.second) + `]}}`}
			_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "this chart installs several extensions together; install it from the Rancher UI")
			assert.Equal(t, clierr.CodeValidation, errCode(err))
		})
	}
}

// TestExtensionUpgradeVersionChoice asserts the default version rule and its
// warnings, the downgrade warning of an older typed version and the warning of
// an installed version the index does not list. A chart whose
// version differs from its application version is compared by its chart
// version: the UIPlugin's version plays no part.
func TestExtensionUpgradeVersionChoice(t *testing.T) {
	t.Parallel()
	entry := func(v, annotations string, deprecated bool) string {
		d := ""
		if deprecated {
			d = `,"deprecated":true`
		}
		return `{"name":"epinio","version":"` + v + `"` + d + `,"annotations":{` + extCompatible + annotations + `}}`
	}
	cases := []struct {
		name      string
		index     string
		version   string
		installed string
		flags     []string
		warning   string
	}{
		{name: "default version rule", index: entry("3.0.0", `,"catalog.cattle.io/rancher-version":">= 9.0.0"`, false) + "," + entry("2.0.0", "", false) + "," + entry("1.0.0", "", false), version: "2.0.0", installed: "1.0.0"},
		{name: "prerelease default", index: entry("2.0.0-rc.1", "", false) + "," + entry("1.0.0", "", false), version: "2.0.0-rc.1", installed: "1.0.0",
			warning: "2.0.0-rc.1 is a prerelease version of extension epinio; name another with --version"},
		{name: "deprecated default", index: entry("2.0.0", "", true) + "," + entry("1.0.0", "", false), version: "2.0.0", installed: "1.0.0",
			warning: "2.0.0 of extension epinio is deprecated; name another with --version"},
		{name: "a downgrade", index: entry("2.0.0", "", false) + "," + entry("1.0.0", "", false), version: "1.0.0", installed: "2.0.0", flags: []string{flagVersion, "1.0.0"},
			warning: "1.0.0 is older than the installed 2.0.0: a downgrade; Helm runs it as any upgrade, and the chart may not support downgrades"},
		{name: "an installed version the index does not list", index: entry("2.0.0", "", false) + "," + entry("1.0.0", "", false), version: "2.0.0", installed: "0.5.0",
			warning: "the index does not list the installed version 0.5.0; whether 2.0.0 is older was not checked; Helm runs any upgrade, and the chart may not support downgrades"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			putExtApp(f, "deployed", tc.installed, extRepo)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + tc.index + `]}}`}
			eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart, tc.flags...))
			assert.Equal(t, tc.version, diffPaths(eff)["version"])
			if tc.warning != "" {
				assert.Contains(t, eff.Warnings, tc.warning)
			}
		})
	}
	t.Run("an upgrade is not a downgrade", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
		for _, w := range eff.Warnings {
			assert.NotContains(t, w, "a downgrade")
		}
	})
	t.Run("the version of the chart, not of the UIPlugin", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		putExtApp(f, "deployed", "1.14.3", extRepo)
		f.put("uiplugins", extUIPlugin, extChart, map[string]any{"spec": map[string]any{"plugin": map[string]any{"name": extChart, "version": "0.0.1"}}})
		_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
		require.ErrorContains(t, err, "version '1.14.3' of extension 'epinio' is already installed")
	})
}

// TestExtensionUpgradeNeverUpgradesToAVersionThatFailsACheck asserts, for every
// check of the Rancher UI and the missing annotation, that a typed version that
// fails only that check is refused with the check's reason and no request, and
// that without --version the default is the older version that passes; an index
// whose only version fails gives the refusal of no compatible version.
func TestExtensionUpgradeNeverUpgradesToAVersionThatFailsACheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		annotations string
		reason      string
	}{
		{name: "kube-version", annotations: extCompatible + `,"catalog.cattle.io/kube-version":">= 1.40.0-0"`, reason: "requires Kubernetes >= 1.40.0-0; the local cluster runs v1.36.5"},
		{name: "rancher-version", annotations: extCompatible + `,"catalog.cattle.io/rancher-version":">= 2.16.0"`, reason: "requires Rancher >= 2.16.0; this server is v2.15.3"},
		{name: "ui-version", annotations: extCompatible + `,"catalog.cattle.io/ui-version":">= 2.16.0"`, reason: "requires Rancher >= 2.16.0; this server is v2.15.3"},
		{name: "ui-extensions-version", annotations: `"catalog.cattle.io/ui-component":"plugins","catalog.cattle.io/ui-extensions-version":">= 9.0.0"`, reason: "requires Extensions API version >= 9.0.0"},
		{name: "ui-extensions-version missing", annotations: `"catalog.cattle.io/ui-component":"plugins"`, reason: "has no annotation catalog.cattle.io/ui-extensions-version, which the Rancher UI requires"},
		{name: "ui-extensions-host", annotations: extCompatible + `,"catalog.cattle.io/ui-extensions-host":"rancher-manager"`, reason: "has ui-extensions-host rancher-manager, which is not compatible with the Rancher UI"},
		{name: "prime-only", annotations: extCompatible + `,"catalog.cattle.io/prime-only":"true"`, reason: "requires Rancher Prime"},
	}
	bad := func(v, ann string) string {
		return `{"name":"epinio","version":"` + v + `","annotations":{` + ann + `}}`
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			putExtApp(f, "deployed", "0.9.0", extRepo)
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + bad("2.0.0", tc.annotations) + `,` + bad("1.0.0", extCompatible) + `]}}`}
			_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart, flagVersion, "2.0.0"))
			require.ErrorContains(t, err, "version '2.0.0' of extension 'epinio' is not compatible with this server: ")
			require.ErrorContains(t, err, tc.reason)
			assert.Equal(t, clierr.CodeValidation, errCode(err))
			assert.Zero(t, f.postCount())

			eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
			assert.Equal(t, "1.0.0", diffPaths(eff)["version"])

			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: `{"entries":{"epinio":[` + bad("2.0.0", tc.annotations) + `]}}`}
			_, _, err = newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
			require.ErrorContains(t, err, "no version of extension 'epinio' in ClusterRepo 'partner-extensions' is compatible with this server; the newest, 2.0.0, ")
			assert.Equal(t, clierr.Rancher+" extension list", clierr.From(err).Hint)
		})
	}
}

// TestExtensionUpgradeRegistryRule asserts the registry the values carry: not
// read when the setting is empty, sent and shown when the setting is set and
// the chart's image repository starts with rancher/, not sent for another
// repository, and a refused setting read is RBAC.
func TestExtensionUpgradeRegistryRule(t *testing.T) {
	t.Parallel()
	info := func(repository string) rootAnswer {
		return rootAnswer{status: 200, body: `{"values":{"image":{"repository":"` + repository + `"}},"chart":{"version":"1.14.3"}}`}
	}
	infoURL := extRepoPath + "?link=info&chartName=epinio&version=1.14.3"
	set := rootAnswer{status: 200, body: `{"id":"system-default-registry","value":"registry.example.test","default":""}`}
	cases := []struct {
		name     string
		setting  rootAnswer
		info     rootAnswer
		registry string
		infoRead bool
		rbac     bool
		refusal  string
		// code and hint are the error's code (VALIDATION when empty) and hint
		// (the extension list command when empty).
		code, hint string
	}{
		{name: "setting empty", setting: rootAnswer{status: 200, body: `{"value":"","default":""}`}},
		{name: "rancher image", setting: set, info: info("rancher/epinio-ui"), registry: "registry.example.test", infoRead: true},
		{name: "other image", setting: set, info: info("other/epinio-ui"), infoRead: true},
		{name: "setting denied", setting: rootAnswer{status: 403}, rbac: true},
		{name: "info of another version", setting: set, infoRead: true,
			info:    rootAnswer{status: 200, body: `{"values":{"image":{"repository":"rancher/epinio-ui"}},"chart":{"version":"1.14.2"}}`},
			refusal: "version '1.14.3' of extension 'epinio' is no longer in ClusterRepo 'partner-extensions' (Rancher returned version '1.14.2' instead)"},
		{name: "version gone from the repository", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart version found for epinio-1.14.3"}`},
			refusal: "version '1.14.3' of extension 'epinio' is no longer in ClusterRepo 'partner-extensions'; name one with --version"},
		{name: "chart gone from the repository", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart name found for epinio"}`},
			refusal: "extension 'epinio' is no longer in ClusterRepo 'partner-extensions'"},
		{name: "chart with no versions", setting: set, infoRead: true,
			info:    rootAnswer{status: 500, body: `{"type":"error","code":"ServerError","message":"no chart version found"}`},
			refusal: "extension 'epinio' has no versions in ClusterRepo 'partner-extensions'"},
		{name: "repository gone", setting: set, infoRead: true, info: rootAnswer{status: 404},
			refusal: "ClusterRepo 'partner-extensions'", code: clierr.CodeNotFound, hint: clierr.Rancher + " get clusterrepos --cluster 'local'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			f.root.answers[settingPath(settingSystemDefaultRegistry)] = tc.setting
			f.root.answers[infoURL] = tc.info
			eff, exec, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
			if tc.rbac {
				assert.Equal(t, clierr.CodeRBAC, errCode(err))
				return
			}
			if tc.refusal != "" {
				require.ErrorContains(t, err, tc.refusal)
				assert.Equal(t, cmp.Or(tc.code, clierr.CodeValidation), errCode(err))
				assert.Equal(t, cmp.Or(tc.hint, clierr.Rancher+" extension list"), clierr.From(err).Hint)
				assert.Equal(t, tc.infoRead, slices.Contains(f.root.requests(), infoURL))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.infoRead, slices.Contains(f.root.requests(), infoURL))
			assert.Equal(t, tc.registry != "", diffPaths(eff)[registryValuePath] != nil)
			_, err = exec(t.Context())
			require.NoError(t, err)
			chart := f.posts[0].body["charts"].([]any)[0].(map[string]any)
			if tc.registry == "" {
				assert.Equal(t, map[string]any{}, chart["values"])
				return
			}
			assert.Equal(t, map[string]any{"global": map[string]any{"cattle": map[string]any{"systemDefaultRegistry": tc.registry}}}, chart["values"])
		})
	}
}

// TestExtensionUpgradeExecuteRechecks asserts that Execute refuses, with no
// request sent, when the App was deleted or changed, an operation started or
// the index entry changed after the preview.
func TestExtensionUpgradeExecuteRechecks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		after func(*extFake)
		text  string
	}{
		{name: "app deleted", after: func(f *extFake) { f.remove("apps", extUIPlugin, extChart) }, text: "app 'epinio' was deleted since the preview"},
		{name: "app changed", after: func(f *extFake) {
			putExtAppMeta(f, extChart, "deployed", extInstalledVersion, extRepo, map[string]any{"resourceVersion": "8"})
		}, text: "app 'epinio' changed since the preview"},
		{name: "operation started", after: func(f *extFake) {
			f.put("operations", extUIPlugin, "helm-operation-b", operation(extChart, testNow.Add(-time.Minute), "True", 1, 1))
		}, text: "operation 'helm-operation-b' for release 'epinio' started since the preview"},
		{name: "index digest changed", after: func(f *extFake) {
			f.root.answers[extIndexPath] = rootAnswer{status: 200, body: strings.ReplaceAll(indexOfExtension(extChart, "1.14.3", "1.14.2"), "sha256:1.14.3", "sha256:other")}
		}, text: "the content of chart 'epinio' '1.14.3' changed in the index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			_, exec := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
			tc.after(f)
			_, err := exec(t.Context())
			require.ErrorContains(t, err, tc.text)
			assert.Equal(t, clierr.CodeConflict, errCode(err))
			assert.Zero(t, f.postCount())
		})
	}
}

// TestExtensionUpgradeWait asserts that --wait follows the operation to the end
// of Helm's run through the existing wait, for the revision after the one the
// App had.
func TestExtensionUpgradeWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newUpgradeFake(t)
		in := extInput(extChart)
		in.b[flagWait] = true
		_, exec := plan(t, newTestExtensionUpgrade(f), in)
		f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
			"version": int64(2), "info": map[string]any{"status": "deployed"},
			"chart": map[string]any{"metadata": map[string]any{"annotations": map[string]any{annotationRequestID: testRequestID}}},
		}}))
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Contains(t, res.Text, "app epinio: upgraded (")
		assert.Contains(t, res.Text, "status deployed)")
	})
}

// TestExtensionUpgradeWaitFailedRun asserts that --wait reports an error, and
// not a finished upgrade, when Helm's run ended with the App in the status
// failed, and when the App still has the revision it had before the request.
func TestExtensionUpgradeWaitFailedRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		version int64
		status  string
		mine    bool
		text    string
	}{
		{name: "failed at the next revision", version: 2, status: "failed", mine: true, text: "the upgrade of release 'epinio' failed"},
		{name: "the revision unchanged", version: 1, status: "deployed", text: "timed out after 55m waiting for release 'epinio'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newUpgradeFake(t)
				in := extInput(extChart)
				in.b[flagWait] = true
				_, exec := plan(t, newTestExtensionUpgrade(f), in)
				f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
				annotations := map[string]any{}
				if tc.mine {
					annotations[annotationRequestID] = testRequestID
				}
				f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
					"version": tc.version, "info": map[string]any{"status": tc.status},
					"chart": map[string]any{"metadata": map[string]any{"annotations": annotations}},
				}}))
				res, err := exec(t.Context())
				require.ErrorContains(t, err, tc.text)
				assert.NotContains(t, res.Text, "upgraded (")
			})
		})
	}
}

// TestExtensionUpgradePlanDisplayIdentity asserts the Effect names the app and
// the chart's name, with no "*" marker in the identity that is committed.
func TestExtensionUpgradePlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.NotContains(t, eff.DisplayName, "*")
	assert.Equal(t, "apps/"+extChart, eff.Target)
}

// TestExtensionUpgradeVersionWarnings asserts the preview warns when the Kubernetes
// version of the local cluster could not be read while a version asks for one,
// and when the Extensions API version of the server is not known, for the
// default version and for a typed --version.
func TestExtensionUpgradeVersionWarnings(t *testing.T) {
	t.Parallel()
	entry := func(version, extra string) string {
		return `{"name":"epinio","version":"` + version + `","digest":"sha256:` + version + `","annotations":{` + extCompatible + extra + `}}`
	}
	kubeIndex := `{"entries":{"epinio":[` + entry("1.14.4", `,"catalog.cattle.io/kube-version":">= 1.20.0"`) + `,` + entry("1.14.3", "") + `,` + entry("1.14.2", "") + `]}}`
	unread := func(f *extFake) {
		f.root.answers[localCluster] = rootAnswer{status: 403}
		f.root.answers[extIndexPath] = rootAnswer{status: 200, body: kubeIndex}
	}
	unknown := func(f *extFake) { f.version = "v2.99.0" }
	cases := []struct {
		name    string
		setup   func(*extFake)
		flags   []string
		warning string
	}{
		{name: "Kubernetes version unread", setup: unread, warning: "the Kubernetes version of the local cluster could not be read"},
		{name: "Extensions API version unknown", setup: unknown, warning: "the Extensions API version of Rancher v2.99.0 is not known"},
		{name: "Extensions API version unknown for a typed version", setup: unknown, flags: []string{flagVersion, "1.14.3"}, warning: "the Extensions API version of Rancher v2.99.0 is not known"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			tc.setup(f)
			eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart, tc.flags...))
			assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool { return strings.Contains(w, tc.warning) }), "%q in %q", tc.warning, eff.Warnings)
		})
	}
}

// TestExtensionUpgradeRBAC asserts a refused read or action is the server's
// RBAC refusal, and a refused read of the local Cluster is only a warning.
func TestExtensionUpgradeRBAC(t *testing.T) {
	t.Parallel()
	for name, path := range map[string]string{"repositories": reposPath, "index": extIndexPath} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			f.root.answers[path] = rootAnswer{status: 403}
			_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
			assert.Equal(t, clierr.CodeRBAC, errCode(err))
		})
	}
	t.Run("the App", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		f.fail("get:apps/"+extUIPlugin+"/"+extChart, clierr.FromResponse("GET", "GET", 403, []byte(`{}`), nil))
		_, _, err := newTestExtensionUpgrade(f).Plan(t.Context(), extInput(extChart))
		assert.Equal(t, clierr.CodeRBAC, errCode(err))
	})
	t.Run("the action", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		f.postAnswer = func(string) ([]byte, error) {
			return nil, &engine.ActionRefused{Err: clierr.FromResponse("POST", "POST", 403, []byte(`{}`), nil), Body: []byte(`{}`)}
		}
		_, exec := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
		_, err := exec(t.Context())
		assert.Equal(t, clierr.CodeRBAC, errCode(err))
	})
	t.Run("the local Cluster", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		f.root.answers[localCluster] = rootAnswer{status: 403}
		eff, _ := plan(t, newTestExtensionUpgrade(f), extInput(extChart))
		assert.Equal(t, "1.14.3", diffPaths(eff)["version"])
	})
}

var _ runtime.Command = (*extensionUpgradeCmd)(nil)
