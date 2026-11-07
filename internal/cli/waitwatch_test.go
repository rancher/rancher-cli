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

// TestFactoryClientsAlwaysOfferObjectWatcher asserts the clients of both
// Cluster and ImpersonatedCluster offer engine.ObjectWatcher and
// engine.TableReader whatever the build version and the experiment variable
// hold. The factory reads no version, so "every build" is covered through the
// experiment view only: the test sets the version only to decide whether the
// variable is read (a development build reads it, a release build reads
// nothing). The name waitwatch is in no experiment table, so a development
// build warns that it is unknown and changes nothing else.
func TestFactoryClientsAlwaysOfferObjectWatcher(t *testing.T) {
	const unknownWarning = "RANCHER_CLI_EXPERIMENT: unknown experiment waitwatch ignored"
	values := []struct {
		name           string
		value          string
		namesWaitwatch bool
	}{
		{"variable empty", "", false},
		{"an unknown experiment name", "waitwatch", true},
		{"another experiment", "telemetryupload=https://example.test", false},
		{"another experiment and an unknown name", "telemetryupload=https://example.test,waitwatch", true},
	}
	for _, version := range []string{"dev", "1.2.3"} {
		for _, v := range values {
			t.Run(version+", "+v.name, func(t *testing.T) {
				setExperimentView(t, version, v.value)
				warnings := currentExperiments().Warnings
				if version == "dev" && v.namesWaitwatch {
					assert.Contains(t, warnings, unknownWarning)
				} else {
					assert.NotContains(t, warnings, unknownWarning)
				}
				f := newClientFactory(writeTestConfig(t), &countingStore{token: "id:secret"})

				plain, err := f.Cluster(t.Context(), "local")
				require.NoError(t, err)
				_, watches := plain.(engine.ObjectWatcher)
				assert.True(t, watches, "Cluster offers the object watcher")
				_, tables := plain.(engine.TableReader)
				assert.True(t, tables, "Cluster offers the table reader")

				as, err := f.ImpersonatedCluster(t.Context(), "local", engine.Impersonation{User: "u-abc"})
				require.NoError(t, err)
				_, watches = as.(engine.ObjectWatcher)
				assert.True(t, watches, "ImpersonatedCluster offers the object watcher")
				_, tables = as.(engine.TableReader)
				assert.True(t, tables, "ImpersonatedCluster offers the table reader")

				cached, err := f.Cluster(t.Context(), "local")
				require.NoError(t, err)
				_, watches = cached.(engine.ObjectWatcher)
				assert.True(t, watches, "the cached client from a second Cluster call offers the object watcher")
				cachedAs, err := f.ImpersonatedCluster(t.Context(), "local", engine.Impersonation{User: "u-abc"})
				require.NoError(t, err)
				_, watches = cachedAs.(engine.ObjectWatcher)
				assert.True(t, watches, "the cached client from a second ImpersonatedCluster call offers the object watcher")
			})
		}
	}
}
