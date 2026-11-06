package fidelity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// ErrNotForMinor is wrapped by the error a probe returns when the server's
// Rancher minor is newer than the probe's MaxMinor. The probe has sent
// nothing to the server except the request for its version.
var ErrNotForMinor = errors.New("the server's Rancher minor is newer than the probe's MaxMinor")

// serverMinorPattern matches the Version field of /rancherversion and
// captures the major and minor numbers. For example v2.15.2, v2.16.0-alpha1
// and v2.16-<commit>-head give them. head and dev have no minor and do not
// match.
var serverMinorPattern = regexp.MustCompile(`^v([1-9][0-9]*)\.(0|[1-9][0-9]*)(?:[.-]|$)`)

// notForMinor is the error checkMaxMinor returns for a server whose minor is
// newer than the probe's MaxMinor. It unwraps to ErrNotForMinor.
type notForMinor struct {
	probe    string
	maxMinor string
	version  string
}

// reason says why the probe is not checked, without the probe's name.
func (e *notForMinor) reason() string {
	return fmt.Sprintf("runs only up to Rancher %s; the server is %s", e.maxMinor, e.version)
}

func (e *notForMinor) Error() string { return "probe " + e.probe + " " + e.reason() }

func (e *notForMinor) Unwrap() error { return ErrNotForMinor }

// parseMajorMinor returns the major and minor numbers from the two captured
// groups in match[1] and match[2], or an error when a number does not fit an
// int.
func parseMajorMinor(match []string) (major, minor int, err error) {
	if major, err = strconv.Atoi(match[1]); err != nil {
		return 0, 0, err
	}
	if minor, err = strconv.Atoi(match[2]); err != nil {
		return 0, 0, err
	}
	return major, minor, nil
}

// checkMaxMinor returns nil when p has no MaxMinor or the server's minor is
// at or below it, and an error that wraps ErrNotForMinor when the server's
// minor is newer. When the server version cannot be read or has no minor, it
// returns an error that is not ErrNotForMinor: the probe neither runs nor is
// reported as not checked. The minors are compared as numbers, major first.
// The only request it sends is the one for the version, without a
// credential. A MaxMinor that is not major.minor is an error; Validate
// reports it first.
func checkMaxMinor(ctx context.Context, tgt Target, p Probe, timeout time.Duration) error {
	if p.MaxMinor == "" {
		return nil
	}
	wanted := maxMinorPattern.FindStringSubmatch(p.MaxMinor)
	if wanted == nil {
		return fmt.Errorf("probe %s: MaxMinor %q is not in the form major.minor", p.Name, p.MaxMinor)
	}
	maxMajor, maxMinor, err := parseMajorMinor(wanted)
	if err != nil {
		return fmt.Errorf("probe %s: MaxMinor %q: %w", p.Name, p.MaxMinor, err)
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := FetchRancherVersion(vctx, tgt)
	if err != nil {
		return fmt.Errorf("probe %s: reading the server version to compare with MaxMinor %s: %w", p.Name, p.MaxMinor, err)
	}
	match := serverMinorPattern.FindStringSubmatch(v.Version)
	if match == nil {
		return fmt.Errorf("probe %s: no Rancher minor in the server version %q, to compare with MaxMinor %s", p.Name, v.Version, p.MaxMinor)
	}
	major, minor, err := parseMajorMinor(match)
	if err != nil {
		return fmt.Errorf("probe %s: cannot read the minor from the server version %q: %w", p.Name, v.Version, err)
	}
	if major > maxMajor || (major == maxMajor && minor > maxMinor) {
		return &notForMinor{probe: p.Name, maxMinor: p.MaxMinor, version: v.Version}
	}
	return nil
}

// RunProbeOrSkip is RunProbeWith for a test. When the server's minor is
// newer than the probe's MaxMinor, it records the probe in drift, the drift
// collector, under suite, when drift is not nil, and skips the test with a
// message that starts "not checked:". The test stops there; the code after
// the call never runs. Any other error is returned to the caller, as
// RunProbeWith returns it.
func RunProbeOrSkip(ctx context.Context, tb testing.TB, tgt Target, p Probe, opts RunOptions, drift *DriftCollector, suite string) ([]ObservationResult, error) {
	tb.Helper()
	results, err := RunProbeWith(ctx, tgt, p, opts)
	if nfm, ok := errors.AsType[*notForMinor](err); ok {
		if drift != nil {
			drift.AddNotCheckedProbe(suite, p.Name, p.Downstream, nfm.reason())
		}
		tb.Skipf("not checked: %v", err)
	}
	return results, err
}
