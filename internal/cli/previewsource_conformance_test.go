package cli

import (
	"context"
	"slices"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverReadsCommands are the commands whose preview comes from server
// reads: the Rancher catalog install, upgrade and uninstall actions.
// serverReadsRows are the rows of one command whose preview comes from
// server reads while its other rows preview through a server-side dry-run:
// node drain and node stop-drain for a caller without access to Rancher's
// local cluster. Every other mutating command previews through a server-side
// dry-run. A command or row added to either list is a reviewed exception to
// the safety pipeline.
var (
	serverReadsCommands = []string{"app install", "app upgrade", "app uninstall"}
	serverReadsRows     = []string{
		"node drain (no wait, through Rancher's node action)",
		"node drain (waited, through Rancher's node action)",
		"node stop-drain (through Rancher's node action)",
	}
)

// writeViaRows maps the rows that reach an Effect with a write path to the path
// they take: only the two node commands set one.
var writeViaRows = map[string]safety.WriteVia{
	"node drain (no wait)": safety.WriteViaKubernetesAPI,
	"node drain (waited)":  safety.WriteViaKubernetesAPI,
	"node stop-drain":      safety.WriteViaKubernetesAPI,
	serverReadsRows[0]:     safety.WriteViaRancherAction,
	serverReadsRows[1]:     safety.WriteViaRancherAction,
	serverReadsRows[2]:     safety.WriteViaRancherAction,
}

// TestEveryMutatingCommandNamesItsPreviewSource runs every result-scrub row
// (one or more per mutating command) with --dry-run, decodes its Effect and
// asserts its previewSource: serverReads for exactly the commands of
// serverReadsCommands and the rows of serverReadsRows, serverDryRun for every
// other, and its writeVia: set for the node rows to the path they take and
// absent for every other row. A row that ends in a
// refusal at Plan reaches no Effect and is skipped; every mutating command
// still has a row that reaches one, unless every row of it is a refusal (a
// verb the kind row refuses builds no Effect).
func TestEveryMutatingCommandNamesItsPreviewSource(t *testing.T) {
	wr, err := buildWiredRegistry("v-test", nil)
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	var mutating []string
	for _, c := range m.Commands {
		if c.CommandType == runtime.Mutating {
			mutating = append(mutating, c.Name)
		}
	}
	reached := map[string]bool{}
	reachedRows := map[string]bool{}
	refusedOnly := map[string]bool{}
	for _, row := range scrubRows() {
		if row.refused != "" {
			if _, seen := refusedOnly[row.command]; !seen {
				refusedOnly[row.command] = true
			}
			continue
		}
		refusedOnly[row.command] = false
		row.dryRun = true
		var planned []safety.Effect
		row.wrap = func(c runtime.Command) runtime.Command {
			mc, ok := c.(runtime.MutatingCommand)
			require.True(t, ok, "%s is not a mutating command", row.name())
			return effectRecorder{MutatingCommand: mc, effects: &planned}
		}
		format := "json"
		if len(row.formats) > 0 {
			format = row.formats[0]
		}
		t.Run(row.name(), func(t *testing.T) {
			run := runScrubRow(t, wr.reg, entries, row, format, plantItemCounts(row.kind)[0])
			require.Nil(t, run.ce, "%s: --dry-run did not reach the Effect: %v", row.name(), run.ce)
			require.Len(t, planned, 1, "%s: Plan ran once", row.name())
			want := safety.PreviewServerDryRun
			if slices.Contains(serverReadsCommands, row.command) || slices.Contains(serverReadsRows, row.name()) {
				want = safety.PreviewServerReads
			}
			assert.Equal(t, want, planned[0].PreviewSource, row.name())
			wantVia := writeViaRows[row.name()]
			assert.Equal(t, wantVia, planned[0].WriteVia, "%s: the planned Effect's write path", row.name())
			if format == "json" {
				env := decodeEnvelope(t, "json", run.stdout)
				require.Equal(t, "Effect", env["kind"], "%s: --dry-run did not render the Effect", row.name())
				data, _ := env["data"].(map[string]any)
				assert.Equal(t, string(want), data["previewSource"], "%s: the rendered Effect", row.name())
				via, has := data["writeVia"]
				if wantVia == "" {
					assert.False(t, has, "%s: only the node rows carry a writeVia", row.name())
				} else {
					assert.Equal(t, string(wantVia), via, "%s: the rendered Effect's write path", row.name())
				}
			}
			reached[row.command] = true
			reachedRows[row.name()] = true
		})
	}
	for _, c := range mutating {
		if refusedOnly[c] {
			continue
		}
		assert.True(t, reached[c], "%s: no result-scrub row reaches its --dry-run Effect", c)
	}
	for _, c := range serverReadsCommands {
		assert.True(t, reached[c], "%s is listed with serverReads but no row reached its Effect", c)
	}
	for _, name := range serverReadsRows {
		assert.True(t, reachedRows[name], "%s is listed with serverReads but no row of that name reached its Effect", name)
	}
	for name := range writeViaRows {
		assert.True(t, reachedRows[name], "%s is listed with a write path but no row of that name reached its Effect", name)
	}
}

// effectRecorder is a mutating command that records the Effect its Plan
// hands the pipeline.
type effectRecorder struct {
	runtime.MutatingCommand
	effects *[]safety.Effect
}

func (r effectRecorder) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	eff, exec, err := r.MutatingCommand.Plan(ctx, in)
	if err == nil {
		*r.effects = append(*r.effects, eff)
	}
	return eff, exec, err
}
