package catalog

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretBody is a release Secret as steve returns it, with the labels and the
// data.release (a JSON value) given.
func secretBody(owner, name, version, release string) string {
	return `{"metadata":{"labels":{"owner":"` + owner + `","name":"` + name + `","version":"` + version + `"}},"data":{"release":` + release + `}}`
}

// storedReadCases are the reads of the release Secret that give no stored
// values, each with the clause the warning of an upgrade with --reset-values
// gives, and the error one without it ends with.
func storedReadCases() []struct {
	name      string
	app       func(*upgradeApp)
	answer    *rootAnswer
	code      string
	exit      int
	retryable bool
	msg       string
	hint      string
	reason    string
} {
	secret := releaseSecretName(3)
	quoted := clierr.ShellQuote(secret)
	getApp := clierr.Rancher + " get app 'my-nginx' --cluster 'prod' --namespace 'web'"
	moved := "app 'my-nginx' changed while it was read; run the command again"
	notReturned := "Rancher did not return the values of release 'my-nginx'; add --reset-values and a --values file that holds every value the release needs"
	const notReturnedReason = "Rancher did not return the release's values"
	ok := func(body string) *rootAnswer { return &rootAnswer{status: 200, body: body} }
	return []struct {
		name      string
		app       func(*upgradeApp)
		answer    *rootAnswer
		code      string
		exit      int
		retryable bool
		msg       string
		hint      string
		reason    string
	}{
		{name: "forbidden", answer: &rootAnswer{status: http.StatusForbidden, body: `{"message":"secrets is forbidden"}`},
			code: clierr.CodeRBAC, exit: 6,
			msg:    "you cannot get Secret " + quoted + " in namespace 'web', where Helm keeps the values of release 'my-nginx'; the upgrade sends these values and has to read them first; ask for permission to get Secrets in the namespace, or add --reset-values and a --values file that holds every value the release needs",
			reason: "you cannot get Secret " + quoted + " in namespace 'web'"},
		{name: "not found", answer: &rootAnswer{status: http.StatusNotFound, body: `{"message":"not found"}`},
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " was not found"},
		{name: "another revision", answer: ok(secretBody("helm", "my-nginx", "2", `{"name":"my-nginx","namespace":"web","version":2,"info":{"status":"deployed"},"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " holds another release or revision"},
		{name: "another release name", answer: ok(secretBody("helm", "my-nginx", "3", `{"name":"other","namespace":"web","version":3,"info":{"status":"deployed"},"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " holds another release or revision"},
		{name: "another namespace", answer: ok(secretBody("helm", "my-nginx", "3", `{"name":"my-nginx","namespace":"other","version":3,"info":{"status":"deployed"},"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " holds another release or revision"},
		{name: "name label of another release", answer: ok(secretBody("helm", "other", "3", `{"name":"my-nginx","namespace":"web","version":3,"info":{"status":"deployed"},"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " holds another release or revision"},
		{name: "version label of another revision", answer: ok(secretBody("helm", "my-nginx", "2", `{"name":"my-nginx","namespace":"web","version":3,"info":{"status":"deployed"},"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeConflict, exit: 5, retryable: true, msg: moved, reason: "Secret " + quoted + " holds another release or revision"},
		{name: "release absent", answer: ok(`{"metadata":{"labels":{"owner":"helm","name":"my-nginx","version":"3"}},"data":{}}`),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "release still a string", answer: ok(secretBody("helm", "my-nginx", "3", `"H4sIAAAAAAAA/6pWSs7PTSzJTFWyUlJKzMksyUwtLs5MzyhRsrIAAQAA//8"`)),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "release null", answer: ok(secretBody("helm", "my-nginx", "3", `null`)),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "owner label not helm", answer: ok(secretBody("someone", "my-nginx", "3", `{"name":"my-nginx","namespace":"web","version":3,"config":{"stored":"`+canary+`"}}`)),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "body not JSON", answer: ok(`not json`),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "release with a name that is not text", answer: ok(secretBody("helm", "my-nginx", "3", `{"name":5}`)),
			code: clierr.CodeGeneric, exit: 1, msg: notReturned, hint: getApp, reason: notReturnedReason},
		{name: "no owner reference", app: func(a *upgradeApp) { a.noOwner = true },
			code: clierr.CodeGeneric, exit: 1, hint: getApp, reason: "the app has no owner reference to a release Secret",
			msg: "app 'my-nginx' has no owner reference to a release Secret and the release's values cannot be read; add --reset-values and a --values file that holds every value the release needs"},
	}
}

// TestUpgradeStoredValuesReadFailures asserts each failed read of the stored
// values ends the upgrade without --reset-values: RBAC for a denied read,
// a CONFLICT that is retryable for a Secret that is gone or holds another
// release or revision, GENERIC for a release steve did not decode or one
// Helm did not write, and GENERIC with the hint that reads the App for an App
// with no release Secret. No request is sent.
func TestUpgradeStoredValuesReadFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range storedReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*upgradeApp)
			if tc.app != nil {
				edits = append(edits, tc.app)
			}
			f := upgradeFake(t, nil, edits...)
			if tc.answer != nil {
				f.root.answers[releaseSecretPath(releaseSecretName(3))] = *tc.answer
			}
			ce := upgradeRefusal(t, f, nil, upgradeInput())
			assert.Equal(t, tc.code, ce.Code)
			assert.Equal(t, tc.exit, ce.ExitCode())
			assert.Equal(t, tc.retryable, ce.Retryable)
			assert.Equal(t, tc.msg, ce.Message)
			assert.Equal(t, tc.hint, ce.Hint)
			if tc.app != nil {
				assert.Empty(t, requestsOf(f, "/secrets"), "an App with no release Secret reads no Secret")
			}
		})
	}
}

// TestUpgradeStoredValuesFailuresWithResetValues asserts each failed read
// is a warning when --reset-values is typed: "cannot read the stored values"
// with the reason, the upgrade goes on and sends resetValues, and no message
// names --reset-values as a remedy, as the caller typed it.
func TestUpgradeStoredValuesFailuresWithResetValues(t *testing.T) {
	t.Parallel()
	for _, tc := range storedReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*upgradeApp)
			if tc.app != nil {
				edits = append(edits, tc.app)
			}
			f := upgradeFake(t, nil, edits...)
			if tc.answer != nil {
				f.root.answers[releaseSecretPath(releaseSecretName(3))] = *tc.answer
			}
			eff, exec, err := planUpgrade(t, f, nil, upgradeInput(withBool(flagResetValues, true)))
			require.NoError(t, err)
			warning := "cannot read the release's values (" + tc.reason + "); the preview cannot list the paths --reset-values removes"
			assert.Contains(t, eff.Warnings, warning)
			for _, w := range eff.Warnings {
				assert.NotContains(t, w, "add --reset-values")
				assert.NotContains(t, w, "or add")
				assert.NotContains(t, w, "the release's values come from revision")
			}
			res, err := exec(t.Context())
			require.NoError(t, err)
			require.Equal(t, 1, f.postCount())
			assert.Equal(t, true, upgradeChartOf(t, f.posts[0].body)["resetValues"])
			for _, w := range res.Warnings {
				assert.NotContains(t, w, "add --reset-values")
			}
		})
	}
}

// TestUpgradeStoredValuesOtherFailuresStayErrors asserts a failed read that
// is neither a denial nor a missing Secret stays the error, with or without
// --reset-values, and never names --reset-values: the request failed in a way
// that says nothing about the release.
func TestUpgradeStoredValuesOtherFailuresStayErrors(t *testing.T) {
	t.Parallel()
	for name, edits := range map[string][]func(*appInput){
		"without --reset-values": nil,
		"with --reset-values":    {withBool(flagResetValues, true)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, nil)
			f.root.answers[releaseSecretPath(releaseSecretName(3))] = rootAnswer{status: http.StatusInternalServerError, body: apiserverError("etcd is unavailable")}
			ce := upgradeRefusal(t, f, nil, upgradeInput(edits...))
			assert.Equal(t, clierr.CodeGeneric, ce.Code)
			assert.NotEqual(t, clierr.CodeConflict, ce.Code)
			assert.Contains(t, ce.Message, "etcd is unavailable")
			assert.NotContains(t, ce.Message+ce.Hint, "--reset-values")
		})
	}
}

// TestUpgradeStoredValuesNamedInThePreview asserts the preview names the
// revision and status of the stored values and the Secret, warns when that
// revision failed, and with --reset-values says the removed paths are listed
// from that revision instead.
func TestUpgradeStoredValuesNamedInThePreview(t *testing.T) {
	t.Parallel()
	secret := "'" + releaseSecretName(3) + "'"
	recordedText := fmt.Sprintf("revision %d", recordedRevision)
	cases := []struct {
		name   string
		status string
		edit   func(*appInput)
		want   []string
		never  []string
	}{
		{"deployed", "deployed", func(*appInput) {},
			[]string{"the release's values come from " + recordedText + " (status deployed), Secret " + secret}, []string{"failed; this upgrade sends"}},
		{"failed", "failed", func(*appInput) {},
			[]string{"the release's values come from " + recordedText + " (status failed), Secret " + secret, recordedText + " failed; this upgrade sends the values that failed, with your changes"}, nil},
		{"deployed with --reset-values", "deployed", withBool(flagResetValues, true),
			[]string{"the preview lists the paths of the values --reset-values removes, read from " + recordedText + " (status deployed), Secret " + secret}, []string{"the release's values come from", "failed; this upgrade sends"}},
		{"failed with --reset-values", "failed", withBool(flagResetValues, true),
			[]string{"the preview lists the paths of the values --reset-values removes, read from " + recordedText + " (status failed), Secret " + secret}, []string{"the release's values come from", "failed; this upgrade sends"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAppFake(t)
			a := f.putUpgradeApp()
			f.putRelease(a, releaseFixture{status: tc.status, config: map[string]any{"k": "v"}})
			eff, _, err := planUpgrade(t, f, nil, upgradeInput(tc.edit))
			require.NoError(t, err)
			for _, w := range tc.want {
				assert.Contains(t, eff.Warnings, w)
			}
			for _, w := range eff.Warnings {
				for _, never := range tc.never {
					assert.NotContains(t, w, never)
				}
			}
		})
	}
}

// TestUpgradeReadsTheReleaseSecretOnce asserts the stored values come from one
// request for the Secret the App's owner reference names, with the query that
// makes steve decode the release, and from no other Secret.
func TestUpgradeReadsTheReleaseSecretOnce(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, map[string]any{"k": "v"})
	_, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	_, err = exec(t.Context())
	require.NoError(t, err)
	got := requestsOf(f, "/secrets")
	require.Len(t, got, 1)
	assert.Equal(t, releaseSecretPath(releaseSecretName(3)), got[0])
	assert.True(t, strings.HasSuffix(got[0], "?includeHelmData=true"))
}
