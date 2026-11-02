package telemetrycmd

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
)

type resetCmd struct {
	dir     string
	changes changes
}

// NewReset builds `telemetry reset` (local): deletes the recorded events,
// the server facts, the config directory id and the upload state, and keeps the
// telemetry key.
func NewReset(dir string) runtime.Command { return &resetCmd{dir: dir, changes: fileChanges{}} }

func (resetCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry reset",
		Usage: "Delete what is recorded and keep the telemetry key",
		Desc: "Deletes the recorded events, the server facts (the version, edition and install id stored for each Rancher server), " +
			"the config directory id and the upload state (the upload lock and the state file in telemetry/upload), " +
			"and keeps the telemetry key and the rest of config.yaml. " +
			"Creates nothing; with no config directory there is nothing to delete.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry reset"},
		Shape:       runtime.ShapeText,
	}
}

func (c resetCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	st, err := loadState(c.dir)
	if err != nil {
		return runtime.Result{}, err
	}
	warnings := keepWarnings(c.dir, st)
	exists, existsWarnings := configDirectoryExists(c.dir)
	warnings = append(warnings, existsWarnings...)

	headline := "deleted the recorded events, the server facts, the config directory id and the upload state"
	if exists {
		if left, errs := deleteRecorded(c.changes, c.dir); len(left) > 0 {
			return runtime.Result{}, deleteFailure("telemetry reset is incomplete", c.dir, left, errs, warnings)
		}
	} else {
		headline = "nothing to delete: the config directory does not exist"
	}
	res := runtime.TextLines(headline)
	res.Kind = "TelemetryReset"
	res.Data = map[string]any{"cleared": clearedList()}
	res.Warnings = warnings
	return res, nil
}
