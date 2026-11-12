package telemetrycmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

type statusCmd struct {
	dir  string
	view telemetry.ExperimentView
	// development is true when the build's version is a development version.
	// Only a development build sends events.
	development bool
}

// NewStatus builds `telemetry status` (local): whether recording is on,
// what decided it, and what is recorded. It also lists the experiments the
// view holds. It creates nothing and works while recording is off. The zero
// view lists nothing. It also says whether this build sends the events that
// later recorded runs store: a release build sends none whatever the view
// holds. The build's version decides first, and is reduced to the build kind
// once, here.
func NewStatus(dir string, view telemetry.ExperimentView, version string) runtime.Command {
	return &statusCmd{dir: dir, view: view, development: telemetry.IsDevelopmentVersion(version)}
}

func (statusCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry status",
		Usage: "Show whether usage recording is on and what is recorded, and any active experiments",
		Desc: "Shows whether recording is on, what decided it (the RANCHER_CLI_TELEMETRY variable, " +
			"the telemetry key of config.yaml, or the default), the number of recorded events, the number of " +
			"dropped events, the contract version (the version of the event format) and the config directory id " +
			"(a random id stored for this config directory). " +
			"It also says whether this build sends the events that later recorded runs store in this config directory " +
			"(sendsEvents in -o json): a release build sends none, and a development build sends only with an " +
			"upload endpoint and recording on; with no endpoint, the text has a sending: none line that gives the reason. " +
			"In a development build with RANCHER_CLI_EXPERIMENT set, it also lists the active experiments and the " +
			"upload endpoint. A development build with the telemetryupload experiment sends the stored events of " +
			"finished hours to that endpoint at most once an hour, after a recorded run, never from the kubectl exec plugin; " +
			"with an endpoint, status shows the last upload attempt, the next attempt, the events waiting, " +
			"the events of the current hour, and a warning for an upload lock that cannot be read. " +
			"Creates nothing and works while recording is off.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry status", "rancher telemetry status -o json"},
		Shape:       runtime.ShapeText,
	}
}

func (c statusCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	st, err := loadState(c.dir)
	if err != nil {
		return runtime.Result{}, err
	}
	warnings := keepWarnings(c.dir, st)
	exists, existsWarnings := configDirectoryExists(c.dir)
	warnings = append(warnings, existsWarnings...)

	var events, dropped int
	var id string
	var haveID bool
	var upload uploadInfo
	if exists {
		s, err := telemetry.OpenStore(c.dir)
		if err != nil {
			warnings = append(warnings, storeWarning(c.dir, err))
		} else {
			var cerr, derr error
			events, cerr = s.Count(telemetry.CountLimit)
			dropped, derr = s.DroppedCount()
			if cerr != nil {
				warnings = append(warnings, storeWarning(c.dir, cerr))
			} else if derr != nil {
				warnings = append(warnings, storeWarning(c.dir, derr))
			}
			id, haveID = s.ReadDirectoryID()
			if c.view.Active() {
				upload = readUploadInfo(s, c.dir, c.view.Endpoint != "")
				warnings = append(warnings, upload.warnings...)
			}
			_ = s.Close()
		}
	}
	limitReached := events >= telemetry.CountLimit

	data := map[string]any{
		"state":                 st.stateText(),
		"decidedBy":             string(st.decision.By),
		"configDirectory":       c.dir,
		"configDirectoryExists": exists,
		"events":                events,
		"eventsLimitReached":    limitReached,
		"droppedEvents":         dropped,
		"contractVersion":       telemetry.ContractVersion,
		"sendsEvents":           c.development && c.view.Endpoint != "" && st.decision.On,
	}
	if st.key != nil {
		data["configKey"] = *st.key
	}
	if haveID {
		data["configDirectoryId"] = id
	}

	headline := "recording is " + st.stateText()
	if st.decision.On && !exists {
		headline += "; nothing is recorded because the config directory does not exist"
	}
	lines := []string{"decided by: " + string(st.decision.By)}
	if st.key != nil {
		lines = append(lines, "key in config.yaml: "+keyText(*st.key))
	}
	existsText := "exists"
	if !exists {
		existsText = "does not exist"
	}
	lines = append(lines, fmt.Sprintf("config directory: %s (%s)", c.dir, existsText))
	eventsText := fmt.Sprintf("%d", events)
	if limitReached {
		eventsText += fmt.Sprintf(" (counting stops at %d)", telemetry.CountLimit)
	}
	lines = append(lines,
		"recorded events: "+eventsText,
		fmt.Sprintf("dropped events: %d", dropped),
		"contract version: "+telemetry.ContractVersion,
	)
	if haveID {
		lines = append(lines, "config directory id: "+id)
	}
	switch {
	case !c.development:
		lines = append(lines, "sending: none - this build sends no event; recorded events stay on this machine")
	case c.view.Endpoint == "":
		lines = append(lines, "sending: none - no upload endpoint is set; recorded events stay on this machine")
	}

	if c.view.Active() {
		data["experiments"] = c.view.Names
		data["uploadLastAttempt"] = upload.lastAttemptData()
		endpoint := "not set"
		if c.view.Endpoint != "" {
			data["uploadEndpoint"] = c.view.Endpoint
			endpoint = c.view.Endpoint
		}
		lines = append(lines,
			"experiments: "+strings.Join(c.view.Names, ", "),
			"upload endpoint: "+endpoint,
			"last upload attempt: "+upload.lastAttemptText(),
		)
		if c.view.Endpoint != "" {
			data["uploadNextAttempt"] = upload.nextAttemptData()
			data["uploadEventsWaiting"] = upload.waiting
			data["uploadEventsWaitingLimitReached"] = upload.waiting >= telemetry.CountLimit
			data["uploadEventsCurrentHour"] = upload.currentHour
			waiting := fmt.Sprintf("%d", upload.waiting)
			if upload.waiting >= telemetry.CountLimit {
				waiting += fmt.Sprintf(" (counting stops at %d)", telemetry.CountLimit)
			}
			lines = append(lines,
				"next upload attempt: "+upload.nextAttemptText(),
				"events waiting: "+waiting,
				fmt.Sprintf("events of the current hour: %d (sent after the hour ends)", upload.currentHour),
			)
		}
	}

	res := runtime.TextLines(headline, lines...)
	res.Kind = "TelemetryStatus"
	res.Data = data
	res.Warnings = append(warnings, c.view.Warnings...)
	return res, nil
}

// uploadInfo is what status reads about the upload.
type uploadInfo struct {
	state telemetry.UploadState
	// known is true when the state file read.
	known bool
	// waiting counts the events of finished hours that the next attempt sends,
	// up to telemetry.CountLimit; currentHour counts those of the current hour.
	waiting, currentHour int
	warnings             []string
}

// due reports whether the next attempt would run at the next recorded run.
func (u uploadInfo) due() bool { return !u.known || u.state.AttemptDue(time.Now()) }

// readUploadInfo reads the upload state and, when withEndpoint is true, counts
// the events waiting and checks the upload lock. It creates nothing and takes
// no lock.
func readUploadInfo(s *telemetry.Store, dir string, withEndpoint bool) uploadInfo {
	var u uploadInfo
	u.state, u.known = s.ReadUploadState()
	if !withEndpoint {
		return u
	}
	covered := make(map[string]bool, len(u.state.CoveredIDs))
	for _, id := range u.state.CoveredIDs {
		covered[id] = true
	}
	finished := map[string]bool{}
	if list, err := s.Ready(telemetry.ReadyOptions{}); err == nil {
		for _, ev := range list.Events {
			finished[ev.UID] = true
			if !covered[ev.UID] {
				u.waiting++
			}
		}
	}
	u.waiting = min(u.waiting, telemetry.CountLimit)
	if list, err := s.Ready(telemetry.ReadyOptions{CurrentHour: true}); err == nil {
		for _, ev := range list.Events {
			if !finished[ev.UID] {
				u.currentHour++
			}
		}
	}
	_, err := s.UploadLockStatus()
	unsafeDir := errors.Is(err, telemetry.ErrUnsafeUploadDir)
	switch {
	case unsafeDir:
		u.warnings = append(u.warnings, fmt.Sprintf("%s is a symlink, not a directory, or cannot be read; uploads are skipped until it is removed; "+
			"remove it, or delete it with the recorded events by running %s telemetry reset", uploadPath(dir), clierr.Rancher))
	case errors.Is(err, telemetry.ErrUploadLockUnreadable):
		u.warnings = append(u.warnings, fmt.Sprintf("the upload lock %s cannot be read; uploads are skipped until it is removed; "+
			"remove it, or delete it with the recorded events by running %s telemetry reset", filepath.Join(uploadPath(dir), "lock"), clierr.Rancher))
	}
	// A directory or a symlink in place of the state file cannot be replaced, so
	// no attempt can store its next attempt time and the limit of one attempt
	// an hour does not hold. The warning for an unsafe upload directory already
	// covers a state file below it.
	state := filepath.Join(uploadPath(dir), "state.json")
	if info, err := os.Lstat(state); err == nil && !unsafeDir && (info.IsDir() || info.Mode()&fs.ModeSymlink != 0) {
		u.warnings = append(u.warnings, fmt.Sprintf("%s is not a regular file; uploads cannot save their state and are not limited to once an hour; "+
			"remove it, or delete it with the recorded events by running %s telemetry reset", state, clierr.Rancher))
	}
	return u
}

// uploadPath is the path of telemetry/upload under dir.
func uploadPath(dir string) string { return filepath.Join(telemetryPath(dir), "upload") }

// lastAttempted reports whether the state records an attempt.
func (u uploadInfo) lastAttempted() bool { return u.known && !u.state.LastAttempt.IsZero() }

// refusedUnknown reports whether the refused count is unknown: the state has
// none, or it is negative or more than the events sent.
func (u uploadInfo) refusedUnknown() bool {
	return u.state.Refused == nil || *u.state.Refused < 0 || *u.state.Refused > u.state.Sent
}

func (u uploadInfo) lastAttemptText() string {
	if !u.lastAttempted() {
		return "no attempt yet"
	}
	st := u.state
	at := st.LastAttempt.UTC().Format(time.RFC3339)
	switch {
	case st.NoAnswer == "lock unavailable":
		return at + ", not sent: the upload lock cannot be taken"
	case st.NoAnswer != "":
		return at + ", no answer: " + st.NoAnswer
	case st.HTTPStatus == 0:
		return at + ", nothing sent"
	}
	text := fmt.Sprintf("%s, HTTP %d, ", at, st.HTTPStatus)
	if u.refusedUnknown() {
		text += "counts unknown"
	} else {
		text += fmt.Sprintf("%d accepted, %d refused", st.Sent-*st.Refused, *st.Refused)
	}
	if st.Dropped != 0 {
		text += fmt.Sprintf(", %d events deleted", st.Dropped)
	}
	return text
}

func (u uploadInfo) lastAttemptData() any {
	if !u.lastAttempted() {
		return nil
	}
	st := u.state
	var refused any
	if !u.refusedUnknown() {
		refused = *st.Refused
	}
	return map[string]any{
		"time":       st.LastAttempt.UTC().Format(time.RFC3339),
		"httpStatus": st.HTTPStatus,
		"noAnswer":   st.NoAnswer,
		"sent":       st.Sent,
		"refused":    refused,
		"dropped":    st.Dropped,
	}
}

func (u uploadInfo) nextAttemptText() string {
	if u.due() {
		return "at the next recorded run"
	}
	return "after " + u.state.NextAttempt.UTC().Format(time.RFC3339)
}

func (u uploadInfo) nextAttemptData() any {
	if u.due() {
		return nil
	}
	return u.state.NextAttempt.UTC().Format(time.RFC3339)
}
