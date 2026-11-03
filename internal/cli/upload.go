package cli

import (
	"context"
	"io"
	"os"
	"time"

	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/telemetrysend"
)

// uploadFloor is the least time that must be left before the upload's
// deadline for its first request to start. A package var so tests shrink it
// together with factsBudget.
var uploadFloor = time.Second

// uploadExcluded maps the registered names of the commands whose runs never
// upload to the reason: the commands the kubectl exec plugin runs, which
// kubectl waits for.
var uploadExcluded = map[string]string{
	"auth get-token": "the exec plugin: kubectl waits for it",
	"token":          "the exec plugin shim: kubectl waits for it",
}

// uploadCommandBound is the longest `telemetry upload` runs. A package var so
// tests shrink it together with uploadFloor.
var uploadCommandBound = telemetry.UploadCommandBound

// sendStoredEvents is the sender. Tests replace it to make it panic.
var sendStoredEvents = telemetrysend.Send

// sendNow is the sender of `telemetry upload`. Tests replace it.
var sendNow = telemetrysend.SendNow

// commandUpload builds the function `telemetry upload` runs. The function
// refuses, in this order, an exec plugin run, a release build, a missing or
// refused endpoint and recording that a control turned off, and calls the
// sender only when none applies. It reads no file before it calls the sender.
// With debug true the sender writes its lines to the run's error output.
func commandUpload(c *ucli.Command, dir, version string, view telemetry.ExperimentView) telemetry.UploadFunc {
	return func(ctx context.Context, debug bool, recording telemetry.Decision) telemetry.UploadResult {
		switch {
		case currentInvocation().ExecPlugin:
			return telemetry.UploadResult{Refusal: telemetry.RefusalExecPlugin}
		case !telemetry.IsDevelopmentVersion(version):
			return telemetry.UploadResult{Refusal: telemetry.RefusalReleaseBuild}
		case view.Endpoint == "" && view.EndpointProblem == "":
			return telemetry.UploadResult{Refusal: telemetry.RefusalNoEndpoint}
		case view.Endpoint == "":
			return telemetry.UploadResult{Refusal: telemetry.RefusalInvalidEndpoint, RefusalReason: view.EndpointProblem}
		case !recording.On:
			return telemetry.UploadResult{Refusal: telemetry.RefusalRecordingOff, RefusalReason: string(recording.By)}
		}
		var out io.Writer
		if debug {
			out = c.Root().ErrWriter
		}
		return sendNow(ctx, telemetrysend.Input{
			ConfigDir:   dir,
			Version:     version,
			Origin:      view.Endpoint,
			Encoder:     telemetrysend.NewOTLPEncoder(),
			Deadline:    time.Now().Add(uploadCommandBound),
			Floor:       uploadFloor,
			Debug:       out,
			RecordingOn: func() bool { return recordingStillOn(dir) },
		})
	}
}

// uploadStoredEvents sends the stored events after a recorded run of a
// development build whose telemetryupload experiment names an endpoint. It
// does nothing for a run that recorded no event, for an exec plugin run and
// for the commands in uploadExcluded. It shares the deadline of the facts
// refresh when that ran, else it ends 2 seconds after the event write and the
// hourly pass ended.
// With --debug typed as true on the root level, the sender writes its lines to
// the run's error output. It never changes the run: nothing it does reaches the
// exit code or standard output, and a panic is recovered.
func uploadStoredEvents(ctx context.Context, root *ucli.Command, k keptLine, in refreshInput, refreshDeadline time.Time) {
	defer func() { _ = recover() }()
	if _, excluded := uploadExcluded[in.command]; !in.recorded || excluded || currentInvocation().ExecPlugin {
		return
	}
	endpoint := currentExperiments().Endpoint
	if endpoint == "" {
		return
	}
	deadline := refreshDeadline
	if deadline.IsZero() {
		deadline = in.wrote.Add(factsBudget)
	}
	var debug io.Writer
	if k.parsed && len(k.line.levels) > 0 && k.line.levels[0].typedOn("debug") {
		debug = root.ErrWriter
	}
	dir := in.opts.ConfigDir
	sendStoredEvents(ctx, telemetrysend.Input{
		ConfigDir:   dir,
		Version:     root.Version,
		Origin:      endpoint,
		Encoder:     telemetrysend.NewOTLPEncoder(),
		Written:     in.written,
		Deadline:    deadline,
		Floor:       uploadFloor,
		Debug:       debug,
		RecordingOn: func() bool { return recordingStillOn(dir) },
	})
}

// recordingStillOn resolves the recording controls again from a fresh read of
// config.yaml in dir and the environment, by the rules of the root hook: a
// config that fails to load turns recording off. The sender calls it while it
// holds the upload lock, so it takes no lock: it only reads config.yaml and the
// environment, and any lock taken here would be taken under the upload lock.
// Reading config.yaml can wait up to 2 seconds on Windows when another program
// holds the file; the sender checks the deadline after each call.
func recordingStillOn(dir string) bool {
	setting := telemetry.ConfigUnreadable
	if cfg, err := config.Load(config.FilePath(dir)); err == nil {
		setting = telemetry.ConfigSettingOf(cfg.Telemetry)
	}
	return telemetry.Resolve(telemetry.ControlInput{Getenv: os.Getenv, Config: setting}).On
}
