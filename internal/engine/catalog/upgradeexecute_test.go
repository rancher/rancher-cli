package catalog

import (
	"net/http"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgradeExecuteRereads asserts Execute reads the App, the operations and
// the index entry again and sends nothing when one changed: each change is a
// retryable CONFLICT; a digest that is empty on either side is sent with a
// warning.
func TestUpgradeExecuteRereads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		change  func(*appFake)
		err     string
		warning string
	}{
		{name: "app changed", change: func(f *appFake) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.objs[objKey("apps", testNS, testRelease)].SetResourceVersion("2")
		}, err: "app 'my-nginx' changed since the preview; run the command again to see a new preview"},
		{name: "app removed", change: func(f *appFake) { f.remove("apps", testNS, testRelease) },
			err: "app 'my-nginx' was removed since the preview; run the command again to see a new preview"},
		{name: "operation started", change: func(f *appFake) {
			f.put("operations", testNS, "helm-operation-z", operation(testRelease, testNow, "", 1, 0))
		}, err: "operation 'helm-operation-z' for release 'my-nginx' started since the preview"},
		{name: "digest changed", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0", Digest: "sha256:new"}}})}
		}, err: "the content of chart 'nginx' '1.0.0' changed in the index since the preview"},
		{name: "version left the index", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "2.0.0"}}})}
		}, err: "version '1.0.0' of chart 'nginx' left the index since the preview"},
		{name: "index not readable", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: http.StatusInternalServerError, body: apiserverError("index is being rebuilt")}
		}, err: "the index of ClusterRepo 'repo1' could not be read again"},
		{name: "digest empty now", change: func(f *appFake) {
			f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0"}}})}
		}, warning: "the index gives no digest for chart nginx 1.0.0; its content was not compared with the preview's"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, nil)
			_, exec, err := planUpgrade(t, f, nil, upgradeInput())
			require.NoError(t, err)
			tc.change(f)
			res, err := exec(t.Context())
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				ce := clierr.From(err)
				assert.Equal(t, clierr.CodeConflict, ce.Code)
				assert.True(t, ce.Retryable)
				assert.Zero(t, f.postCount())
				return
			}
			require.NoError(t, err)
			assert.Contains(t, res.Warnings, tc.warning)
			assert.Equal(t, 1, f.postCount())
		})
	}
}

// TestUpgradeResult asserts the result of an accepted request with no wait:
// the operation, the request id, the follow-up commands, the warning that
// nothing checks for a change after the request and the note on what the
// follow-up shows when Helm refuses the upgrade before it stores a revision.
func TestUpgradeResult(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	_, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, result{
		Release: testRelease, Namespace: testNS,
		OperationName: "helm-operation-abcde", OperationNamespace: "web", RequestID: testRequestID,
		FollowUp: []string{
			clierr.Rancher + " get operation 'helm-operation-abcde' --cluster 'prod' --namespace 'web' --wait",
			clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'",
		},
	}, res.Data)
	assert.Contains(t, res.Warnings, warnNoWatch)
	assert.Contains(t, res.Warnings, "an upgrade Helm refuses before it stores a revision leaves the app unchanged; the operation then shows the failure")
	assert.NotContains(t, res.Warnings, noAppNote)
	assert.Contains(t, res.Text, "Rancher accepted the upgrade request; operation helm-operation-abcde in namespace web runs Helm")
}

// TestUpgradeSendsNothingBeforeTheGates asserts a Plan sends no request and
// writes nothing: the preview of an upgrade reads only.
func TestUpgradeSendsNothingBeforeTheGates(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	_, _, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagVersion, "2.0.0")))
	require.NoError(t, err)
	assert.Zero(t, f.postCount())
}
