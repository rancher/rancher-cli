package telemetrycmd

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/runtime"
)

type onCmd struct {
	dir     string
	changes changes
}

// NewOn builds `telemetry on` (local): sets the telemetry key to true. It
// creates the config directory when it is missing and deletes nothing.
func NewOn(dir string) runtime.Command { return &onCmd{dir: dir, changes: fileChanges{}} }

func (onCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry on",
		Usage: "Turn usage recording on",
		Desc: "Sets the telemetry key of config.yaml to true. " +
			"Creates the config directory when it is missing and deletes nothing. " +
			"The RANCHER_CLI_TELEMETRY variable overrides the key; the command warns when it does.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry on"},
		Shape:       runtime.ShapeText,
	}
}

func (c onCmd) Execute(ctx context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	replaced, err := c.changes.writeKey(ctx, c.dir, "true")
	if err != nil {
		return runtime.Result{}, err
	}
	key := "true"
	st := resolveKey(&key)
	res := settingResult(st, replaced)
	res.Warnings = append(variableWarning(st), overrideWarning(st, true)...)
	return res, nil
}

type offCmd struct {
	dir     string
	changes changes
}

// NewOff builds `telemetry off` (local): sets the telemetry key to false,
// then deletes the recorded events, the server facts, the config directory id
// and the upload state. It creates the config directory when it is missing, to
// hold the key.
func NewOff(dir string) runtime.Command { return &offCmd{dir: dir, changes: fileChanges{}} }

func (offCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry off",
		Usage: "Turn usage recording off and delete what is recorded",
		Desc: "Sets the telemetry key of config.yaml to false, then deletes the recorded events, the server facts " +
			"(the version, edition and install id stored for each Rancher server), the config directory id " +
			"and the upload state (the upload lock and the state file in telemetry/upload), " +
			"which leaves nothing recorded. Keeps config.yaml. " +
			"Creates the config directory when it is missing, to hold the key. " +
			"The RANCHER_CLI_TELEMETRY variable overrides the key; the command warns when it does. " +
			"Deletes even when RANCHER_CLI_TELEMETRY is set.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry off"},
		Shape:       runtime.ShapeText,
	}
}

func (c offCmd) Execute(ctx context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	replaced, err := c.changes.writeKey(ctx, c.dir, "false")
	if err != nil {
		return runtime.Result{}, err
	}
	key := "false"
	st := resolveKey(&key)
	warnings := append(variableWarning(st), overrideWarning(st, false)...)

	if left, errs := deleteRecorded(c.changes, c.dir); len(left) > 0 {
		return runtime.Result{}, deleteFailure(`the telemetry key is "false"`, c.dir, left, errs, warnings)
	}
	res := settingResult(st, replaced, "deleted the recorded events, the server facts, the config directory id and the upload state")
	res.Data.(map[string]any)["cleared"] = clearedList()
	res.Warnings = warnings
	return res, nil
}

// settingResult is the result of `on` and `off` before their own additions:
// the key written, the state the controls give and, when the key held an
// invalid text, that text. extra lines follow the ones it builds.
func settingResult(st state, replaced *string, extra ...string) runtime.Result {
	data := map[string]any{
		"configKey": *st.key,
		"state":     st.stateText(),
		"decidedBy": string(st.decision.By),
	}
	lines := []string{fmt.Sprintf("recording is %s (decided by %s)", st.stateText(), st.decision.By)}
	if replaced != nil {
		data["replacedValue"] = *replaced
		lines = append(lines, fmt.Sprintf("replaced the invalid value %q of the telemetry key", *replaced))
	}
	lines = append(lines, extra...)
	res := runtime.TextLines("telemetry key set to "+fmt.Sprintf("%q", *st.key), lines...)
	res.Kind = "TelemetrySetting"
	res.Data = data
	return res
}
