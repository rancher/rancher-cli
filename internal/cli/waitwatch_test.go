package cli

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/experiment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setExperimentView makes the run of this test read value from the
// experiment variable as a build of the version, the way the root hook does,
// and clears the result when the test ends. Not for a parallel test.
func setExperimentView(t *testing.T, version, value string) {
	t.Helper()
	orig := experimentGetenv
	experimentGetenv = func(key string) string {
		if key == experiment.EnvVar {
			return value
		}
		return ""
	}
	noteExperiments(version)
	t.Cleanup(func() {
		experimentGetenv = orig
		resetExperimentState()
	})
}

// TestFactoryClientsOfferObjectWatcherExactlyWhenWaitWatchIsOn asserts the
// clients of both Cluster and ImpersonatedCluster offer engine.ObjectWatcher
// when a development build has the waitwatch experiment on, and neither does
// when the variable is empty, names another experiment, or is set in a
// release build.
func TestFactoryClientsOfferObjectWatcherExactlyWhenWaitWatchIsOn(t *testing.T) {
	tests := []struct {
		name    string
		version string
		value   string
		want    bool
	}{
		{"on", "dev", "waitwatch", true},
		{"on with another experiment", "dev", "telemetryupload,waitwatch", true},
		{"off, variable empty", "dev", "", false},
		{"off, another experiment", "dev", "telemetryupload=https://example.test", false},
		{"off, release build", "1.2.3", "waitwatch", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setExperimentView(t, tt.version, tt.value)
			f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})

			plain, err := f.Cluster(t.Context(), "local")
			require.NoError(t, err)
			_, got := plain.(engine.ObjectWatcher)
			assert.Equal(t, tt.want, got, "Cluster")

			as, err := f.ImpersonatedCluster(t.Context(), "local", engine.Impersonation{User: "u-abc"})
			require.NoError(t, err)
			_, got = as.(engine.ObjectWatcher)
			assert.Equal(t, tt.want, got, "ImpersonatedCluster")

			_, tables := plain.(engine.TableReader)
			assert.True(t, tables, "the table reader stays")
		})
	}
}
