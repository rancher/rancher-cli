package envtestlog

import (
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// retryLogEnv names a file that receives the report of every failed first
// start, in addition to stderr. `go test` hides the output of a package that
// passes, so a start that failed and then worked would otherwise leave no
// trace.
const retryLogEnv = "ENVTEST_START_RETRY_LOG"

// stopEnv stops an environment that failed to start. Tests replace it to
// record the stops.
var stopEnv = Stop

// Start builds an environment with newEnv, captures its process output (see
// Capture) and starts it. When that start fails, it stops the environment,
// reports the failure and starts a second environment from a new call to
// newEnv, because envtest's own tries reuse the same ports and directories.
// Only one retry is made, with no wait, and any error is retried.
//
// The report goes to stderr and, when ENVTEST_START_RETRY_LOG names a file,
// is appended to it. Start returns the started environment and its
// configuration, or a nil environment and an error when the second start
// fails too. Stop the returned environment with Stop.
//
// newEnv must return a new environment on every call. stdout and stderr are
// passed to Capture; stderr also receives the report.
func Start(newEnv func() *envtest.Environment, stdout, stderr io.Writer) (*envtest.Environment, *rest.Config, error) {
	env, cfg, err := startOnce(newEnv, stdout, stderr)
	if err == nil {
		return env, cfg, nil
	}
	stopErr := stopEnv(env)

	var report strings.Builder
	fmt.Fprintf(&report, "envtest start failed; %s:\n%v\n", retryMarker, err)
	if stopErr != nil {
		fmt.Fprintf(&report, "%s%v\n", stopFailed, stopErr)
	}
	logErr := appendRetryLog(report.String())
	if logErr != nil {
		fmt.Fprintf(&report, "could not write %s: %v\n", retryLogEnv, logErr)
	}
	if stderr != nil {
		_, _ = io.WriteString(stderr, report.String())
	}

	env, cfg, err = startOnce(newEnv, stdout, stderr)
	if err == nil {
		return env, cfg, nil
	}
	secondStopErr := stopEnv(env)
	msg := "envtest start failed twice; the first failure was reported on the given stderr and in " + retryLogEnv + " when set: %w"
	if secondStopErr != nil {
		return nil, nil, fmt.Errorf(msg+"; "+stopFailed+"%w", err, secondStopErr)
	}
	return nil, nil, fmt.Errorf(msg, err)
}

const (
	retryMarker = "starting a fresh control plane once more"
	stopFailed  = "stopping the failed control plane: "
)

// startOnce builds, captures and starts one environment. On failure it
// returns the environment, so the caller can stop it, and the error with the
// process output added.
func startOnce(newEnv func() *envtest.Environment, stdout, stderr io.Writer) (*envtest.Environment, *rest.Config, error) {
	env := newEnv()
	withOutput := Capture(env, stdout, stderr)
	cfg, err := env.Start()
	if err != nil {
		return env, nil, withOutput(err)
	}
	return env, cfg, nil
}

// appendRetryLog appends text to the file named by ENVTEST_START_RETRY_LOG.
// It does nothing when the variable is empty.
func appendRetryLog(text string) error {
	path := os.Getenv(retryLogEnv)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
