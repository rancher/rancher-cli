package catalog

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storedCanary is in the stored values of the release the tests below read,
// beside canary, which is in the values file and in every other part of the
// release Secret. No command prints either.
const storedCanary = "canary-stored-0ther-s3cr3t"

// canaryRelease is the stored values of a release that hold the canary at a
// path no command shows.
func canaryRelease() map[string]any {
	return storedValues(map[string]any{"stored": storedCanary, "nested": map[string]any{"deep": storedCanary}})
}

// canarySource is the values file the tests below pass.
func canarySource() files {
	return files{"v.yaml": "fromFile: " + canary + "\nnested:\n  other: " + canary + "\n"}
}

func withCanaryFile(in *appInput) { in.strs[flagValues] = []string{"v.yaml"} }

// requireNoCanary asserts no canary is in the text of any part.
func requireNoCanary(t *testing.T, what string, parts ...any) {
	t.Helper()
	for _, part := range parts {
		text := fmt.Sprintf("%+v", part)
		assert.NotContains(t, text, canary, what)
		assert.NotContains(t, text, storedCanary, what)
	}
}

// requireNoCanaryInError asserts no canary is in an error's message, hint,
// details or warnings.
func requireNoCanaryInError(t *testing.T, what string, ce *clierr.Error) {
	t.Helper()
	requireNoCanary(t, what, ce.Message, ce.Hint, ce.Details, ce.Warnings(), ce.Error())
}

// requireNoCanaryInEffect asserts no canary is in any part of a preview.
func requireNoCanaryInEffect(t *testing.T, what string, eff safety.Effect) {
	t.Helper()
	requireNoCanary(t, what, eff)
}

// TestUpgradeNeverPrintsValues asserts the preview, the result of a request
// with no wait and the result of a wait that succeeded hold no value of the
// stored values, of the values file, or of the chart, the manifest and the
// hooks of the release Secret.
func TestUpgradeNeverPrintsValues(t *testing.T) {
	t.Parallel()
	t.Run("dry-run and apply", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, canaryRelease())
		eff, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile, withFlag(flagVersion, "2.0.0")))
		require.NoError(t, err)
		requireNoCanaryInEffect(t, "the preview", eff)
		assert.Equal(t, hiddenText, diffPaths(eff)["values.fromFile"])
		assert.NotContains(t, diffPaths(eff), "values.stored", "a stored value that is resent unchanged is counted, not listed")
		res, err := exec(t.Context())
		require.NoError(t, err)
		requireNoCanary(t, "the result", res.Text, res.Warnings, res.Data)
		assert.Contains(t, upgradeValuesOf(t, f), "fromFile", "the request itself carries the values")
	})
	t.Run("wait success", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := upgradeFake(t, canaryRelease())
			f.script(opKey, opDone)
			f.script(appKey, appRev("deployed", testRequestID, upgradedRevision, "Upgrade complete "+canary))
			_, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile, waitFor("")))
			require.NoError(t, err)
			r := execute(t, exec)
			require.NoError(t, r.err)
			requireNoCanary(t, "the result", r.res.Text, r.res.Warnings, r.res.Data)
		})
	})
	t.Run("--reset-values", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, canaryRelease())
		eff, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile, withBool(flagResetValues, true)))
		require.NoError(t, err)
		requireNoCanaryInEffect(t, "the preview", eff)
		res, err := exec(t.Context())
		require.NoError(t, err)
		requireNoCanary(t, "the result", res.Text, res.Warnings, res.Data)
	})
}

// TestUpgradeFailureNeverPrintsValues asserts the errors of the wait hold no
// value when Helm's text, the App's description or the Stalled message echoes
// the stored values or the values file.
func TestUpgradeFailureNeverPrintsValues(t *testing.T) {
	t.Parallel()
	echo := "failed with " + canary + " and " + storedCanary
	cases := map[string]struct {
		op   scripted
		apps []scripted
		edit func(*appInput)
	}{
		"the failure of the revision":     {opDone, []scripted{appRev("failed", testRequestID, upgradedRevision, echo)}, nil},
		"the failure after a stall":       {opStalled("exit code: 1"), []scripted{appRev("failed", testRequestID, upgradedRevision, echo)}, nil},
		"a stall message":                 {opStalled(echo), []scripted{appRev("failed", testRequestID, upgradedRevision, "x")}, nil},
		"a stall message and no revision": {opStalled(echo), []scripted{atRecorded()}, nil},
		"a stall message, deployed":       {opStalled(echo), []scripted{mineAt("deployed", upgradedRevision)}, nil},
		"a stall message, pending":        {opStalled(echo), []scripted{mineAt("pending-upgrade", upgradedRevision)}, nil},
		"the failure with cleanup":        {opDone, []scripted{appRev("failed", testRequestID, upgradedRevision, echo)}, withBool(flagCleanupOnFail, true)},
		"the timeout":                     {opDone, []scripted{appRev("deployed", otherID, recordedRevision, echo)}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				f := upgradeFake(t, canaryRelease())
				f.script(opKey, tc.op)
				f.script(appKey, tc.apps...)
				edits := []func(*appInput){withCanaryFile, waitFor("2m")}
				if tc.edit != nil {
					edits = append(edits, tc.edit)
				}
				_, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(edits...))
				require.NoError(t, err)
				r := execute(t, exec)
				requireNoCanaryInError(t, name, r.clierr(t))
			})
		})
	}
}

// TestUpgradeRefusalsNeverPrintValues asserts no refusal of the Plan prints a
// value, with a release Secret that holds the canary in its stored values, chart,
// manifest and hooks and a values file that holds it too.
func TestUpgradeRefusalsNeverPrintValues(t *testing.T) {
	t.Parallel()
	for _, tc := range upgradeRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*upgradeApp)
			if tc.app != nil {
				edits = append(edits, tc.app)
			}
			f := upgradeFake(t, canaryRelease(), edits...)
			if tc.setup != nil {
				tc.setup(f)
			}
			in := upgradeInput(withCanaryFile)
			if tc.edit != nil {
				tc.edit(&in)
			}
			_, _, err := planUpgrade(t, f, canarySource(), in)
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			requireNoCanaryInError(t, tc.name, ce)
		})
	}
}

// TestUpgradeStoredReadErrorsNeverPrintValues asserts no failed read of the
// stored values prints a value, as an error or as the warning of an upgrade
// with --reset-values, though the Secret that was read holds the canary.
func TestUpgradeStoredReadErrorsNeverPrintValues(t *testing.T) {
	t.Parallel()
	for _, tc := range storedReadCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edits []func(*upgradeApp)
			if tc.app != nil {
				edits = append(edits, tc.app)
			}
			f := upgradeFake(t, canaryRelease(), edits...)
			if tc.answer != nil {
				f.root.answers[releaseSecretPath(releaseSecretName(3))] = *tc.answer
			}
			_, _, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile))
			require.Error(t, err)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			requireNoCanaryInError(t, tc.name, ce)

			eff, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile, withBool(flagResetValues, true)))
			require.NoError(t, err)
			requireNoCanaryInEffect(t, tc.name+" with --reset-values", eff)
			res, err := exec(t.Context())
			require.NoError(t, err)
			requireNoCanary(t, tc.name+" with --reset-values", res.Text, res.Warnings, res.Data)
		})
	}
}

// TestUpgradeRefusedActionHidesStoredValues asserts the server's text of a
// refused action reaches the error with the stored values and the values of
// the files hidden, in each status.
func TestUpgradeRefusedActionHidesStoredValues(t *testing.T) {
	t.Parallel()
	bodies := []string{
		`{"message":"chart refused value ` + canary + ` and ` + storedCanary + `"}`,
		`{"message":"nested value ` + storedCanary + ` refused"}`,
		"plain text " + canary + " " + storedCanary,
	}
	for _, status := range []int{400, 403, 404, 409, 500, 503} {
		for i, body := range bodies {
			t.Run(fmt.Sprintf("status %d body %d", status, i), func(t *testing.T) {
				t.Parallel()
				f := upgradeFake(t, canaryRelease())
				f.postAnswer = func(string) ([]byte, error) {
					return nil, clierr.FromResponse("POST /x on the Rancher server", http.MethodPost, status, []byte(body), nil)
				}
				_, exec, err := planUpgrade(t, f, canarySource(), upgradeInput(withCanaryFile))
				require.NoError(t, err)
				_, err = exec(t.Context())
				require.Error(t, err)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok)
				requireNoCanaryInError(t, "the refused action", ce)
				assert.Equal(t, 1, f.postCount())
			})
		}
	}
}
