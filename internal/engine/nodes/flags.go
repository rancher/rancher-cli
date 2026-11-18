package nodes

import (
	"fmt"
	"strconv"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

const (
	flagForce          = "force"
	flagDeleteEmptyDir = "delete-emptydir-data"
	flagGracePeriod    = "grace-period"
	flagDrainTimeout   = "drain-timeout"
	flagWait           = "wait"
	flagTimeout        = "timeout"

	// defaultDrainTimeout is the time Rancher's drain may run when
	// --drain-timeout is not given, and maxDrainTimeout the most it accepts.
	defaultDrainTimeout = 2 * time.Minute
	maxDrainTimeout     = 3 * time.Hour
	// ownGracePeriod is the grace period that tells kubectl to use each
	// pod's own.
	ownGracePeriod = -1

	// waitExtra is how long the default wait outlasts Rancher's drain.
	waitExtra          = time.Minute
	defaultStopTimeout = 2 * time.Minute
)

// waitFlags is how a command waits after Rancher accepted its request.
type waitFlags struct {
	wait    bool
	timeout time.Duration
	// timeoutText is --timeout as typed, or the default as --help shows it.
	timeoutText string
}

// drainFlags are the typed options of node drain.
type drainFlags struct {
	force, deleteEmptyDir bool
	// gracePeriodText is --grace-period as typed, "" when not typed.
	gracePeriodText string
	// gracePeriod is the seconds each pod gets to stop; ownGracePeriod when
	// not typed.
	gracePeriod int64
	// drainTimeoutText is --drain-timeout as typed, "" when not typed.
	drainTimeoutText string
	// drainTimeout is how long Rancher's drain may run.
	drainTimeout time.Duration
	waitFlags
}

// parseWait reads --wait and --timeout: the wait is on unless --wait=false is
// given, --timeout is a positive duration (default def), and --wait=false with
// --timeout is refused.
func parseWait(in runtime.Input, def time.Duration) (waitFlags, error) {
	if in.IsSet(flagWait) && !in.Bool(flagWait) {
		if in.String(flagTimeout) != "" {
			return waitFlags{}, clierr.New(clierr.CodeValidation, "--wait=false conflicts with --timeout; remove one")
		}
		return waitFlags{}, nil
	}
	d, err := runtime.DurationFlag(in, flagTimeout, def, "10m")
	if err != nil {
		return waitFlags{}, err
	}
	text := in.String(flagTimeout)
	if text == "" {
		text = formatDuration(def)
	}
	return waitFlags{wait: true, timeout: d, timeoutText: text}, nil
}

// parseDrainFlags reads every option of node drain; it sends no request.
func parseDrainFlags(in runtime.Input) (drainFlags, error) {
	f := drainFlags{
		force:          in.Bool(flagForce),
		deleteEmptyDir: in.Bool(flagDeleteEmptyDir),
		gracePeriod:    ownGracePeriod,
		drainTimeout:   defaultDrainTimeout,
	}
	if text := in.String(flagGracePeriod); text != "" {
		seconds, err := strconv.ParseInt(text, 10, 64)
		if err != nil || seconds < 1 {
			return drainFlags{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("--grace-period %q must be a whole number of seconds, 1 or more", text))
		}
		f.gracePeriodText, f.gracePeriod = text, seconds
	}
	if text := in.String(flagDrainTimeout); text != "" {
		d, err := time.ParseDuration(text)
		if err != nil || d%time.Second != 0 || d < time.Second || d > maxDrainTimeout {
			return drainFlags{}, clierr.New(clierr.CodeValidation, fmt.Sprintf("--drain-timeout %q must be whole seconds from 1s to 3h, e.g. 5m", text))
		}
		f.drainTimeoutText, f.drainTimeout = text, d
	}
	w, err := parseWait(in, f.drainTimeout+waitExtra)
	if err != nil {
		return drainFlags{}, err
	}
	f.waitFlags = w
	return f, nil
}
