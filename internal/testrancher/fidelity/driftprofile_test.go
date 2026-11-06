package fidelity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompareSetsTheCauseByProfile asserts the cause of a new difference
// by profile: in a helm-check run a downstream probe on another k3s than the
// goldens' is kubernetes and every other new difference is install; in an
// upcoming run, and in a run with no profile, the causes are rancher and, for
// a downstream probe on another k3s, undetermined.
func TestCompareSetsTheCauseByProfile(t *testing.T) {
	cases := []struct {
		name       string
		profile    string
		probe      string
		downstream string
		want       string
	}{
		{"helm-check, not a downstream probe", ProfileHelmCheck, "fam/a", "k3s:golden", CauseInstall},
		{"helm-check, not a downstream probe on another k3s", ProfileHelmCheck, "fam/a", "k3s:other", CauseInstall},
		{"helm-check, downstream probe on the goldens' k3s", ProfileHelmCheck, "fam/down", "k3s:golden", CauseInstall},
		{"helm-check, downstream probe on another k3s", ProfileHelmCheck, "fam/down", "k3s:other", CauseKubernetes},
		{"upcoming, not a downstream probe", ProfileUpcoming, "fam/a", "k3s:golden", CauseRancher},
		{"upcoming, downstream probe on the goldens' k3s", ProfileUpcoming, "fam/down", "k3s:golden", CauseRancher},
		{"upcoming, downstream probe on another k3s", ProfileUpcoming, "fam/down", "k3s:other", CauseUndetermined},
		{"no profile, not a downstream probe", "", "fam/a", "k3s:golden", CauseRancher},
		{"no profile, downstream probe on another k3s", "", "fam/down", "k3s:other", CauseUndetermined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := cleanRun(diff(tc.probe, "/read/status", "200", "403"))
			run.header.Profile = tc.profile
			run.header.DownstreamImage = tc.downstream
			r := compareRun(KnownFile{}, run)
			require.Len(t, r.New, 1)
			assert.Equal(t, tc.want, r.New[0].Entry.Cause)
		})
	}
}

// TestInstallKnownFileCoversTheHelmCheckRun asserts that the tracked
// helm-check file for v2.15 lists the two differences the helm-check run finds
// in the request port: a run with exactly those two differences has no new
// difference and is clean, and both entries are known with no ruling. A run
// that finds the same pointers with another port has both as new.
func TestInstallKnownFileCoversTheHelmCheckRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "install", "v2.15.json"))
	require.NoError(t, err)
	known, err := ParseKnownFile(raw)
	require.NoError(t, err)
	run := func(got string) testRun {
		r := cleanRun()
		r.header.Minor = "2.15"
		r.header.Profile = ProfileHelmCheck
		r.probes = append(r.probes,
			ProbeRun{Suite: SuiteOracle, Probe: "access/impersonate-user-dropped-on-local", Differences: []Difference{
				diff("access/impersonate-user-dropped-on-local", "/whoami-with-header/body/status/userInfo/extra/requesthost/0", `"127.0.0.1:8443"`, got),
			}},
			ProbeRun{Suite: SuiteOracle, Probe: "access/whoami-through-proxy", Differences: []Difference{
				diff("access/whoami-through-proxy", "/whoami/body/status/userInfo/extra/requesthost/0", `"127.0.0.1:8443"`, got),
			}},
		)
		return r
	}

	r := compareRun(known, run(`"127.0.0.1:8447"`))
	assert.Empty(t, r.New)
	assert.True(t, r.Clean)
	assert.Empty(t, r.Known)
	assert.Empty(t, r.NoLongerSeen)
	assert.Equal(t, []string{
		"access/impersonate-user-dropped-on-local/whoami-with-header/body/status/userInfo/extra/requesthost/0",
		"access/whoami-through-proxy/whoami/body/status/userInfo/extra/requesthost/0",
	}, entryPointers(r.KnownNoRuling))
	assert.Equal(t, DriftCounts{KnownNoRuling: 2}, r.Counts)

	r = compareRun(known, run(`"127.0.0.1:8445"`))
	assert.Len(t, r.New, 2)
	assert.False(t, r.Clean)
}

// TestDriftConfigProfile asserts that the profile variable is read apart from
// the six drift variables: empty means no profile (the upcoming one), upcoming
// and helm-check are read, any other value is an error in drift mode, and with
// drift off the variable is not read.
func TestDriftConfigProfile(t *testing.T) {
	with := func(value string) func(string) string {
		vars := allDriftVars()
		vars[EnvDriftProfile] = value
		return env(vars)
	}
	for _, profile := range []string{"", ProfileUpcoming, ProfileHelmCheck} {
		t.Run("profile "+profile, func(t *testing.T) {
			cfg, on, err := DriftConfigFromEnv(with(profile), false)
			require.NoError(t, err)
			assert.True(t, on)
			assert.Equal(t, profile, cfg.Profile)
		})
	}
	t.Run("another value is an error", func(t *testing.T) {
		_, on, err := DriftConfigFromEnv(with("nightly"), false)
		require.ErrorContains(t, err, "E2E_FIDELITY_DRIFT_PROFILE=nightly is not upcoming or helm-check")
		assert.False(t, on)
	})
	t.Run("drift off does not read it", func(t *testing.T) {
		cfg, on, err := DriftConfigFromEnv(env(map[string]string{EnvDriftProfile: "nightly", envFidelityMinor: "2.16"}), false)
		require.NoError(t, err)
		assert.False(t, on)
		assert.Equal(t, DriftConfig{}, cfg)
	})
	t.Run("it is not one of the six drift variables", func(t *testing.T) {
		assert.NotContains(t, driftVariables, EnvDriftProfile)
		vars := allDriftVars()
		_, on, err := DriftConfigFromEnv(env(vars), false)
		require.NoError(t, err)
		assert.True(t, on, "the six variables alone turn drift on")
	})
}

// TestFinishDriftRunReportsTheProfile asserts that the report names the
// profile of the config before Compare runs, upcoming when the config names
// none, and that a helm-check run's new difference is install in the written
// report.
func TestFinishDriftRunReportsTheProfile(t *testing.T) {
	t.Run("no profile reports upcoming", func(t *testing.T) {
		r, _ := FinishDriftRun(finishInputs(t))
		assert.Equal(t, ProfileUpcoming, r.Profile)
	})
	t.Run("upcoming", func(t *testing.T) {
		in := finishInputs(t)
		in.Config.Profile = ProfileUpcoming
		r, _ := FinishDriftRun(in)
		assert.Equal(t, ProfileUpcoming, r.Profile)
	})
	t.Run("helm-check, with the difference it found", func(t *testing.T) {
		in := finishInputs(t)
		in.Config.Profile = ProfileHelmCheck
		in.Collector.Probe(SuiteOracle, "fam/b", false, []Difference{diff("fam/b", "/read/status", "200", "403")}, nil)
		r, code := FinishDriftRun(in)
		assert.Equal(t, ProfileHelmCheck, r.Profile)
		assert.Equal(t, 1, code)
		require.Len(t, r.New, 1)
		assert.Equal(t, CauseInstall, r.New[0].Entry.Cause)

		path := filepath.Join(t.TempDir(), "report.json")
		require.NoError(t, WriteReport(path, r))
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(raw, &decoded))
		assert.Equal(t, ProfileHelmCheck, decoded["profile"])
	})
}
