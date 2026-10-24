package telemetrycmd

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

type statusCmd struct{ dir string }

// NewStatus builds `telemetry status` (local): whether recording is on,
// which control decided, and what is recorded. It creates nothing and works
// while recording is off.
func NewStatus(dir string) runtime.Command { return &statusCmd{dir: dir} }

func (statusCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry status",
		Usage: "Show whether usage recording is on and what is recorded",
		Desc: "Shows whether recording is on, which control decided (the RANCHER_CLI_TELEMETRY variable, " +
			"the telemetry key of config.yaml, or the default), the number of recorded events, the number of " +
			"dropped events, the contract version (the version of the event format) and the config directory id " +
			"(a random id stored for this config directory). " +
			"Recorded events stay on this machine. Creates nothing and works while recording is off.",
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

	res := runtime.TextLines(headline, lines...)
	res.Kind = "TelemetryStatus"
	res.Data = data
	res.Warnings = warnings
	return res, nil
}
