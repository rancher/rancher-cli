//go:build envtest

package envtestlog

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestStartRetriesAfterFirstFailure asserts that when the first start fails
// and the second works, Start returns the second environment with its
// configuration, stops only the first, and reports the first failure once on
// stderr and in the retry log. The second environment uses the real etcd and
// kube-apiserver binaries from KUBEBUILDER_ASSETS.
func TestStartRetriesAfterFirstFailure(t *testing.T) {
	logPath := isolate(t)
	stopped := recordStops(t)
	var built []*envtest.Environment
	newEnv := func() *envtest.Environment {
		var env *envtest.Environment
		if len(built) == 0 {
			env = fakeEnvironment(t)
		} else {
			t.Setenv(fakeEtcdEnv, "")
			env = &envtest.Environment{}
		}
		built = append(built, env)
		return env
	}

	var stderr bytes.Buffer
	env, cfg, err := Start(newEnv, nil, &stderr)
	t.Logf("stderr:\n%s", stderr.String())
	require.NoError(t, err)
	require.NotNil(t, env)
	t.Cleanup(func() { assert.NoError(t, Stop(env)) })
	require.NotNil(t, cfg)
	assert.NotEmpty(t, cfg.Host)

	require.Len(t, built, 2)
	assert.Same(t, built[1], env)
	assert.Equal(t, built[:1], *stopped)
	assert.Equal(t, 1, strings.Count(stderr.String(), wantRetryMarker))
	assert.Contains(t, stderr.String(), fakeEtcdMarker)
	assert.Equal(t, stderr.String(), readRetryLog(t, logPath))
}

// TestStartFirstTrySucceedsPrintsNothing asserts a start that works at once
// builds one environment, stops nothing, writes nothing to stderr and leaves
// the retry log empty.
func TestStartFirstTrySucceedsPrintsNothing(t *testing.T) {
	logPath := isolate(t)
	stopped := recordStops(t)
	calls := 0
	newEnv := func() *envtest.Environment {
		calls++
		return &envtest.Environment{}
	}

	var stderr bytes.Buffer
	env, cfg, err := Start(newEnv, nil, &stderr)
	require.NoError(t, err)
	require.NotNil(t, env)
	t.Cleanup(func() { assert.NoError(t, Stop(env)) })
	require.NotNil(t, cfg)

	assert.Equal(t, 1, calls)
	assert.Empty(t, *stopped)
	assert.Empty(t, stderr.String())
	assert.Empty(t, readRetryLog(t, logPath))
}
