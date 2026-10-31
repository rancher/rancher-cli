package cli

import (
	"fmt"
	"os"
	"slices"
	"sync"

	"github.com/rancher/rancher-cli/internal/experiment"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/telemetry"
	"github.com/rancher/rancher-cli/internal/telemetrysend"
)

// experimentGetenv reads RANCHER_CLI_EXPERIMENT. It is a variable so a test
// counts the reads: a release build makes none.
var experimentGetenv = os.Getenv

var (
	experimentMu sync.Mutex
	// experimentsNow is what the root Before hook found in the variable for
	// this run: the active experiments, the upload origin and the warning
	// lines about ignored items.
	experimentsNow telemetry.ExperimentView
)

// resetExperimentState says no experiment is on and there is nothing to warn
// about. run calls it before urfave runs, and noteExperiments before it
// decides; a run that never reaches the hook honors no experiment.
func resetExperimentState() {
	experimentMu.Lock()
	defer experimentMu.Unlock()
	experimentsNow = telemetry.ExperimentView{}
}

// noteExperiments reads RANCHER_CLI_EXPERIMENT once for the run the hook
// belongs to. It reads nothing for a completion request or for a build that
// is not a development build, and checks each known item. A warning names
// the item by its position or the experiment by its name, and never holds
// part of a value, which can hold a password.
func noteExperiments(version string) {
	resetExperimentState()
	if completionActive() || !telemetry.IsDevelopmentVersion(version) {
		return
	}
	raw := experimentGetenv(experiment.EnvVar)
	if raw == "" {
		return
	}
	items := experiment.Parse(raw)
	// The last item of a name decides, so an earlier item of a known name is
	// neither checked nor warned about.
	last := map[string]int{}
	for i, it := range items {
		if it.Kind == experiment.Known {
			last[it.Name] = i
		}
	}
	var view telemetry.ExperimentView
	warn := func(text string) {
		view.Warnings = append(view.Warnings, experiment.EnvVar+": "+text)
	}
	for i, it := range items {
		switch it.Kind {
		case experiment.Unrecognized:
			warn(fmt.Sprintf("ignored an unrecognized item (item %d)", it.Position))
		case experiment.Unknown:
			warn(fmt.Sprintf("unknown experiment %s ignored", it.Name))
		case experiment.Known:
			if last[it.Name] != i {
				continue
			}
			switch {
			case it.Name != experiment.TelemetryUpload:
				view.Names = append(view.Names, it.Name)
			case it.Value == "":
				view.Names = append(view.Names, it.Name)
				warn(it.Name + " has no endpoint; set " + it.Name + "=<url>")
			default:
				origin, err := telemetrysend.Origin(it.Value)
				if err != nil {
					warn(it.Name + " ignored: " + err.Error())
					continue
				}
				view.Names = append(view.Names, it.Name)
				view.Endpoint = origin
			}
		}
	}
	experimentMu.Lock()
	defer experimentMu.Unlock()
	experimentsNow = view
}

// currentExperiments returns what the hook last kept.
func currentExperiments() telemetry.ExperimentView {
	experimentMu.Lock()
	defer experimentMu.Unlock()
	return experimentsNow
}

// experimentView returns what the hook last kept, for `telemetry status`. An
// exec plugin run gets no warnings.
func experimentView() telemetry.ExperimentView {
	v := currentExperiments()
	if currentInvocation().ExecPlugin {
		v.Warnings = nil
	}
	return v
}

// experimentNoticeExcluded maps the registered names of the commands that
// print no experiment warning to the reason.
var experimentNoticeExcluded = map[string]string{
	"completion":       "prints a shell script",
	"auth get-token":   "the exec plugin prints nothing besides its credential",
	"token":            "the exec plugin shim prints nothing besides its credential",
	"telemetry status": "lists the same lines in its own warnings",
}

// experimentNotices drains the warning lines once, then nothing.
type experimentNotices struct {
	mu    sync.Mutex
	lines []string
}

func (n *experimentNotices) DrainNotices() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.lines
	n.lines = nil
	return out
}

// addExperimentNotices puts the warning lines of the experiment variable on
// the notices of one command's run, after any source already there. It adds
// nothing for the excluded commands, for an exec plugin run or when there is
// no warning.
func addExperimentNotices(streams *runtime.IO, name string) {
	if _, excluded := experimentNoticeExcluded[name]; excluded || currentInvocation().ExecPlugin {
		return
	}
	lines := currentExperiments().Warnings
	if len(lines) == 0 {
		return
	}
	src := &experimentNotices{lines: slices.Clone(lines)}
	if streams.Notices == nil {
		streams.Notices = src
		return
	}
	streams.Notices = noticeSources{streams.Notices, src}
}
