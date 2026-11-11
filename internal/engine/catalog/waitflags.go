package catalog

import (
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// The CLI's own wait flags of app install and app uninstall. Helm's options
// are --helm-wait and --helm-timeout.
const (
	flagWait    = "wait"
	flagTimeout = "timeout"
)

// maxWaitTimeout is the longest --timeout, and its default: Rancher removes
// the Operation one hour after it starts, and the wait reads it.
const maxWaitTimeout = 55 * time.Minute

// waitRequest is what --wait and --timeout ask for.
type waitRequest struct {
	on      bool
	timeout time.Duration
}

// readWaitFlags reads --wait and --timeout: --timeout alone starts the wait,
// --wait=false with --timeout is refused, and --timeout is a positive
// duration of at most maxWaitTimeout.
func readWaitFlags(in runtime.Input) (waitRequest, error) {
	raw := in.String(flagTimeout)
	if in.IsSet(flagWait) && !in.Bool(flagWait) && raw != "" {
		return waitRequest{}, clierr.New(clierr.CodeValidation, "--wait=false conflicts with --timeout; remove one")
	}
	d, err := runtime.DurationFlag(in, flagTimeout, maxWaitTimeout, "10m")
	if err != nil {
		return waitRequest{}, err
	}
	if d > maxWaitTimeout {
		return waitRequest{}, clierr.New(clierr.CodeValidation,
			"--timeout "+raw+" is longer than 55m; Rancher removes the operation one hour after it starts")
	}
	return waitRequest{on: in.Bool(flagWait) || raw != "", timeout: d}, nil
}

// waitFlags are the flag specs of --wait and --timeout.
func waitFlags() []runtime.ArgSpec {
	return []runtime.ArgSpec{
		{Name: flagWait, Type: "bool", Desc: "wait until Helm's run finishes and report how it ended"},
		{Name: flagTimeout, Type: "string", Desc: "wait `timeout`, e.g. 10m (default 55m, at most 55m; starts the wait)"},
	}
}
