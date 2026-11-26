package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryMutatingCommandPreviewNamesItsGates runs every result-scrub row
// (one or more per mutating command) with --dry-run, as text and as JSON, and
// asserts the preview names every flag the apply needs. The flags are the
// ones the command lists as its gates in the schema manifest, and that entry
// is checked against its own rule: --apply first, --yes when its destructive
// value is true, --allow-deprecated when its stability is deprecated. The last
// line of the text preview names the flags in the order of the manifest, and
// the Effect of the JSON preview lists them as `gates`. A command whose
// destructive value is dependsOnInput lists only --apply in the manifest; its
// preview adds --yes when the Plan returns a destructive Effect, and the test
// writes those flags out for that case. For every other command the destructive
// value in the manifest says whether the Plan returns a destructive Effect; a
// Plan and a manifest that disagree fail here. At least one command with a
// deprecated stability must reach its preview; without one, a gate lost for a
// deprecated command would go unseen. A command every row of which ends in a
// refusal at Plan has no preview and is skipped. The number of those commands
// is refusalOnlyCommandCount, and the test fails when it changes; a command
// that starts to refuse leaves the check only when this number changes. The
// manifest still lists gates for each of them; the surface rules decide whether
// it should.
func TestEveryMutatingCommandPreviewNamesItsGates(t *testing.T) {
	const refusalOnlyCommandCount = 13
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	entries := map[string]entry{}
	for _, e := range buildEntries() {
		entries[e.make(runDeps{}).Describe().Name] = e
	}
	m, err := buildManifest("v-test")
	require.NoError(t, err)
	listed := map[string]runtime.SchemaCommand{}
	for _, c := range m.Commands {
		if c.CommandType == runtime.Mutating {
			listed[c.Name] = c
		}
	}
	require.NotEmpty(t, listed)

	reached := map[string]bool{}
	refusedOnly := map[string]bool{}
	deprecatedReached := 0
	for _, row := range scrubRows() {
		if row.refused != "" {
			if _, seen := refusedOnly[row.command]; !seen {
				refusedOnly[row.command] = true
			}
			continue
		}
		refusedOnly[row.command] = false
		cmd, ok := listed[row.command]
		require.True(t, ok, "%s: no mutating command of that name in the schema manifest", row.name())
		require.NotEmpty(t, cmd.Gates, "%s: a mutating command lists its gates", row.name())
		assert.Equal(t, safety.GateApply, cmd.Gates[0], "%s: the manifest lists --apply first", row.name())
		assert.Equal(t, cmd.Destructive == "true", slices.Contains(cmd.Gates, safety.GateYes),
			"%s: the manifest lists --yes exactly when its destructive value is true", row.name())
		assert.Equal(t, cmd.Stability == string(safety.Deprecated), slices.Contains(cmd.Gates, safety.GateAllowDeprecated),
			"%s: the manifest lists --allow-deprecated exactly when its stability is deprecated", row.name())
		row.dryRun = true
		formats := []string{"", "json"}
		if len(row.formats) > 0 {
			formats = row.formats[:1]
		}
		for _, format := range formats {
			var planned []safety.Effect
			row.wrap = func(c runtime.Command) runtime.Command {
				mc, ok := c.(runtime.MutatingCommand)
				require.True(t, ok, "%s is not a mutating command", row.name())
				return effectRecorder{MutatingCommand: mc, effects: &planned}
			}
			t.Run(row.name()+" "+format, func(t *testing.T) {
				run := runScrubRow(t, wr.reg, entries, row, format, plantItemCounts(row.kind)[0])
				require.Nil(t, run.ce, "%s: --dry-run did not reach the Effect: %v", row.name(), run.ce)
				require.Len(t, planned, 1, "%s: Plan ran once", row.name())

				want := slices.Clone(cmd.Gates)
				switch cmd.Destructive {
				case "dependsOnInput":
					if planned[0].Destructive {
						want = []string{safety.GateApply, safety.GateYes}
						if cmd.Stability == "deprecated" {
							want = append(want, safety.GateAllowDeprecated)
						}
					}
				default:
					assert.Equal(t, cmd.Destructive == "true", planned[0].Destructive,
						"%s: the manifest says destructive %q and the Plan's Effect disagrees", row.name(), cmd.Destructive)
				}

				switch format {
				case "", "table":
					lines := strings.Split(strings.TrimSuffix(run.stdout, "\n"), "\n")
					assert.Equal(t, "  dry-run preview - re-run with "+strings.Join(want, " ")+" to apply the change",
						lines[len(lines)-1], row.name())
				case "json":
					data, _ := decodeEnvelope(t, "json", run.stdout)["data"].(map[string]any)
					got, _ := data["gates"].([]any)
					gates := make([]string, len(got))
					for i, g := range got {
						gates[i], _ = g.(string)
					}
					assert.Equal(t, want, gates, "%s: the gates of the rendered Effect", row.name())
				}
			})
		}
		reached[row.command] = true
		if cmd.Stability == string(safety.Deprecated) {
			deprecatedReached++
		}
	}
	for name := range listed {
		if refusedOnly[name] {
			continue
		}
		assert.True(t, reached[name], "%s: no result-scrub row reaches its --dry-run preview", name)
	}
	assert.Positive(t, deprecatedReached, "no mutating command with a deprecated stability reaches its preview; "+
		"the table in internal/runtime covers the case; remove this check with the last deprecated command")
	var skipped []string
	for name, only := range refusedOnly {
		if only {
			require.Contains(t, listed, name, "%s: a refusal-only row names no mutating command", name)
			skipped = append(skipped, name)
		}
	}
	slices.Sort(skipped)
	t.Logf("%d commands have only rows that end in a refusal at Plan and are skipped: %s", len(skipped), strings.Join(skipped, ", "))
	assert.Len(t, skipped, refusalOnlyCommandCount, "the commands that only refuse changed; update the count after reading the list above")
}
