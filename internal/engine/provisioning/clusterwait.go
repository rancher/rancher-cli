package provisioning

import (
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

const (
	clusterWaitFlag    = "wait"
	clusterTimeoutFlag = "timeout"

	// clusterTimeoutDefault limits the wait of a cluster command when
	// --timeout is not given.
	clusterTimeoutDefault = 30 * time.Minute
)

// clusterWaitInterval is the time between two checks of a wait that polls.
// A package variable so tests of other packages can shorten it, through
// SetClusterWaitIntervalForTest.
var clusterWaitInterval = 5 * time.Second

// SetClusterWaitIntervalForTest sets the time between two checks of a cluster
// command's wait and returns a function that puts the old value back. A test
// package calls it once, in its TestMain, before its tests run. A test never
// sets it, because waits that run in parallel read the value.
func SetClusterWaitIntervalForTest(d time.Duration) (restore func()) {
	old := clusterWaitInterval
	clusterWaitInterval = d
	return func() { clusterWaitInterval = old }
}

// clusterWaitFlags are the flags of a cluster command that waits until
// Rancher reports the operation done.
func clusterWaitFlags() []runtime.ArgSpec {
	return []runtime.ArgSpec{
		{Name: clusterWaitFlag, Type: "bool", Desc: "wait until Rancher reports the operation done before returning (default true; --wait=false returns once the server accepts the request)"},
		{Name: clusterTimeoutFlag, Type: "string", Desc: "wait `timeout`, a positive duration, e.g. 1h (default 30m)"},
	}
}

// clusterWait is how a cluster command waits after the server accepted its
// request.
type clusterWait struct {
	On bool
	// Timeout is the limit of the wait; TimeoutText is --timeout as given, or
	// the default, as messages print it.
	Timeout     time.Duration
	TimeoutText string
}

// readClusterWait reads --wait and --timeout: the wait is on unless
// --wait=false is given, --timeout is a positive duration (default 30m),
// and --wait=false with --timeout is refused. A command calls it before any
// request.
func readClusterWait(in runtime.Input) (clusterWait, error) {
	if in.IsSet(clusterWaitFlag) && !in.Bool(clusterWaitFlag) {
		if in.String(clusterTimeoutFlag) != "" {
			return clusterWait{}, clierr.New(clierr.CodeValidation, "--wait=false conflicts with --timeout; remove one")
		}
		return clusterWait{}, nil
	}
	d, err := runtime.DurationFlag(in, clusterTimeoutFlag, clusterTimeoutDefault, "30m")
	if err != nil {
		return clusterWait{}, err
	}
	text := in.String(clusterTimeoutFlag)
	if text == "" {
		text = "30m"
	}
	return clusterWait{On: true, Timeout: d, TimeoutText: text}, nil
}

// committedWaitError is the error that ends a wait after the request was
// accepted: GENERIC, not retryable, the get command as its hint, and the
// committed details that tell a caller the operation was requested.
func committedWaitError(resourceKind, resourceName string, recovery clierr.Command, msg string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, msg).WithRetryable(false).WithHintCommand(recovery).
		WithDetails(clierr.CommittedContextDetails{Committed: true, ResourceKind: resourceKind, ResourceName: resourceName, Recovery: recovery.Field()})
}
