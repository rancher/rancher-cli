package envtestlog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	wantRetryMarker = "starting a fresh control plane once more"
	wantStopMarker  = "stopping the failed control plane: "
)

var errTarget = errors.New("target")

// emptyAggregate is an error that reports no errors beneath it.
type emptyAggregate struct{}

func (emptyAggregate) Error() string   { return "empty aggregate" }
func (emptyAggregate) Errors() []error { return nil }

// isolate clears the variables that change where envtest sends process
// output or whether it starts processes at all, and points the retry log at
// a file in a fresh directory. It returns that path.
func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv(attachEnv, "")
	t.Setenv("USE_EXISTING_CLUSTER", "")
	t.Setenv(fakeEtcdEnv, "")
	path := filepath.Join(t.TempDir(), "retry.log")
	t.Setenv(retryLogEnv, path)
	return path
}

// recordStops replaces stopEnv with a function that records each
// environment it is given, then stops it as Stop does. The original is
// restored when the test ends.
func recordStops(t *testing.T) *[]*envtest.Environment {
	t.Helper()
	var stopped []*envtest.Environment
	orig := stopEnv
	t.Cleanup(func() { stopEnv = orig })
	stopEnv = func(env *envtest.Environment) error {
		stopped = append(stopped, env)
		return Stop(env)
	}
	return &stopped
}

// fakeEnvironment returns an environment that runs this test binary as etcd
// (see TestMain), so its start fails with fakeEtcdMarker. It sets the
// variable that makes the binary behave so; the caller clears it before a
// start that should work.
func fakeEnvironment(t *testing.T) *envtest.Environment {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	t.Setenv(fakeEtcdEnv, "1")
	env := &envtest.Environment{}
	env.ControlPlane.Etcd = &envtest.Etcd{Path: self, DataDir: filepath.Join(t.TempDir(), "etcd")}
	env.ControlPlane.GetAPIServer().Path = self
	return env
}

// fakeEnvFunc returns a newEnv function whose environments all fail to
// start, and the list of environments it has built.
func fakeEnvFunc(t *testing.T) (newEnv func() *envtest.Environment, built *[]*envtest.Environment) {
	t.Helper()
	built = new([]*envtest.Environment)
	return func() *envtest.Environment {
		env := fakeEnvironment(t)
		*built = append(*built, env)
		return env
	}, built
}

func readRetryLog(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// TestStartBothFailReturnsErrorAndPrintsFirstFailure asserts that when both
// starts fail, Start returns no environment and a "failed twice" error that
// carries the process output, prints one retry block to stderr and appends
// the same block to the retry log, builds exactly two environments and stops
// both.
func TestStartBothFailReturnsErrorAndPrintsFirstFailure(t *testing.T) {
	logPath := isolate(t)
	stopped := recordStops(t)
	newEnv, built := fakeEnvFunc(t)

	var stdout, stderr bytes.Buffer
	env, cfg, err := Start(newEnv, &stdout, &stderr)
	t.Logf("stderr:\n%s", stderr.String())
	require.Error(t, err)
	assert.Nil(t, env)
	assert.Nil(t, cfg)
	require.ErrorContains(t, err, "envtest start failed twice")
	require.ErrorContains(t, err, "the first failure was reported on the given stderr and in ENVTEST_START_RETRY_LOG when set")
	require.ErrorContains(t, err, fakeEtcdMarker)

	assert.Len(t, *built, 2)
	assert.Equal(t, *built, *stopped)

	assert.Equal(t, 1, strings.Count(stderr.String(), wantRetryMarker))
	assert.Contains(t, stderr.String(), fakeEtcdMarker)
	assert.Empty(t, stdout.String())
	logged := readRetryLog(t, logPath)
	assert.Equal(t, 1, strings.Count(logged, wantRetryMarker))
	assert.Contains(t, logged, fakeEtcdMarker)
	assert.Equal(t, logged, stderr.String())
}

// TestStartPassesWritersToCapture asserts the writers given to Start receive
// the process output when envtest is asked to attach it, as they do through
// Capture.
func TestStartPassesWritersToCapture(t *testing.T) {
	isolate(t)
	t.Setenv(attachEnv, "true")
	recordStops(t)
	newEnv, _ := fakeEnvFunc(t)

	var stdout, stderr bytes.Buffer
	_, _, err := Start(newEnv, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, stdout.String(), fakeEtcdMarker)
}

// TestStartStopFailureIsReported asserts a failed stop of either failed
// control plane is reported: the first one in the stderr block and the retry
// log, the second at the end of the returned error.
func TestStartStopFailureIsReported(t *testing.T) {
	logPath := isolate(t)
	orig := stopEnv
	t.Cleanup(func() { stopEnv = orig })
	stopCalls := 0
	stopEnv = func(env *envtest.Environment) error {
		stopCalls++
		_ = Stop(env)
		return fmt.Errorf("stop %d refused", stopCalls)
	}
	newEnv, _ := fakeEnvFunc(t)

	var stderr bytes.Buffer
	_, _, err := Start(newEnv, nil, &stderr)
	require.Error(t, err)
	assert.Equal(t, 2, stopCalls)

	assert.Contains(t, stderr.String(), wantStopMarker+"stop 1 refused")
	assert.NotContains(t, stderr.String(), "stop 2 refused")
	assert.Contains(t, readRetryLog(t, logPath), wantStopMarker+"stop 1 refused")
	assert.True(t, strings.HasSuffix(err.Error(), "; "+wantStopMarker+"stop 2 refused"), err.Error())
}

// TestStartRetryLogUnwritableStillRetries asserts that a retry log path in a
// directory that does not exist is reported in the stderr block and does not
// stop the second start.
func TestStartRetryLogUnwritableStillRetries(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "missing", "retry.log")
	t.Setenv(retryLogEnv, missing)
	recordStops(t)
	newEnv, built := fakeEnvFunc(t)

	var stderr bytes.Buffer
	_, _, err := Start(newEnv, nil, &stderr)
	require.ErrorContains(t, err, "envtest start failed twice")
	assert.Len(t, *built, 2)
	assert.Contains(t, stderr.String(), "could not write "+retryLogEnv)
	assert.Contains(t, stderr.String(), missing)
	assert.Equal(t, 1, strings.Count(stderr.String(), wantRetryMarker))
	_, statErr := os.Stat(missing)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestStartWithoutRetryLogVariable asserts an unset retry log variable only
// skips the file.
func TestStartWithoutRetryLogVariable(t *testing.T) {
	isolate(t)
	t.Setenv(retryLogEnv, "")
	recordStops(t)
	newEnv, _ := fakeEnvFunc(t)

	var stderr bytes.Buffer
	_, _, err := Start(newEnv, nil, &stderr)
	require.ErrorContains(t, err, "envtest start failed twice")
	assert.NotContains(t, stderr.String(), "could not write")
	assert.Equal(t, 1, strings.Count(stderr.String(), wantRetryMarker))
}

// TestAllLeavesAre asserts the walk accepts an error only when every leaf
// matches the target, through wrapping, Kubernetes aggregates and joined
// errors.
func TestAllLeavesAre(t *testing.T) {
	t.Parallel()
	other := errors.New("other")
	wrapped := fmt.Errorf("signalling: %w", errTarget)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain match", errTarget, true},
		{"plain other", other, false},
		{"wrapped match", wrapped, true},
		{"aggregate of matches", utilerrors.NewAggregate([]error{errTarget, wrapped}), true},
		{"mixed aggregate", utilerrors.NewAggregate([]error{errTarget, other}), false},
		{"nested aggregate of matches", utilerrors.NewAggregate([]error{
			utilerrors.NewAggregate([]error{errTarget, wrapped}), wrapped,
		}), true},
		{"nested mixed aggregate", utilerrors.NewAggregate([]error{
			errTarget, utilerrors.NewAggregate([]error{errTarget, other}),
		}), false},
		{"wrapped mixed aggregate", fmt.Errorf("stop: %w",
			utilerrors.NewAggregate([]error{errTarget, other})), false},
		{"empty aggregate", emptyAggregate{}, false},
		{"joined matches", errors.Join(errTarget, wrapped), true},
		{"joined mixed", errors.Join(errTarget, other), false},
		{"aggregate holding a join", utilerrors.NewAggregate([]error{errors.Join(errTarget, other)}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, allLeavesAre(tc.err, errTarget))
		})
	}
}

// TestStopOutcome asserts how Stop combines envtest's error, the number of
// processes stopped and the error for a signal the platform does not
// support.
func TestStopOutcome(t *testing.T) {
	t.Parallel()
	other := errors.New("other")
	killFailed := errors.New("kill failed")
	onlyTarget := utilerrors.NewAggregate([]error{errTarget, fmt.Errorf("again: %w", errTarget)})
	for _, tc := range []struct {
		name    string
		stopErr error
		stopped int
		killErr error
		want    []error
	}{
		{name: "all clear"},
		{name: "EWINDOWS-only error with a stop", stopErr: onlyTarget, stopped: 2},
		{name: "EWINDOWS-only error without a stop", stopErr: onlyTarget, want: []error{onlyTarget}},
		{name: "other error", stopErr: other, stopped: 1, want: []error{other}},
		{name: "mixed aggregate", stopErr: utilerrors.NewAggregate([]error{errTarget, other}), stopped: 2,
			want: []error{other}},
		{name: "kill error", stopped: 1, killErr: killFailed, want: []error{killFailed}},
		{name: "both", stopErr: other, stopped: 1, killErr: killFailed, want: []error{other, killFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stopOutcome(tc.stopErr, tc.stopped, tc.killErr, errTarget)
			if len(tc.want) == 0 {
				assert.NoError(t, got)
				return
			}
			require.Error(t, got)
			for _, want := range tc.want {
				assert.ErrorContains(t, got, want.Error())
			}
		})
	}
	assert.Same(t, other, stopOutcome(other, 1, nil, errTarget), "a lone envtest error is returned unchanged")
	require.NoError(t, stopOutcome(nil, 0, nil, nil), "no unsupported-signal error to drop")
	assert.ErrorIs(t, stopOutcome(errTarget, 3, nil, nil), errTarget, "nil target ignores nothing")
}

// TestIsControlPlaneImage asserts the image name filter accepts the two
// control plane programs with or without an .exe suffix in any case, and
// nothing else.
func TestIsControlPlaneImage(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"etcd":                 true,
		"etcd.exe":             true,
		"ETCD.EXE":             true,
		"kube-apiserver":       true,
		"kube-apiserver.exe":   true,
		"Kube-ApiServer.Exe":   true,
		"etcdctl.exe":          false,
		"kube-apiserver2.exe":  false,
		"kubectl.exe":          false,
		"etcd.exe.bak":         false,
		"":                     false,
		"envtestlog.test.exe":  false,
		"my-etcd.exe":          false,
		"kube-apiserver.exe.x": false,
	} {
		assert.Equal(t, want, isControlPlaneImage(name), name)
	}
}
