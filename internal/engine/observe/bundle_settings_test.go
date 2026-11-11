package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const settingsItemName = "rancher/settings.json"

// settingsNotRead is the text the settings item gives for the settings named.
func settingsNotRead(names []string) string {
	return fmt.Sprintf("%d of %d settings not read (%s)", len(names), len(settingsAllowlist), strings.Join(names, ", "))
}

// settingObject is a Setting with the value given, or with neither value nor
// default when value is empty.
func settingObject(name, value string) *unstructured.Unstructured {
	obj := map[string]any{"metadata": map[string]any{"name": name}}
	if value != "" {
		obj["value"] = value
	}
	return &unstructured.Unstructured{Object: obj}
}

// decodeSettingsFile reads the settings file of an archive and reports
// whether it holds one.
func decodeSettingsFile(t *testing.T, archive map[string]string) (map[string]string, bool) {
	t.Helper()
	raw, ok := archive[settingsItemName]
	if !ok {
		return nil, false
	}
	var values map[string]string
	require.NoError(t, json.Unmarshal([]byte(raw), &values))
	return values, true
}

// TestSupportBundleSettingsReportsEveryFailureButNotFound asserts a failed
// settings read, whatever its error, makes the settings item an error status
// that names the settings not read and ends with the first failure, counts in
// errors, leaves the settings file out when every read failed and does not
// change the exit code; a read answered not found stays silent. The error
// classes include ones the code never names, and one row for every code of
// clierr.AllCodes() but not found, so a code added later is exercised.
// Mutations: skip the failure again; report not found too; report only the
// errors of some codes; write the file when every read failed.
func TestSupportBundleSettingsReportsEveryFailureButNotFound(t *testing.T) {
	t.Parallel()
	type settingsRow struct {
		name     string
		err      error
		reported bool
	}
	rows := []settingsRow{
		{"not found", statusFailure(404), false},
		{"wrapped not found", fmt.Errorf("get the setting: %w", statusFailure(404)), false},
		{"401", statusFailure(401), true},
		{"403", statusFailure(403), true},
		{"500", statusFailure(500), true},
		{"530", statusFailure(530), true},
		{"certificate", certificateFailure(), true},
		{"transport", errors.New("dial tcp 192.0.2.1:443: connect: connection refused"), true},
		{"validation", clierr.New(clierr.CodeValidation, "x"), true},
		{"unclassified", errors.New("something nobody named"), true},
	}
	require.Contains(t, clierr.AllCodes(), clierr.CodeNotFound)
	for _, code := range clierr.AllCodes() {
		if code != clierr.CodeNotFound {
			rows = append(rows, settingsRow{"code " + code, clierr.New(code, "x"), true})
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, path, err := runExit(t, t.Context(), exitFactory(failOnly(tc.err, readSettings)), "local", "webhook")
			require.NoError(t, err, "a failed settings read does not change the exit code")
			data, ok := res.Data.(bundleResult)
			require.True(t, ok)
			item := findItem(t, manifestFrom(t, path), settingsItemName, "")
			values, hasFile := decodeSettingsFile(t, readArchive(t, path))

			if !tc.reported {
				assert.Equal(t, statusCollected, item.Status)
				assert.Zero(t, data.Errors)
				require.True(t, hasFile)
				assert.Empty(t, values)
				return
			}
			assert.True(t, strings.HasPrefix(item.Status, "error: "+settingsNotRead(settingsAllowlist)+"; first failure: "), item.Status)
			assert.True(t, strings.HasSuffix(item.Status, clierr.ExpandRemedies(tc.err.Error(), "")), item.Status)
			assert.NotContains(t, item.Status, clierr.RemedyMarker)
			assert.False(t, holdsMarkerRune(item.Status), "the status holds a character of the remedy marker")
			assert.Equal(t, 1, data.Errors)
			assert.False(t, hasFile, "no setting was answered: no settings file")
		})
	}
}

// TestSupportBundleSettingsPartialReadKeepsTheSettingsThatWereRead asserts a
// run in which some settings fail names only the failed ones, in allowlist
// order, and keeps the others in the settings file, the version file and the
// manifest.
func TestSupportBundleSettingsPartialReadKeepsTheSettingsThatWereRead(t *testing.T) {
	t.Parallel()
	denied := statusFailure(403)
	notFound := statusFailure(404)
	answers := map[string]error{
		"server-url":                   denied,
		"install-uuid":                 denied,
		"agent-image":                  denied,
		"agent-tls-mode":               notFound,
		"fleet-default-workspace-name": notFound,
	}
	values := map[string]string{"server-version": "v2.9.0", "system-agent-version": "v0.3.1"}

	factory := exitFactory(nil)
	local := factory.local.(*fakeClient)
	var failed []string
	for _, name := range settingsAllowlist {
		if err, ok := answers[name]; ok {
			local.withGetErr(settingKind.GVR, "", name, err)
			if err == denied {
				failed = append(failed, name)
			}
			continue
		}
		local.withObject(settingKind.GVR, "", name, settingObject(name, values[name]))
	}
	require.Len(t, failed, 3)

	res, path, err := runExit(t, t.Context(), factory, "local", "webhook")
	require.NoError(t, err)
	data, ok := res.Data.(bundleResult)
	require.True(t, ok)
	m := manifestFrom(t, path)
	item := findItem(t, m, settingsItemName, "")
	assert.True(t, strings.HasPrefix(item.Status, "error: "+settingsNotRead(failed)+"; first failure: "), item.Status)
	assert.Equal(t, 1, data.Errors)

	archive := readArchive(t, path)
	file, hasFile := decodeSettingsFile(t, archive)
	require.True(t, hasFile)
	assert.Equal(t, values, file, "k8s-version has neither value nor default: no key")
	var version map[string]string
	require.NoError(t, json.Unmarshal([]byte(archive["rancher/version.json"]), &version))
	assert.Equal(t, "v2.9.0", version["serverVersion"])
	assert.Equal(t, "v2.9.0", m.ServerVersion)
}

// TestSupportBundleSettingsFailedServerVersionLeavesTheVersionEmpty asserts a
// failed read of server-version, with the other settings answered, leaves the
// version empty and keeps the others in the settings file.
func TestSupportBundleSettingsFailedServerVersionLeavesTheVersionEmpty(t *testing.T) {
	t.Parallel()
	factory := exitFactory(nil)
	local := factory.local.(*fakeClient)
	want := map[string]string{}
	for _, name := range settingsAllowlist {
		if name == "server-version" {
			local.withGetErr(settingKind.GVR, "", name, statusFailure(530))
			continue
		}
		want[name] = "value of " + name
		local.withObject(settingKind.GVR, "", name, settingObject(name, want[name]))
	}

	_, path, err := runExit(t, t.Context(), factory, "local", "webhook")
	require.NoError(t, err)
	m := manifestFrom(t, path)
	assert.Empty(t, m.ServerVersion)
	assert.True(t, strings.HasPrefix(findItem(t, m, settingsItemName, "").Status, "error: "+settingsNotRead([]string{"server-version"})+"; first failure: "))
	file, hasFile := decodeSettingsFile(t, readArchive(t, path))
	require.True(t, hasFile)
	assert.Equal(t, want, file)
}

// TestSupportBundleSettingsNamesTheFirstFailureInAllowlistOrder asserts that
// when every read fails with one of two errors, the status ends with the error
// of the first setting of the allowlist.
func TestSupportBundleSettingsNamesTheFirstFailureInAllowlistOrder(t *testing.T) {
	t.Parallel()
	first, later := statusFailure(401), statusFailure(403)
	require.NotEqual(t, first.Error(), later.Error())
	factory := exitFactory(nil)
	local := factory.local.(*fakeClient)
	for i, name := range settingsAllowlist {
		err := later
		if i == 0 {
			err = first
		}
		local.withGetErr(settingKind.GVR, "", name, err)
	}

	_, path, err := runExit(t, t.Context(), factory, "local", "webhook")
	require.NoError(t, err)
	status := findItem(t, manifestFrom(t, path), settingsItemName, "").Status
	assert.True(t, strings.HasPrefix(status, "error: "+settingsNotRead(settingsAllowlist)+"; first failure: "), status)
	assert.True(t, strings.HasSuffix(status, clierr.ExpandRemedies(first.Error(), "")), status)
}

// TestCollectSettingsReturnsTheReadsTheFileAndTheError asserts the three
// results of collectSettings: the settings that were read, the file (nil when
// every read failed) and an error that names the settings not read and wraps
// the first failure.
func TestCollectSettingsReturnsTheReadsTheFileAndTheError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	other := errors.New("other")

	t.Run("every setting read", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient()
		for _, name := range settingsAllowlist {
			local.withObject(settingKind.GVR, "", name, settingObject(name, "v"))
		}
		values, data, err := collectSettings(t.Context(), local)
		require.NoError(t, err)
		assert.Len(t, values, len(settingsAllowlist))
		assert.NotNil(t, data)
	})

	t.Run("every setting not found", func(t *testing.T) {
		t.Parallel()
		values, data, err := collectSettings(t.Context(), newFakeClient())
		require.NoError(t, err)
		assert.Empty(t, values)
		require.NotNil(t, data)
		var decoded map[string]string
		require.NoError(t, json.Unmarshal(data, &decoded))
		assert.Empty(t, decoded)
	})

	t.Run("some failed", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient().
			withObject(settingKind.GVR, "", settingsAllowlist[0], settingObject(settingsAllowlist[0], "v")).
			withGetErr(settingKind.GVR, "", settingsAllowlist[1], boom).
			withGetErr(settingKind.GVR, "", settingsAllowlist[2], statusFailure(404)).
			withGetErr(settingKind.GVR, "", settingsAllowlist[3], other)
		values, data, err := collectSettings(t.Context(), local)
		assert.Equal(t, map[string]string{settingsAllowlist[0]: "v"}, values)
		assert.NotNil(t, data)
		require.EqualError(t, err, settingsNotRead([]string{settingsAllowlist[1], settingsAllowlist[3]})+"; first failure: boom")
		require.ErrorIs(t, err, boom)
		require.NotErrorIs(t, err, other)
	})

	t.Run("every setting failed", func(t *testing.T) {
		t.Parallel()
		local := newFakeClient()
		for _, name := range settingsAllowlist {
			local.withGetErr(settingKind.GVR, "", name, boom)
		}
		values, data, err := collectSettings(t.Context(), local)
		assert.Empty(t, values)
		assert.Nil(t, data)
		require.EqualError(t, err, settingsNotRead(settingsAllowlist)+"; first failure: boom")
		require.ErrorIs(t, err, boom)
	})
}
