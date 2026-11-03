package telemetrysend

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/rancher/rancher-cli/internal/telemetry"
)

// SendNow makes one upload attempt for the upload command and returns what it
// did. It is the attempt of Send with three differences: it ignores the hourly
// limit and any wait the state file holds, it lists the events of the current
// hour too, and its deadline is the caller's. It looks, takes the upload lock,
// lists again, sends, deletes the files the answers covered, writes the state
// file and releases the lock, and creates telemetry/upload only to take the
// lock. A recovered panic is a failure in the result, never a silent return.
//
// The caller has checked that the run may send. SendNow still sends nothing for
// a release build, an origin that Origin would change or refuse, a nil encoder
// or a nil controls function, and reports that as a failure.
func SendNow(ctx context.Context, in Input) (out telemetry.UploadResult) {
	a := &attempt{in: in, command: true, start: time.Now()}
	defer func() {
		if recover() == nil {
			return
		}
		out = a.out
		out.Skipped, out.Failure, out.FailurePath = "", telemetry.FailureSenderFailed, ""
		if a.live != nil {
			out.Requests = a.live.requests
		}
	}()
	if ctx.Err() != nil {
		return out
	}
	if origin, err := Origin(in.Origin); !telemetry.IsDevelopmentVersion(in.Version) || err != nil || origin != in.Origin ||
		in.Encoder == nil || in.RecordingOn == nil {
		return telemetry.UploadResult{Failure: telemetry.FailureSenderFailed}
	}
	if in.Deadline.Sub(clock()) < in.Floor {
		a.timeUp()
		return a.out
	}
	s, err := telemetry.OpenStore(in.ConfigDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return telemetry.UploadResult{Skipped: telemetry.SkippedNothingWaiting}
		}
		return telemetry.UploadResult{
			Failure:     telemetry.FailureStoreUnavailable,
			FailurePath: filepath.Join(in.ConfigDir, "telemetry"),
		}
	}
	defer func() { _ = s.Close() }()
	a.store = s
	a.run(ctx)
	if st, ok := s.ReadUploadState(); ok && !st.AttemptDue(time.Now()) {
		a.out.NextAttempt = st.NextAttempt.UTC()
	}
	return a.out
}
