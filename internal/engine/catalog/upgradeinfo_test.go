package catalog

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three texts of Helm's index lookup that Rancher's info link returns as a
// 500 when the repository no longer has a version, has no version of the chart,
// or has no such chart.
const (
	helmNoVersionText  = "no chart version found for nginx-9.9.9"
	helmNoVersionsText = "no chart version found"
	helmNoChartText    = "no chart name found"
)

// apiserverError is the body the apiserver library writes for an error that
// is not an API error: a 500 whose message is the error's text.
func apiserverError(message string) string {
	return `{"type":"error","status":500,"code":"ServerError","message":"` + message + `"}`
}

// listIndex makes the filtered index list exactly the versions of the chart.
func listIndex(f *appFake, versions ...string) {
	list := make([]ChartVersion, 0, len(versions))
	for _, v := range versions {
		list = append(list, ChartVersion{Version: v, Digest: "sha256:" + v})
	}
	f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: list})}
}

// TestFromResponseKeepsHelmLookupTexts asserts the message of the status
// table for an apiserver 500 keeps each text of Helm's index lookup whole:
// the upgrade tells a version, a chart and a repository that no longer have
// something apart by them.
func TestFromResponseKeepsHelmLookupTexts(t *testing.T) {
	t.Parallel()
	for _, text := range []string{helmNoVersionText, helmNoVersionsText, helmNoChartText} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			ce := clierr.FromResponse("GET /x on the Rancher server", http.MethodGet, http.StatusInternalServerError, []byte(apiserverError(text)), nil)
			assert.Contains(t, ce.Message, `("`+text+`")`)
			status, ok := clierr.HTTPStatus(ce)
			assert.True(t, ok)
			assert.Equal(t, http.StatusInternalServerError, status)
		})
	}
}

// TestUpgradeKeptVersionNotInTheIndex asserts an upgrade that keeps the
// version the App runs, which the filtered index no longer lists, omitted or
// typed: the info of that version is read and nothing else of the chart, the
// index is read once, no request carries skipFilter or k8sVersion, the
// warning is in the preview and the result, and Execute compares no digest.
func TestUpgradeKeptVersionNotInTheIndex(t *testing.T) {
	t.Parallel()
	warning := "version 1.0.0 is no longer listed in the index for this cluster; the upgrade stays on it, and its chart content is not compared between the preview and the request"
	for name, edit := range map[string]func(*appInput){"omitted": func(*appInput) {}, "typed": withFlag(flagVersion, "1.0.0")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, nil)
			listIndex(f, "2.0.0")
			eff, exec, err := planUpgrade(t, f, nil, upgradeInput(edit))
			require.NoError(t, err)
			assert.Contains(t, eff.Warnings, warning)
			assert.Equal(t, "1.0.0", diffPaths(eff)["version"])
			assert.Equal(t, []string{infoPath(testChart, "1.0.0")}, requestsOf(f, "link=info"))
			assert.Equal(t, []string{indexPath()}, requestsOf(f, "link=index"))

			// The index lists the version again with another digest: nothing
			// compares it.
			listIndex(f, "2.0.0", "1.0.0")
			res, err := exec(t.Context())
			require.NoError(t, err)
			assert.Contains(t, res.Warnings, warning)
			assert.Equal(t, []string{indexPath()}, requestsOf(f, "link=index"), "Execute reads no index for a version the index does not list")
			assert.Equal(t, []string{infoPath(testChart, "1.0.0")}, requestsOf(f, "link=info"))
			for _, p := range f.root.requests() {
				assert.NotContains(t, p, "skipFilter")
				assert.NotContains(t, p, "k8sVersion")
			}
			assert.Equal(t, "1.0.0", upgradeChartOf(t, f.posts[0].body)["version"])
		})
	}
}

// TestUpgradeKeptVersionInTheIndexIsCompared asserts a kept version the index
// lists is handled as any other: no warning in the preview, and Execute
// compares its digest.
func TestUpgradeKeptVersionInTheIndexIsCompared(t *testing.T) {
	t.Parallel()
	f := upgradeFake(t, nil)
	eff, exec, err := planUpgrade(t, f, nil, upgradeInput())
	require.NoError(t, err)
	for _, w := range eff.Warnings {
		assert.NotContains(t, w, "is no longer listed in the index for this cluster")
	}
	f.root.answers[indexPath()] = rootAnswer{status: 200, body: indexJSON(map[string][]ChartVersion{testChart: {{Version: "1.0.0", Digest: "sha256:new"}}})}
	_, err = exec(t.Context())
	require.ErrorContains(t, err, "the content of chart 'nginx' '1.0.0' changed in the index since the preview")
	assert.Zero(t, f.postCount())
}

// TestUpgradeInfoAnswers asserts what the Plan does with the answers of the
// info link for the version it sends: Rancher's 500 for a version, a chart or
// a list of versions the repository no longer has is VALIDATION naming what is
// missing, with the remedy that fits; a 500 with another text keeps the
// status table's code; an answer for another build of the same base version
// is VALIDATION.
func TestUpgradeInfoAnswers(t *testing.T) {
	t.Parallel()
	chartsHint := clierr.Rancher + " charts --cluster 'prod' --repo 'repo1'"
	scenarios := []struct {
		name    string
		version string
		setup   func(*appFake)
		edit    func(*appInput)
	}{
		{"kept version the index lists", "1.0.0", func(*appFake) {}, func(*appInput) {}},
		{"kept version the index does not list", "1.0.0", func(f *appFake) { listIndex(f, "2.0.0") }, func(*appInput) {}},
		{"kept version typed", "1.0.0", func(*appFake) {}, withFlag(flagVersion, "1.0.0")},
		{"typed version the index lists", "2.0.0", func(*appFake) {}, withFlag(flagVersion, "2.0.0")},
	}
	rows := []struct {
		name   string
		answer func(version string) rootAnswer
		code   string
		exit   int
		msg    string
		hint   string
		// noRemedy is true when the message and hint name no --version.
		noRemedy bool
	}{
		{name: "no version found for the version", answer: func(string) rootAnswer { return rootAnswer{500, apiserverError(helmNoVersionText)} },
			code: clierr.CodeValidation, exit: 2, hint: chartsHint + " --all-versions",
			msg: "version '%s' of chart 'nginx' is no longer in ClusterRepo 'repo1'; name one with --version"},
		{name: "no versions", answer: func(string) rootAnswer { return rootAnswer{500, apiserverError(helmNoVersionsText)} },
			code: clierr.CodeValidation, exit: 2, hint: chartsHint, noRemedy: true,
			msg: "chart 'nginx' has no versions in ClusterRepo 'repo1'"},
		{name: "no chart", answer: func(string) rootAnswer { return rootAnswer{500, apiserverError(helmNoChartText)} },
			code: clierr.CodeValidation, exit: 2, hint: chartsHint, noRemedy: true,
			msg: "chart 'nginx' is no longer in ClusterRepo 'repo1'"},
		{name: "another 500", answer: func(string) rootAnswer { return rootAnswer{500, apiserverError("the index is being rebuilt")} },
			code: clierr.CodeGeneric, exit: 1, msg: "the index is being rebuilt"},
		{name: "another build of the same base version", code: clierr.CodeValidation, exit: 2, hint: chartsHint + " --all-versions",
			answer: func(version string) rootAnswer {
				return rootAnswer{200, upgradeInfoOf(version+"+up0.1.1", nil)}
			},
			msg: "version '%s' of chart 'nginx' is no longer in ClusterRepo 'repo1' (Rancher returned version '%s+up0.1.1' instead); name one with --version"},
		{name: "no chart version in the answer", code: clierr.CodeValidation, exit: 2, hint: chartsHint + " --all-versions",
			answer: func(string) rootAnswer { return rootAnswer{200, `{"values":{},"chart":{}}`} },
			msg:    "(Rancher returned version '' instead)"},
		{name: "not found", answer: func(string) rootAnswer { return rootAnswer{404, `{"message":"no such repository"}`} },
			code: clierr.CodeNotFound, exit: 4, hint: clierr.Rancher + " get clusterrepos --cluster 'prod'", msg: "ClusterRepo 'repo1'"},
	}
	for _, sc := range scenarios {
		for _, row := range rows {
			t.Run(sc.name+", "+row.name, func(t *testing.T) {
				t.Parallel()
				f := upgradeFake(t, nil)
				sc.setup(f)
				f.root.answers[infoPath(testChart, sc.version)] = row.answer(sc.version)
				ce := upgradeRefusal(t, f, nil, upgradeInput(sc.edit))
				assert.Equal(t, row.code, ce.Code)
				assert.Equal(t, row.exit, ce.ExitCode())
				assert.Equal(t, row.hint, ce.Hint)
				assert.Contains(t, ce.Message, strings.ReplaceAll(row.msg, "%s", sc.version))
				if row.noRemedy {
					assert.NotContains(t, ce.Message+ce.Hint, "--version")
				}
				if row.code != clierr.CodeValidation && row.code != clierr.CodeNotFound {
					assert.NotContains(t, ce.Message, "no longer in ClusterRepo")
				}
			})
		}
	}
}

// TestUpgradeDowngradeWarnings asserts the warning for a version after the
// current one in the filtered index order, none for a newer version, and the
// warning that the order could not be checked when the index does not list the
// current version.
func TestUpgradeDowngradeWarnings(t *testing.T) {
	t.Parallel()
	const suffix = "Helm runs it as any upgrade, and the chart may not support downgrades"
	t.Run("older", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil, func(a *upgradeApp) { a.version = "2.0.0" })
		eff, _, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagVersion, "1.0.0")))
		require.NoError(t, err)
		assert.Contains(t, eff.Warnings, "1.0.0 is older than the installed 2.0.0: a downgrade; "+suffix)
		var version []any
		for _, d := range eff.Diff {
			if d.Path == "version" {
				version = []any{d.From, d.To}
			}
		}
		assert.Equal(t, []any{"2.0.0", "1.0.0"}, version)
	})
	t.Run("newer", func(t *testing.T) {
		t.Parallel()
		eff, _, err := planUpgrade(t, upgradeFake(t, nil), nil, upgradeInput(withFlag(flagVersion, "2.0.0")))
		require.NoError(t, err)
		for _, w := range eff.Warnings {
			assert.NotContains(t, w, "a downgrade")
			assert.NotContains(t, w, "was not checked")
		}
	})
	t.Run("the index does not list the installed version", func(t *testing.T) {
		t.Parallel()
		f := upgradeFake(t, nil, func(a *upgradeApp) { a.version = "0.5.0" })
		eff, _, err := planUpgrade(t, f, nil, upgradeInput(withFlag(flagVersion, "1.0.0")))
		require.NoError(t, err)
		assert.Contains(t, eff.Warnings, "the index does not list the installed version 0.5.0; whether 1.0.0 is older was not checked; Helm runs any upgrade, and the chart may not support downgrades")
		for _, w := range eff.Warnings {
			assert.NotContains(t, w, "a downgrade")
		}
	})
}

// TestUpgradeHistoryWarnings asserts the warning that Helm deletes the oldest
// revision appears only when a deletion is possible: without --history-max
// from the fifth revision (Rancher sends 5), with --history-max n from the
// nth.
func TestUpgradeHistoryWarnings(t *testing.T) {
	t.Parallel()
	const kept = "Helm deletes the oldest before it runs, never the last deployed one"
	cases := []struct {
		name     string
		revision int64
		edit     func(*appInput)
		want     string
	}{
		{"default at 4", 4, func(*appInput) {}, ""},
		{"default at 5", 5, func(*appInput) {}, "--history-max is not given and Rancher uses 5: when the new revision would take the release over 5 revisions, " + kept + "; give --history-max to keep more"},
		{"default at 6", 6, func(*appInput) {}, "--history-max is not given and Rancher uses 5: when the new revision would take the release over 5 revisions, " + kept + "; give --history-max to keep more"},
		{"10 at 9", 9, withFlag(flagHistoryMax, "10"), ""},
		{"10 at 10", 10, withFlag(flagHistoryMax, "10"), "--history-max 10: when the new revision would take the release over 10 revisions, " + kept},
		{"1 at 1", 1, withFlag(flagHistoryMax, "1"), "--history-max 1: when the new revision would take the release over 1 revisions, " + kept},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := upgradeFake(t, nil, func(a *upgradeApp) { a.revision = tc.revision })
			eff, _, err := planUpgrade(t, f, nil, upgradeInput(tc.edit))
			require.NoError(t, err)
			var got []string
			for _, w := range eff.Warnings {
				if strings.Contains(w, "would take the release over") {
					got = append(got, w)
				}
			}
			if tc.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, []string{tc.want}, got)
		})
	}
}
