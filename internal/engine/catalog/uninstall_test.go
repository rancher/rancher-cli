package catalog

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// deployedApp stores the App of the release with chart annotations.
func deployedApp(f *appFake, status string, annotations map[string]any) {
	f.put("apps", testNS, testRelease, map[string]any{"spec": map[string]any{
		"name": testRelease,
		"info": map[string]any{"status": status},
		"chart": map[string]any{"metadata": map[string]any{
			"name": testChart, "version": "2.0.0", "annotations": annotations,
		}},
	}})
}

// TestUninstallPreview asserts the Effect of an uninstall: a destructive
// delete of the App previewed from server reads, with no precondition, the
// release, chart and what Helm removes in the diff, and no request sent.
func TestUninstallPreview(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", nil)
	eff, _ := plan(t, newTestUninstall(f), uninstallInput())
	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.Equal(t, "apps/"+testRelease, eff.Target)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, testRelease, eff.DisplayName)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.Equal(t, testCluster, eff.Cluster)
	assert.Empty(t, eff.ResourceVersion)
	assert.False(t, eff.ConflictDetection)
	assert.False(t, eff.Reversible)
	assert.True(t, eff.Destructive)
	assert.Equal(t, map[string]any{
		"releaseName": testRelease, "namespace": testNS, "chart": testChart, "version": "2.0.0",
		"resources": "the release's resources",
	}, diffPaths(eff))
	assert.Equal(t, []string{
		"Rancher runs Helm in a pod as you; the chart's own writes are checked only when Helm runs them",
		"this preview comes from server reads; the CLI does not send the uninstall action's dry-run option",
		"Rancher may delete image pull secrets for this release; the preview cannot list them",
	}, eff.Warnings)
	assert.Zero(t, f.postCount())
}

// TestUninstallBody asserts the body: the flags given and nothing else,
// never dryRun, and the timeout only when typed.
func TestUninstallBody(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(*appInput)
		want map[string]any
	}{
		"bare":         {func(*appInput) {}, map[string]any{}},
		"keep history": {func(in *appInput) { in.b[flagKeepHistory] = true }, map[string]any{"keepHistory": true}},
		"no hooks":     {func(in *appInput) { in.b[flagNoHooks] = true }, map[string]any{"noHooks": true}},
		"helm timeout": {func(in *appInput) { in.s[flagHelmTimeout] = "2m" }, map[string]any{"timeout": "2m0s"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			deployedApp(f, "deployed", nil)
			_, exec := plan(t, newTestUninstall(f), uninstallInput(tc.edit))
			_, err := exec(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, f.postCount())
			assert.Equal(t, "/k8s/clusters/c-1/v1/catalog.cattle.io.apps/web/my-nginx?action=uninstall", f.posts[0].path)
			assert.Equal(t, tc.want, f.posts[0].body)
			for field, row := range map[string]string{"keepHistory": flagKeepHistory, "noHooks": flagNoHooks, "timeout": flagHelmTimeout} {
				assert.Equal(t, field, uninstallFlagFields[row].field)
			}
		})
	}
}

// TestUninstallAppStates asserts a missing App is NOT_FOUND naming the
// command that lists the namespace's apps, an uninstalled App with
// --keep-history is VALIDATION, and without it the preview says the kept
// history goes.
func TestUninstallAppStates(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	_, _, err := newTestUninstall(f).Plan(t.Context(), uninstallInput())
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, clierr.Rancher+" get apps --cluster 'prod' --namespace 'web'", ce.Hint)

	deployedApp(f, "uninstalled", nil)
	_, _, err = newTestUninstall(f).Plan(t.Context(), uninstallInput(func(in *appInput) { in.b[flagKeepHistory] = true }))
	require.ErrorContains(t, err, "release 'my-nginx' is already uninstalled; run without --keep-history to remove its kept history")
	assert.Equal(t, clierr.CodeValidation, clierr.From(err).Code)

	eff, _ := plan(t, newTestUninstall(f), uninstallInput())
	assert.Contains(t, eff.Warnings, "release 'my-nginx' is already uninstalled; this removes its kept history")
}

// TestUninstallIdentity asserts who the preview says Helm runs as: the
// service account of the ClusterRepo the App's source annotations name, in
// the App's namespace, else the caller.
func TestUninstallIdentity(t *testing.T) {
	t.Parallel()
	source := map[string]any{annotationSourceRepoType: "cluster", annotationSourceRepo: testRepo}
	cases := map[string]struct {
		ann      map[string]any
		spec     map[string]any
		identity string
	}{
		"no source annotations":      {nil, map[string]any{"serviceAccount": "helm"}, "you"},
		"repository with an account": {source, map[string]any{"serviceAccount": "helm"}, "system:serviceaccount:web:helm"},
		"repository without one":     {source, map[string]any{}, "you"},
		"colon in its namespace":     {source, map[string]any{"serviceAccount": "helm", "serviceAccountNamespace": "a:b"}, "you"},
		"another source type":        {map[string]any{annotationSourceRepoType: "namespace", annotationSourceRepo: testRepo}, map[string]any{"serviceAccount": "helm"}, "you"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			deployedApp(f, "deployed", tc.ann)
			f.put("clusterrepos", "", testRepo, map[string]any{"spec": tc.spec})
			eff, _ := plan(t, newTestUninstall(f), uninstallInput())
			assert.Equal(t, identityWarning(tc.identity), eff.Warnings[0])
		})
	}
}

// TestUninstallCRDAppWarning asserts an App whose chart installed a CRD
// chart with it warns that the CRD chart stays and names the command that
// lists the namespace's apps.
func TestUninstallCRDAppWarning(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", map[string]any{annotationAutoInstall: "nginx-crd=match"})
	eff, _ := plan(t, newTestUninstall(f), uninstallInput())
	assert.Contains(t, eff.Warnings, "the chart installed nginx-crd with it, which stays installed; find its release with "+
		clierr.Rancher+" get apps --cluster 'prod' --namespace 'web'")
}

// TestUninstallExecuteRereads asserts Execute refuses, with a retryable
// CONFLICT and no request, an App that changed or went away and an
// operation that started since the preview.
func TestUninstallExecuteRereads(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		change func(*appFake)
		err    string
	}{
		"app changed": {func(f *appFake) {
			obj := f.put("apps", testNS, testRelease, map[string]any{})
			obj.SetResourceVersion("2")
		}, "app 'my-nginx' changed since the preview"},
		"app removed":       {func(f *appFake) { f.remove("apps", testNS, testRelease) }, "app 'my-nginx' was removed since the preview"},
		"operation started": {func(f *appFake) { f.put("operations", testNS, "op", operation(testRelease, testNow, "True", 1, 1)) }, "operation 'op' for release 'my-nginx' started"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			deployedApp(f, "deployed", nil)
			_, exec := plan(t, newTestUninstall(f), uninstallInput())
			tc.change(f)
			_, err := exec(t.Context())
			require.ErrorContains(t, err, tc.err)
			ce := clierr.From(err)
			assert.Equal(t, clierr.CodeConflict, ce.Code)
			assert.True(t, ce.Retryable)
			assert.Zero(t, f.postCount())
		})
	}
}

// TestUninstallResult asserts the result names the operation and the
// follow-up commands and carries no request id.
func TestUninstallResult(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", nil)
	_, exec := plan(t, newTestUninstall(f), uninstallInput())
	res, err := exec(t.Context())
	require.NoError(t, err)
	r := res.Data.(result)
	assert.Empty(t, r.RequestID)
	assert.Equal(t, "helm-operation-abcde", r.OperationName)
	assert.Len(t, r.FollowUp, 2)
}

// TestUninstallPlanDisplayIdentity asserts the preview names the App by the
// release, DisplayKind app, and the committed result names the same
// release, never with the dry-run preview's "*" marker.
func TestUninstallPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", nil)
	eff, exec := plan(t, newTestUninstall(f), uninstallInput())
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, testRelease, eff.DisplayName)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, testRelease, res.Data.(result).Release)
	assert.NotContains(t, res.Text, "*")
	assert.True(t, strings.HasPrefix(res.Text, "app "+testRelease+":"), res.Text)
}

// TestUninstallCRDWarningNamesAutoInstallChartsOnly asserts the warning
// names only the charts of catalog.cattle.io/auto-install, as the Rancher
// UI does; an auto-install-gvr entry names no chart and gives no warning.
func TestUninstallCRDWarningNamesAutoInstallChartsOnly(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", map[string]any{annotationAutoInstallGVR: "example.io.thing"})
	eff, _ := plan(t, newTestUninstall(f), uninstallInput())
	for _, w := range eff.Warnings {
		assert.NotContains(t, w, "stays installed")
	}
}

// TestUninstallUsesTheAppReleaseName asserts the release name is the App's
// spec.name, which Rancher records on the Operation: an Operation of that
// name refuses the uninstall, and the preview names it.
func TestUninstallUsesTheAppReleaseName(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", nil)
	require.NoError(t, setReleaseName(f, "helm-release"))
	eff, _ := plan(t, newTestUninstall(f), uninstallInput())
	assert.Equal(t, "helm-release", diffPaths(eff)["releaseName"])

	f.put("operations", testNS, "helm-operation-y", operation("helm-release", testNow, "True", 1, 1))
	_, _, err := newTestUninstall(f).Plan(t.Context(), uninstallInput())
	require.ErrorContains(t, err, "operation 'helm-operation-y' for release 'helm-release'")
}

// setReleaseName gives the stored App the spec.name name.
func setReleaseName(f *appFake, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := f.objs[objKey("apps", testNS, testRelease)]
	return unstructured.SetNestedField(obj.Object, name, "spec", "name")
}

// TestUninstallResultCarriesNoInstallNote asserts the uninstall result does
// not carry the install's warning about a chart that leaves no app.
func TestUninstallResultCarriesNoInstallNote(t *testing.T) {
	t.Parallel()
	f := newAppFake(t)
	deployedApp(f, "deployed", nil)
	_, exec := plan(t, newTestUninstall(f), uninstallInput())
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, res.Warnings, noAppNote)
	assert.NotContains(t, res.Text, noAppNote)
}
