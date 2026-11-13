package telemetrycmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

type uploadCmd struct {
	dir    string
	upload telemetry.UploadFunc
}

// NewUpload builds `telemetry upload` (local): sends the stored events now
// through upload, a function the caller passes in. It refuses when upload
// returns a refusal and fails when it returns a failure. A nil upload makes
// Execute fail; Describe never calls it.
func NewUpload(dir string, upload telemetry.UploadFunc) runtime.Command {
	return &uploadCmd{dir: dir, upload: upload}
}

func (uploadCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry upload",
		Usage: "Send the stored usage events now (development builds with the telemetryupload experiment)",
		Desc: "Development builds with the telemetryupload experiment only. " +
			"Sends the stored events of finished hours and of the current hour to the experiment endpoint now, " +
			"without waiting for the hourly limit or a Retry-After wait, " +
			"with the same upload lock, request limits and decisions as the automatic upload. " +
			"Records its attempt: the next automatic attempt comes about an hour later. " +
			"Takes at most 30 seconds. " +
			"Never runs from the kubectl exec plugin, and a release build refuses it. " +
			"Its own run records no event. " +
			"Exits 0 whatever the endpoint answered. " +
			"sent counts every event in a request the endpoint answered with a 2xx, including the ones it refused; " +
			"refused says how many. " +
			"The data holds sent, refused, dropped, httpStatus, noAnswer, nextAttempt, timeLimitReached and skipped. " +
			"With --debug it prints one line before and one after each request on standard error.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry upload", "rancher telemetry upload -o json", "rancher telemetry upload --debug"},
		Shape:       runtime.ShapeText,
	}
}

func (c uploadCmd) Execute(ctx context.Context, in runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	if c.upload == nil {
		return runtime.Result{}, clierr.New(clierr.CodeGeneric, "telemetry upload has no sender in this run")
	}
	st, err := loadState(c.dir)
	if err != nil {
		return runtime.Result{}, err
	}
	res := c.upload(ctx, in.Bool("debug"), st.decision)
	if ctx.Err() != nil {
		return runtime.Result{}, fmt.Errorf("telemetry upload stopped before it finished; requests sent: %d", res.Requests)
	}
	if res.Refusal != "" {
		return runtime.Result{}, refusal(res)
	}
	if res.Failure == telemetry.FailureRecordingTurnedOff {
		return runtime.Result{}, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"recording was turned off while telemetry upload ran; nothing was deleted; requests sent: %d", res.Requests))
	}
	if res.Failure != "" {
		return runtime.Result{}, failure(c.dir, res)
	}
	out := runtime.TextLines(uploadLine(res))
	out.Kind = "TelemetryUpload"
	out.Data = uploadData(res)
	return out, nil
}

// endpointRemedy is the remedy for a missing or refused endpoint.
const endpointRemedy = "set RANCHER_CLI_EXPERIMENT=telemetryupload=<url>"

// refusal is the typed error for a run that was refused before any file was
// read. Its message names why and never the endpoint value.
func refusal(res telemetry.UploadResult) error {
	switch res.Refusal {
	case telemetry.RefusalExecPlugin:
		return clierr.New(clierr.CodeValidation, "telemetry upload does not run in the kubectl exec plugin")
	case telemetry.RefusalReleaseBuild:
		return clierr.New(clierr.CodeValidation,
			"telemetry upload runs only in a development build: a release build ignores RANCHER_CLI_EXPERIMENT")
	case telemetry.RefusalNoEndpoint:
		return clierr.New(clierr.CodeValidation, "telemetry upload has no endpoint; "+endpointRemedy)
	case telemetry.RefusalInvalidEndpoint:
		return clierr.New(clierr.CodeValidation, "the telemetryupload endpoint is ignored: "+res.RefusalReason+"; "+endpointRemedy)
	}
	// The last refusal value is RefusalRecordingOff; its reason is the control.
	return recordingOffError(telemetry.Control(res.RefusalReason))
}

// recordingOffError is the refusal for recording that a control turned off.
func recordingOffError(by telemetry.Control) error {
	switch by {
	case telemetry.ControlVariable:
		return clierr.New(clierr.CodeValidation,
			"recording is off: "+telemetry.EnvTelemetry+" is false; unset "+telemetry.EnvTelemetry+" to send")
	case telemetry.ControlConfig:
		return clierr.New(clierr.CodeValidation, "recording is off: the telemetry key of config.yaml is false").
			WithHintCommand(clierr.NewCommand("telemetry on"))
	}
	return clierr.New(clierr.CodeValidation, "recording is off: decided by "+string(by))
}

// failure is the typed error for an allowed run that failed. Its message names
// the failed step and the requests already sent.
func failure(dir string, res telemetry.UploadResult) error {
	return clierr.New(clierr.CodeGeneric, fmt.Sprintf("telemetry upload failed: %s; requests sent: %d", failedStep(dir, res), res.Requests))
}

// failedStep says what failed and what to do about it, for each failure value
// except recordingTurnedOff, which Execute handles first.
func failedStep(dir string, res telemetry.UploadResult) string {
	switch res.Failure {
	case telemetry.FailureUploadDirectoryUnsafe:
		return withReset(fmt.Sprintf("%s is a symlink, not a directory, or cannot be read; remove it", res.FailurePath))
	case telemetry.FailureLockUnreadable:
		return withReset(fmt.Sprintf("the upload lock %s cannot be read; remove it", res.FailurePath))
	case telemetry.FailureLockUnavailable:
		return fmt.Sprintf("the upload lock %s cannot be created because the file system refuses a hard link; "+
			"put the config directory on a file system that supports hard links, "+
			"or set RANCHER_CLI_CONFIG_DIR to a directory on one", res.FailurePath)
	case telemetry.FailureLockPermissionDenied:
		return withReset(fmt.Sprintf("the upload lock %s cannot be created: permission denied; "+
			"give this user write access to %s", res.FailurePath, filepath.Dir(res.FailurePath)))
	case telemetry.FailureLockCreateFailed:
		return withReset(fmt.Sprintf("the upload lock %s cannot be created; "+
			"check the free space and the access to %s", res.FailurePath, filepath.Dir(res.FailurePath)))
	case telemetry.FailureStoreUnavailable:
		return fmt.Sprintf("%s is a symlink, not a directory, or cannot be opened", telemetryPath(dir))
	case telemetry.FailureReadFailed:
		return fmt.Sprintf("the stored events in %s cannot be listed", filepath.Join(telemetryPath(dir), "events"))
	case telemetry.FailureRequestFailed:
		return "a request could not be built"
	}
	return "the sender stopped on an unexpected error"
}

// withReset is the text with the way out that deletes the recorded events:
// "<text>, or delete it with the recorded events by running <command>".
func withReset(text string) string {
	return clierr.Text(text+", or delete it with the recorded events",
		clierr.Lead(" by running "), clierr.NewCommand("telemetry reset"))
}

// uploadData is the data of a successful run.
func uploadData(res telemetry.UploadResult) map[string]any {
	var refused, status, next any
	if res.Refused != nil {
		refused = *res.Refused
	}
	if res.HTTPStatus != 0 {
		status = res.HTTPStatus
	}
	if !res.NextAttempt.IsZero() {
		next = res.NextAttempt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"sent":             res.Sent,
		"refused":          refused,
		"dropped":          res.Dropped,
		"httpStatus":       status,
		"noAnswer":         res.NoAnswer,
		"nextAttempt":      next,
		"timeLimitReached": res.TimeLimitReached,
		"skipped":          res.Skipped,
	}
}

// uploadLine is the one text line of a successful run.
func uploadLine(res telemetry.UploadResult) string {
	switch res.Skipped {
	case telemetry.SkippedLockHeld:
		return "nothing sent: another upload is running"
	case telemetry.SkippedNothingWaiting:
		return "nothing waiting"
	case telemetry.SkippedTimeUp:
		return "nothing sent: the 30-second limit passed before the first request"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sent %d events", res.Sent)
	if res.Sent > 0 {
		refused := "refused count unknown"
		if res.Refused != nil {
			refused = fmt.Sprintf("%d refused", *res.Refused)
		}
		fmt.Fprintf(&b, " (HTTP %d, %s)", res.AcceptedStatus, refused)
	}
	sep := ": "
	if res.Sent > 0 {
		sep = ", then "
	}
	switch res.LastOutcome {
	case telemetry.OutcomeNoAnswer:
		fmt.Fprintf(&b, "%sno answer (%s), files kept", sep, res.NoAnswer)
	case telemetry.OutcomeDeleted:
		fmt.Fprintf(&b, "%sHTTP %d, %d events deleted", sep, res.HTTPStatus, res.Dropped)
	case telemetry.OutcomeKept:
		fmt.Fprintf(&b, "%sHTTP %d, files kept", sep, res.HTTPStatus)
	}
	if res.Dropped > 0 && res.LastOutcome != telemetry.OutcomeDeleted {
		fmt.Fprintf(&b, ", %d events deleted", res.Dropped)
	}
	if res.TimeLimitReached {
		b.WriteString(", then stopped at the 30-second limit, the rest kept")
	}
	if res.NextAttempt.IsZero() {
		b.WriteString("; next automatic attempt at the next recorded run")
	} else {
		b.WriteString("; next automatic attempt after " + res.NextAttempt.UTC().Format(time.RFC3339))
	}
	return b.String()
}
