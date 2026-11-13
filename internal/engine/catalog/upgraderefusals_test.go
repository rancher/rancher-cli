package catalog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upgradeInfoOf is an info answer for version whose chart carries the
// annotations.
func upgradeInfoOf(version string, annotations map[string]string) string {
	b, err := json.Marshal(map[string]any{
		"values": map[string]any{"replicaCount": 1},
		"chart":  map[string]any{"annotations": annotations, "version": version},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// annotateVersion makes the info answer of version carry the annotations.
func annotateVersion(f *appFake, version string, annotations map[string]string) {
	f.root.answers[infoPath(testChart, version)] = rootAnswer{status: 200, body: upgradeInfoOf(version, annotations)}
}

// runningOperationFixture is an Operation of the release created 10 minutes
// before the fake's clock that is still reconciling.
func runningOperationFixture(f *appFake) {
	f.put("operations", testNS, "helm-operation-x1", operation(testRelease, testNow.Add(-10*time.Minute), "True", 1, 1))
}

// windowsWorkers makes the management Cluster hold Windows workers.
func windowsWorkers(f *appFake) {
	f.root.answers[clusterPath] = rootAnswer{status: 200, body: `{"status":{"windowsWorkerCount":1,"linuxWorkerCount":1}}`}
}

// refusalCase is one refusal of the Plan of an upgrade.
type refusalCase struct {
	name  string
	app   func(*upgradeApp)
	setup func(*appFake)
	edit  func(*appInput)
	code  string
	exit  int
	msg   string
	hint  string
}

// upgradeRefusalCases are the refusals of the Plan of an upgrade, each with
// the fake it needs and the error it ends with.
func upgradeRefusalCases() []refusalCase {
	getApp := clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	getOperation := clierr.Rancher + " get operation 'helm-operation-x1' --cluster 'prod' --namespace 'web'"
	chartsHint := clierr.Rancher + " charts --cluster 'prod' --repo 'repo1'"
	status := func(s string) func(*upgradeApp) { return func(a *upgradeApp) { a.status = s } }
	version := withFlag(flagVersion, "2.0.0")
	running := "operation 'helm-operation-x1' for release 'my-nginx' in namespace 'web' may still be running"
	return []refusalCase{
		{name: "uninstalled", app: status("uninstalled"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "release 'my-nginx' is uninstalled and Helm keeps its history; Helm would install it again rather than upgrade it; " +
				"install it under another release name with " + clierr.Rancher + " app install"},
		{name: "uninstalling", app: status("uninstalling"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "release 'my-nginx' is uninstalling and Helm has no deployed revision to upgrade; read it with " + getApp},
		{name: "pending install", app: status("pending-install"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "Helm refuses an upgrade while release 'my-nginx' is pending-install; to remove it, add --apply --yes after reading the preview of " + uninstallCmdOf},
		{name: "pending upgrade", app: status("pending-upgrade"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "Helm refuses an upgrade while release 'my-nginx' is pending-upgrade"},
		{name: "pending rollback", app: status("pending-rollback"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "Helm refuses an upgrade while release 'my-nginx' is pending-rollback"},
		{name: "pending install with an operation running", app: status("pending-install"), setup: func(f *appFake) { runningOperationFixture(f) },
			code: clierr.CodeConflict, exit: 5, hint: getOperation, msg: running},
		{name: "pending upgrade with an operation running", app: status("pending-upgrade"), setup: func(f *appFake) { runningOperationFixture(f) },
			code: clierr.CodeConflict, exit: 5, hint: getOperation, msg: running},
		{name: "pending rollback with an operation running", app: status("pending-rollback"), setup: func(f *appFake) { runningOperationFixture(f) },
			code: clierr.CodeConflict, exit: 5, hint: getOperation, msg: running},
		{name: "deployed with an operation running", setup: func(f *appFake) { runningOperationFixture(f) },
			code: clierr.CodeConflict, exit: 5, hint: getOperation, msg: running},
		{name: "status empty", app: status(""), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "release 'my-nginx' has the status unknown, and Helm upgrades only a deployed, failed or superseded release"},
		{name: "status unknown", app: status("unknown"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "release 'my-nginx' has the status unknown, and Helm upgrades only a deployed, failed or superseded release"},
		{name: "another status", app: status("rolled-back"), code: clierr.CodeConflict, exit: 5, hint: getApp,
			msg: "release 'my-nginx' has the status rolled-back"},
		{name: "app absent", setup: func(f *appFake) { f.remove("apps", testNS, testRelease) },
			code: clierr.CodeNotFound, exit: 4, hint: clierr.Rancher + " get apps --cluster 'prod' --namespace 'web'",
			msg: "app 'my-nginx' not found in namespace 'web'"},
		{name: "different chart", edit: withFlag(flagChart, "other"), code: clierr.CodeValidation, exit: 2,
			msg: "release 'my-nginx' has chart 'nginx' installed, and an upgrade cannot change the chart; to install chart 'other' as a new release, run " + clierr.Rancher + " app install"},
		{name: "auto-install target", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/auto-install": "nginx-crd=match"})
			},
			code: clierr.CodeValidation, exit: 2,
			msg: "chart 'nginx' '2.0.0' names nginx-crd to install first (annotation catalog.cattle.io/auto-install); " +
				"rancher-cli does not upgrade a chart that names another chart to install first; upgrade it from the Rancher UI, which upgrades the chart it names too"},
		{name: "auto-install-gvr target", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/auto-install-gvr": "example.io/v1/Thing"})
			},
			code: clierr.CodeValidation, exit: 2, msg: "names example.io/v1/Thing to install first (annotation catalog.cattle.io/auto-install-gvr)"},
		{name: "auto-install on the kept version",
			setup: func(f *appFake) {
				annotateVersion(f, "1.0.0", map[string]string{"catalog.cattle.io/auto-install": "nginx-crd=match"})
			},
			code: clierr.CodeValidation, exit: 2, msg: "chart 'nginx' '1.0.0' names nginx-crd to install first"},
		{name: "namespace annotation on another version", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/namespace": "cattle-system"})
			},
			code: clierr.CodeValidation, exit: 2,
			msg: "chart 'nginx' '2.0.0' installs only in namespace 'cattle-system' (annotation catalog.cattle.io/namespace), and release 'my-nginx' is in namespace 'web'; " +
				"stay on the installed version '1.0.0', or install chart 'nginx' '2.0.0' as a new release in 'cattle-system' with " + clierr.Rancher + " app install"},
		{name: "release-name annotation on another version", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/release-name": "nginx-system"})
			},
			code: clierr.CodeValidation, exit: 2,
			msg: "chart 'nginx' '2.0.0' installs only as release 'nginx-system' (annotation catalog.cattle.io/release-name); " +
				"stay on the installed version '1.0.0', or install chart 'nginx' '2.0.0' as release 'nginx-system' with " + clierr.Rancher + " app install"},
		{name: "requires-gvr on another version", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/requires-gvr": "monitoring.coreos.com.prometheus"})
			},
			code: clierr.CodeValidation, exit: 2,
			msg: "chart 'nginx' '2.0.0' needs the resource monitoring.coreos.com.prometheus on the cluster (annotation catalog.cattle.io/requires-gvr), and the cluster does not serve it"},
		{name: "requires-gvr on the kept version",
			setup: func(f *appFake) {
				annotateVersion(f, "1.0.0", map[string]string{"catalog.cattle.io/requires-gvr": "monitoring.coreos.com.prometheus"})
			},
			code: clierr.CodeValidation, exit: 2, msg: "chart 'nginx' '1.0.0' needs the resource monitoring.coreos.com.prometheus on the cluster"},
		{name: "permits-os on another version", edit: version,
			setup: func(f *appFake) {
				annotateVersion(f, "2.0.0", map[string]string{"catalog.cattle.io/permits-os": "linux"})
				windowsWorkers(f)
			},
			code: clierr.CodeValidation, exit: 2,
			msg: "chart 'nginx' '2.0.0' permits only linux nodes (annotation catalog.cattle.io/permits-os), and cluster 'prod' has windows workers"},
		{name: "permits-os on the kept version",
			setup: func(f *appFake) {
				annotateVersion(f, "1.0.0", map[string]string{"catalog.cattle.io/permits-os": "linux"})
				windowsWorkers(f)
			},
			code: clierr.CodeValidation, exit: 2, msg: "chart 'nginx' '1.0.0' permits only linux nodes"},
		{name: "no source and no --repo", app: func(a *upgradeApp) { a.noSource = true }, code: clierr.CodeValidation, exit: 2,
			hint: clierr.Rancher + " get clusterrepos --cluster 'prod'",
			msg:  "app 'my-nginx' does not say which ClusterRepo it came from; name one with --repo"},
		{name: "ClusterRepo disabled", setup: func(f *appFake) {
			f.put("clusterrepos", "", testRepo, map[string]any{"spec": map[string]any{"url": "https://charts.example.test", "enabled": false}})
		}, code: clierr.CodeValidation, exit: 2, hint: clierr.Rancher + " get clusterrepo 'repo1' --cluster 'prod'",
			msg: "ClusterRepo 'repo1' is disabled"},
		{name: "ClusterRepo absent", setup: func(f *appFake) { f.remove("clusterrepos", "", testRepo) },
			code: clierr.CodeNotFound, exit: 4, hint: clierr.Rancher + " get clusterrepos --cluster 'prod'",
			msg: "ClusterRepo 'repo1' not found"},
		{name: "version not in the index", edit: withFlag(flagVersion, "9.9.9"), code: clierr.CodeValidation, exit: 2, hint: chartsHint + " --all-versions",
			msg: "version '9.9.9' of chart 'nginx' is not in the index ClusterRepo 'repo1' serves for this cluster"},
		{name: "chart not in the index", setup: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{"other": {{Version: "1.0.0"}}})}
		}, edit: version, code: clierr.CodeValidation, exit: 2, hint: chartsHint,
			msg: "chart 'nginx' is not in the index ClusterRepo 'repo1' serves for this cluster"},
	}
}

// TestUpgradeRefusals asserts each refusal of the Plan of an upgrade: its
// code and exit code, that it is never retryable, its text and hint, and
// that nothing was sent.
func TestUpgradeRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range upgradeRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*upgradeApp)
			if tc.app != nil {
				edits = append(edits, tc.app)
			}
			f := upgradeFake(t, nil, edits...)
			if tc.setup != nil {
				tc.setup(f)
			}
			in := upgradeInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			ce := upgradeRefusal(t, f, nil, in)
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.exit, ce.ExitCode())
			assert.False(t, ce.Retryable, "the remedy of a refusal says what to do first")
			assert.Contains(t, ce.Message, tc.msg)
			assert.Equal(t, tc.hint, ce.Hint)
		})
	}
}

// TestUpgradeAppAbsentReadsNothingElse asserts an upgrade of a release with no
// App stops at the App read: no request to the server's index, info or
// Secrets and no POST, which the server would turn into an install.
func TestUpgradeAppAbsentReadsNothingElse(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	f.remove("apps", testNS, testRelease)
	ce := upgradeRefusal(t, f, nil, upgradeInput(withFlag(flagVersion, "2.0.0")))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, f.root.requests())
}

// TestUpgradeAllowedStatuses asserts an App with the status deployed, failed
// or superseded gets a plan.
func TestUpgradeAllowedStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"deployed", "failed", "superseded"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			a := f.putUpgradeApp(func(a *upgradeApp) { a.status = status })
			f.putRelease(a, releaseFixture{status: status})
			eff, _, err := planUpgrade(t, f, nil, upgradeInput())
			require.NoError(t, err)
			assert.Equal(t, "apps/"+testRelease, eff.Target)
		})
	}
}

// TestUpgradeChecksThatPassOnTheKeptVersion asserts the namespace and
// release-name annotations of the version the App already runs are not
// checked, omitted or typed: the release is in that namespace under that
// name.
func TestUpgradeChecksThatPassOnTheKeptVersion(t *testing.T) {
	t.Parallel()
	annotations := map[string]map[string]string{
		"namespace":    {"catalog.cattle.io/namespace": "cattle-system"},
		"release-name": {"catalog.cattle.io/release-name": "nginx-system"},
		"both":         {"catalog.cattle.io/namespace": "cattle-system", "catalog.cattle.io/release-name": "nginx-system"},
	}
	for name, ann := range annotations {
		for how, edit := range map[string]func(*appInput){"omitted": func(*appInput) {}, "typed": withFlag(flagVersion, "1.0.0")} {
			t.Run(name+" "+how, func(t *testing.T) {
				t.Parallel()
				f := upgradeFake(t, nil)
				annotateVersion(f, "1.0.0", ann)
				_, _, err := planUpgrade(t, f, nil, upgradeInput(edit))
				require.NoError(t, err)
			})
		}
	}
}

// TestUpgradeChecksThatPassOnTheTarget asserts annotations that the target
// meets give a plan: the namespace and release name of the release, and a
// linux-only chart on a cluster with linux workers.
func TestUpgradeChecksThatPassOnTheTarget(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	annotateVersion(f, "2.0.0", map[string]string{
		"catalog.cattle.io/namespace": testNS, "catalog.cattle.io/release-name": testRelease, "catalog.cattle.io/permits-os": "linux",
	})
	_, _, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagVersion, "2.0.0")))
	require.NoError(t, err)
}

// TestUpgradeNothingToChange asserts an upgrade of a release that already
// runs the version with the stored values is VALIDATION naming the release,
// chart and version, with the get app hint.
func TestUpgradeNothingToChange(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, storedValues(nil))
	ce := upgradeRefusal(t, f, nil, upgradeInput())
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 2, ce.ExitCode())
	assert.False(t, ce.Retryable)
	assert.Equal(t, "release 'my-nginx' already has version '1.0.0' of chart 'nginx' installed with these values; nothing to change", ce.Message)
	assert.Equal(t, clierr.Rancher+" get app 'my-nginx' --cluster 'prod' --namespace 'web'", ce.Hint)
}

// TestUpgradeRepoOtherThanTheSource asserts a --repo other than the App's
// source ClusterRepo is allowed, shows the change in the Diff and warns that
// uninstall then runs as that repository's service account; the request goes
// to the repository named, and a --repo that is the source changes nothing.
func TestUpgradeRepoOtherThanTheSource(t *testing.T) {
	t.Parallel()
	const repo2 = "repo2"
	f := upgradeFake(t, nil)
	f.put("clusterrepos", "", repo2, map[string]any{"spec": map[string]any{"url": "https://other.example.test"}})
	base := "/k8s/clusters/" + testCluster + "/v1/catalog.cattle.io.clusterrepos/" + repo2
	f.root.answers[base+"?link=index"] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0", Digest: "sha256:r2"}}})}
	f.root.answers[base+"?link=info&chartName=nginx&version=1.0.0"] = rootAnswer{status: 200, body: upgradeInfoOf("1.0.0", nil)}
	eff, exec, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagRepo, repo2)))
	require.NoError(t, err)
	var repo []any
	for _, d := range eff.Diff {
		if d.Path == "repo" {
			repo = []any{d.From, d.To}
		}
	}
	assert.Equal(t, []any{testRepo, repo2}, repo)
	assert.Contains(t, eff.Warnings, "the app will record ClusterRepo 'repo2' as the one it came from; a later uninstall of this app runs Helm as the service account in spec.serviceAccount of that ClusterRepo, when it sets one")
	_, err = exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "/k8s/clusters/"+testCluster+"/v1/catalog.cattle.io.clusterrepos/repo2?action=upgrade", f.posts[0].path)
	ann := upgradeChartOf(t, f.posts[0].body)["annotations"].(map[string]any)
	assert.Equal(t, repo2, ann[annotationSourceRepo])

	same, _, err := planUpgrade(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagRepo, testRepo)))
	require.NoError(t, err)
	for _, w := range same.Warnings {
		assert.NotContains(t, w, "the app will record ClusterRepo")
	}
}

// TestUpgradeNoSourceWithRepo asserts an App with no source annotation can be
// upgraded with --repo, which then sets the source with no earlier value.
func TestUpgradeNoSourceWithRepo(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil, func(a *upgradeApp) { a.noSource = true })
	eff, _, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagRepo, testRepo)))
	require.NoError(t, err)
	assert.Contains(t, eff.Warnings, "the app will record ClusterRepo 'repo1' as the one it came from; a later uninstall of this app runs Helm as the service account in spec.serviceAccount of that ClusterRepo, when it sets one")
	assert.Equal(t, testRepo, diffPaths(eff)["repo"])
}
