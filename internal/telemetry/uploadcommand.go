package telemetry

import (
	"context"
	"time"
)

// The values of UploadResult.Skipped: why an allowed run sent nothing and is
// not a failure. A run that sent a request has an empty Skipped.
const (
	// SkippedLockHeld means another process holds the upload lock.
	SkippedLockHeld = "lockHeld"
	// SkippedNothingWaiting means no stored event waits for an upload.
	SkippedNothingWaiting = "nothingWaiting"
	// SkippedTimeUp means the time limit passed before the first request.
	SkippedTimeUp = "timeUp"
)

// The values of UploadResult.Refusal: why a run was refused before any file
// was read.
const (
	// RefusalExecPlugin means the run is the kubectl exec plugin.
	RefusalExecPlugin = "execPlugin"
	// RefusalReleaseBuild means the build is a release build.
	RefusalReleaseBuild = "releaseBuild"
	// RefusalNoEndpoint means no upload endpoint is set.
	RefusalNoEndpoint = "noEndpoint"
	// RefusalInvalidEndpoint means the upload endpoint value was refused.
	RefusalInvalidEndpoint = "invalidEndpoint"
	// RefusalRecordingOff means a control turned recording off.
	RefusalRecordingOff = "recordingOff"
)

// The values of UploadResult.Failure: what went wrong in a run that was
// allowed.
const (
	// FailureUploadDirectoryUnsafe means telemetry/upload is a symlink, is not a
	// directory or cannot be read.
	FailureUploadDirectoryUnsafe = "uploadDirectoryUnsafe"
	// FailureLockUnreadable means the upload lock exists and cannot be read.
	FailureLockUnreadable = "lockUnreadable"
	// FailureLockUnavailable means the upload lock cannot be created because the
	// file system refuses a hard link.
	FailureLockUnavailable = "lockUnavailable"
	// FailureLockPermissionDenied means the upload lock cannot be created
	// because this user may not write to telemetry/upload.
	FailureLockPermissionDenied = "lockPermissionDenied"
	// FailureLockCreateFailed means the upload lock cannot be created for
	// another reason, such as a full disk.
	FailureLockCreateFailed = "lockCreateFailed"
	// FailureStoreUnavailable means the telemetry directory cannot be opened.
	FailureStoreUnavailable = "storeUnavailable"
	// FailureReadFailed means the stored events cannot be listed.
	FailureReadFailed = "readFailed"
	// FailureRequestFailed means a request could not be built.
	FailureRequestFailed = "requestFailed"
	// FailureSenderFailed means the sender stopped on an unexpected error.
	FailureSenderFailed = "senderFailed"
	// FailureRecordingTurnedOff means a control turned recording off while the
	// attempt ran.
	FailureRecordingTurnedOff = "recordingTurnedOff"
)

// The values of UploadResult.LastOutcome: what the last request's answer did.
const (
	// OutcomeAccepted means a 2xx answer.
	OutcomeAccepted = "accepted"
	// OutcomeDeleted means an answer that deletes its events: a 400, 413 or 415.
	OutcomeDeleted = "deleted"
	// OutcomeKept means any other status: the files are kept.
	OutcomeKept = "kept"
	// OutcomeNoAnswer means the request got no answer: the files are kept.
	OutcomeNoAnswer = "noAnswer"
)

// UploadResult is what one run of the upload command did. It holds no event
// field. A run with a Refusal sent nothing and touched no file. A run with a
// Failure has an empty Skipped.
type UploadResult struct {
	// Skipped is why an allowed run sent nothing and did not fail: one of the
	// Skipped constants, or empty when a request was sent.
	Skipped string
	// Sent counts the events in the requests the endpoint answered with a 2xx,
	// the events it refused included.
	Sent int
	// Dropped counts the events in the requests answered with a 400, 413 or 415,
	// which the upload deletes.
	Dropped int
	// Refused is the sum of the refused counts of the 2xx answers; nil when no
	// request got a 2xx or one of the counts is unknown.
	Refused *int
	// HTTPStatus is the status of the last request's answer, 0 when it got none.
	HTTPStatus int
	// AcceptedStatus is the status of the last 2xx answer, 0 when none got one.
	AcceptedStatus int
	// LastOutcome is what the last request's answer did, one of the Outcome
	// constants; empty when no request was made.
	LastOutcome string
	// NoAnswer says why the last request got no answer: empty with an answer,
	// else "timeout", "network error" or "refused redirect".
	NoAnswer string
	// NextAttempt is the time the state file holds for the next automatic
	// attempt; the zero time when there is no state file or the attempt is due.
	NextAttempt time.Time
	// TimeLimitReached is true when the time limit stopped the attempt with
	// events left.
	TimeLimitReached bool
	// Requests counts the requests made.
	Requests int
	// Refusal is one of the Refusal constants, or empty.
	Refusal string
	// RefusalReason is the text that goes with a refusal: the reason an endpoint
	// value was refused, or the control that turned recording off.
	RefusalReason string
	// Failure is one of the Failure constants, or empty.
	Failure string
	// FailurePath is the path a lock or directory failure names.
	FailurePath string
}

// UploadFunc runs the upload command: it checks the refusals, then makes one
// attempt that ignores the hourly limit. recording is the decision of the
// recording controls the command resolved from the configuration file.
type UploadFunc func(ctx context.Context, debug bool, recording Decision) UploadResult
