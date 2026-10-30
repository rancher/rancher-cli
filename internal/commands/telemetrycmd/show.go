package telemetrycmd

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
)

// Stored keys of an event that the table shows.
const (
	keyTime     = "time"
	keyName     = "event.name"
	keyCommand  = "io.rancher.cli.command"
	keyOutcome  = "io.rancher.cli.command.outcome"
	keyExitCode = "process.exit.code"
)

// eventList is the recorded events, oldest first. Each item holds the keys
// of the stored event. It renders as a table of the fields people read most.
type eventList []map[string]any

func (eventList) Columns() []string {
	return []string{"TIME", "EVENT", "COMMAND", "OUTCOME", "EXIT CODE"}
}

func (l eventList) Rows() [][]string {
	rows := make([][]string, 0, len(l))
	for _, ev := range l {
		rows = append(rows, []string{
			cell(ev[keyTime]), cell(ev[keyName]), cell(ev[keyCommand]), cell(ev[keyOutcome]), cell(ev[keyExitCode]),
		})
	}
	return rows
}

// cell renders a stored value as a table cell; an absent value is empty.
func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

type showCmd struct{ dir string }

// NewShow builds `telemetry show` (local): the recorded events, oldest
// first.
func NewShow(dir string) runtime.Command { return &showCmd{dir: dir} }

func (showCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name:  "telemetry show",
		Usage: "List the recorded events",
		Desc: "Lists the recorded events, oldest first. The table shows the time, event name, command, outcome and exit " +
			"code; -o json and -o yaml carry every stored field of each event. " +
			"Events that cannot be read are skipped. Creates nothing and works while recording is off.",
		CommandType: runtime.Local,
		Examples:    []string{"rancher telemetry show", "rancher telemetry show -o json"},
		Shape:       runtime.ShapeTabular,
	}
}

func (c showCmd) Execute(_ context.Context, _ runtime.Input, _ runtime.Prompter) (runtime.Result, error) {
	st, err := loadState(c.dir)
	if err != nil {
		return runtime.Result{}, err
	}
	warnings := keepWarnings(c.dir, st)
	exists, existsWarnings := configDirectoryExists(c.dir)
	warnings = append(warnings, existsWarnings...)

	list := eventList{}
	if exists {
		s, err := telemetry.OpenStore(c.dir)
		if err != nil {
			warnings = append(warnings, storeWarning(c.dir, err))
		} else {
			events, lerr := s.List()
			_ = s.Close()
			if lerr != nil {
				warnings = append(warnings, storeWarning(c.dir, lerr))
			}
			for _, ev := range events {
				list = append(list, ev.Fields)
			}
		}
	}
	return runtime.Result{Kind: "TelemetryEventList", Data: list, Warnings: warnings}, nil
}
