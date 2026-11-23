package catalog

import (
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

func newTestExtensionUninstall(f *extFake) *extensionUninstallCmd {
	return &extensionUninstallCmd{clients: f, actions: f, now: func() time.Time { return testNow }, interval: time.Second}
}

// TestExtensionUninstallPreview asserts the Effect of an uninstall: a delete of
// the App from server reads with no precondition, destructive, the release, its
// namespace and chart as they are now.
func TestExtensionUninstallPreview(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	eff, _ := plan(t, newTestExtensionUninstall(f), extInput(extChart))
	assert.Equal(t, safety.ActionDelete, eff.Action)
	assert.Equal(t, safety.PreviewServerReads, eff.PreviewSource)
	assert.True(t, eff.Destructive)
	assert.False(t, eff.Reversible)
	assert.False(t, eff.ConflictDetection)
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.Equal(t, engine.LocalCluster, eff.Cluster)
	assert.Equal(t, map[string]any{"releaseName": extChart, "namespace": extUIPlugin, "chart": extChart, "version": extInstalledVersion, "resources": "the release's resources"}, diffPaths(eff))
	assert.Contains(t, eff.Warnings, warnServerReadsUninstall)
	assert.Zero(t, f.postCount())
}

// TestExtensionUninstallBody asserts the request: an empty object to the
// uninstall action of the App on the local cluster, for an App with no UIPlugin.
func TestExtensionUninstallBody(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	_, exec := plan(t, newTestExtensionUninstall(f), extInput(extChart))
	_, err := exec(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, f.postCount())
	p := f.posts[0]
	assert.Equal(t, "/v1/catalog.cattle.io.apps/"+extUIPlugin+"/"+extChart+"?action=uninstall", p.path)
	assert.NotContains(t, p.path, "/k8s/clusters/")
	assert.Empty(t, p.body)
}

// TestExtensionUninstallRefusals asserts each refusal of the uninstall plan:
// its code, its text and that nothing is sent, and that an App of any other
// status is uninstalled.
func TestExtensionUninstallRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(*extFake)
		code  string
		text  string
		hint  string
	}{
		{name: "not installed", code: clierr.CodeNotFound,
			setup: func(f *extFake) { f.remove("apps", extUIPlugin, extChart) },
			text:  "extension 'epinio' is not installed: no App 'epinio' in namespace cattle-ui-plugin-system",
			hint:  clierr.Rancher + " extension list"},
		{name: "loaded from a URL", code: clierr.CodeNotFound,
			setup: func(f *extFake) {
				f.remove("apps", extUIPlugin, extChart)
				f.put("uiplugins", "other-ns", "dev-load", map[string]any{"spec": map[string]any{"plugin": map[string]any{"name": extChart, "version": "0.1.0"}}})
			},
			text: "extension 'epinio' has no App in namespace cattle-ui-plugin-system; it was loaded from a URL (UIPlugin 'other-ns/dev-load'), and it is uninstalled in the Rancher UI"},
		{name: "operation running", code: clierr.CodeConflict,
			setup: func(f *extFake) {
				f.put("operations", extUIPlugin, "helm-operation-a", operation(extChart, testNow.Add(-time.Minute), "True", 1, 1))
			},
			text: "operation 'helm-operation-a' for release 'epinio' in namespace 'cattle-ui-plugin-system' may still be running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			tc.setup(f)
			_, _, err := newTestExtensionUninstall(f).Plan(t.Context(), extInput(extChart))
			require.Error(t, err)
			ce := clierr.From(err)
			assert.Equal(t, tc.code, ce.Code, ce.Message)
			require.ErrorContains(t, err, tc.text)
			if tc.hint != "" {
				assert.Equal(t, tc.hint, ce.Hint)
			}
			assert.Zero(t, f.postCount())
		})
	}
	for _, status := range []string{"failed", "pending-install", "superseded"} {
		t.Run("status "+status, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			putExtApp(f, status, extInstalledVersion, extRepo)
			_, exec := plan(t, newTestExtensionUninstall(f), extInput(extChart))
			_, err := exec(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, f.postCount())
		})
	}
}

// TestExtensionUninstallKeptHistory asserts that the preview of an App with the
// status uninstalled has a warning that the command removes the history Helm
// kept.
func TestExtensionUninstallKeptHistory(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	putExtApp(f, "uninstalled", extInstalledVersion, extRepo)
	eff, _ := plan(t, newTestExtensionUninstall(f), extInput(extChart))
	assert.Contains(t, eff.Warnings, "release 'epinio' is already uninstalled; this removes its kept history")
}

// TestExtensionUninstallExecuteRechecks asserts that Execute refuses, with no
// request sent, when the App was removed or changed or an operation started
// after the preview.
func TestExtensionUninstallExecuteRechecks(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpgradeFake(t)
			_, exec := plan(t, newTestExtensionUninstall(f), extInput(extChart))
			tc.after(f)
			_, err := exec(t.Context())
			require.ErrorContains(t, err, tc.text)
			assert.Equal(t, clierr.CodeConflict, errCode(err))
			assert.Zero(t, f.postCount())
		})
	}
}

// TestExtensionUninstallWait asserts that --wait follows the operation to the
// end of Helm's run and finds the App gone.
func TestExtensionUninstallWait(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newUpgradeFake(t)
		in := extInput(extChart)
		in.b[flagWait] = true
		_, exec := plan(t, newTestExtensionUninstall(f), in)
		f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, absent)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Contains(t, res.Text, "app epinio: uninstalled")
	})
}

// TestExtensionUninstallWaitFailedRun asserts that --wait reports an error,
// and not a finished uninstall, when Helm's run ended with the App still
// there. The App can have the status it had before the request, the status
// failed or the status uninstalled; the status uninstalled gives the
// kept-history error whether or not the App had it before the request.
func TestExtensionUninstallWaitFailedRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after, message string }{
		{"unchanged", "deployed", "deployed", "operation 'helm-operation-abcde' ended; release 'epinio' is at status deployed; the release stays installed"},
		{"failed", "deployed", "failed", "operation 'helm-operation-abcde' ended; release 'epinio' is at status failed; the release stays installed"},
		{"history kept before and after", "uninstalled", "uninstalled", "release 'epinio' is at status uninstalled; its kept history was not removed"},
		{"history kept by the request", "deployed", "uninstalled", "release 'epinio' is at status uninstalled; its kept history was not removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newUpgradeFake(t)
				putExtApp(f, tc.before, extInstalledVersion, extRepo)
				in := extInput(extChart)
				in.b[flagWait] = true
				_, exec := plan(t, newTestExtensionUninstall(f), in)
				f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opDone)
				f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
					"version": int64(1), "info": map[string]any{"status": tc.after},
				}}))
				res, err := exec(t.Context())
				require.Error(t, err)
				assert.Equal(t, tc.message, clierr.From(err).Message)
				assert.NotContains(t, res.Text, "uninstalled")
			})
		})
	}
}

// TestExtensionUninstallWaitStalledRun asserts that --wait keeps the exit code
// of a Stalled Operation when the App is still there: an App that kept its
// status says the release stays installed, and an App that had the status
// uninstalled before the request and still has it says its kept history was
// not removed.
func TestExtensionUninstallWaitStalledRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, status, message string }{
		{"unchanged", "deployed", "operation 'helm-operation-abcde' ended (exit code 1); release 'epinio' is at status deployed; the release stays installed"},
		{"uninstalled before and still", "uninstalled", "operation 'helm-operation-abcde' ended (exit code 1); release 'epinio' is at status uninstalled; its kept history was not removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := newUpgradeFake(t)
				putExtApp(f, tc.status, extInstalledVersion, extRepo)
				in := extInput(extChart)
				in.b[flagWait] = true
				_, exec := plan(t, newTestExtensionUninstall(f), in)
				f.script("get:operations/"+extUIPlugin+"/helm-operation-abcde", opStalled(" exit code: 1"))
				f.script("get:apps/"+extUIPlugin+"/"+extChart, found(map[string]any{"spec": map[string]any{
					"version": int64(1), "info": map[string]any{"status": tc.status},
				}}))
				_, err := exec(t.Context())
				require.Error(t, err)
				assert.Equal(t, tc.message, clierr.From(err).Message)
			})
		})
	}
}

// TestExtensionUninstallWaitWithoutOperationRead asserts that --wait for a
// caller who may not get the Operation, when the ClusterRepo of the App has a
// service account, warns in the preview and goes by the App alone: it
// finishes with the App gone and reads no Operation.
func TestExtensionUninstallWaitWithoutOperationRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newUpgradeFake(t)
		f.allowed = false
		f.put("clusterrepos", "", extRepo, map[string]any{"spec": map[string]any{"serviceAccount": "helm", "serviceAccountNamespace": extUIPlugin}})
		in := extInput(extChart)
		in.b[flagWait] = true
		eff, exec := plan(t, newTestExtensionUninstall(f), in)
		assert.True(t, slices.ContainsFunc(eff.Warnings, func(w string) bool {
			return strings.HasPrefix(w, "you cannot get operations in namespace '"+extUIPlugin+"'; the wait reads the app only")
		}), "%v", eff.Warnings)
		f.script("get:apps/"+extUIPlugin+"/"+extChart, absent)
		res, err := exec(t.Context())
		require.NoError(t, err)
		assert.Contains(t, res.Text, "app epinio: uninstalled")
		assert.NotContains(t, f.reads, "get:operations/"+extUIPlugin+"/helm-operation-abcde")
	})
}

// TestExtensionUninstallPlanDisplayIdentity asserts the Effect names the app
// and the chart's name, with no "*" marker in the identity that is committed.
func TestExtensionUninstallPlanDisplayIdentity(t *testing.T) {
	t.Parallel()
	f := newUpgradeFake(t)
	eff, _ := plan(t, newTestExtensionUninstall(f), extInput(extChart))
	assert.Equal(t, "app", eff.DisplayKind)
	assert.Equal(t, extChart, eff.DisplayName)
	assert.NotContains(t, eff.DisplayName, "*")
	assert.Equal(t, "apps/"+extChart, eff.Target)
}

// TestExtensionUninstallRBAC asserts a refused read of the App or a refused
// action is the server's RBAC refusal.
func TestExtensionUninstallRBAC(t *testing.T) {
	t.Parallel()
	t.Run("the App", func(t *testing.T) {
		t.Parallel()
		f := newUpgradeFake(t)
		f.fail("get:apps/"+extUIPlugin+"/"+extChart, clierr.FromResponse("GET", "GET", 403, []byte(`{}`), nil))
		_, _, err := newTestExtensionUninstall(f).Plan(t.Context(), extInput(extChart))
		assert.Equal(t, clierr.CodeRBAC, errCode(err))
	})
	f := newUpgradeFake(t)
	f.postAnswer = func(string) ([]byte, error) {
		return nil, &engine.ActionRefused{Err: clierr.FromResponse("POST", "POST", 403, []byte(`{}`), nil), Body: []byte(`{}`)}
	}
	_, exec := plan(t, newTestExtensionUninstall(f), extInput(extChart))
	_, err := exec(t.Context())
	assert.Equal(t, clierr.CodeRBAC, errCode(err))
}

var _ runtime.Command = (*extensionUninstallCmd)(nil)
