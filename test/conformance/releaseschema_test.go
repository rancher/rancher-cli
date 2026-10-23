package conformance

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	releaseWorkflowPath = ".github/workflows/release.yaml"
	releaseMatrixStep   = "Build release matrix"
	releaseSchemaAsset  = "dist/rancher-schema.json"
)

// releaseSchemaStepProblems checks the run text of the release build step:
// after the platform loop and before the checksum line, the runner's own
// linux binary writes `schema --all -o json` to the schema asset, with a
// throwaway config directory and the file credential store. The checksum,
// attestation and upload steps then include the asset by their globs.
// It returns one line per problem.
func releaseSchemaStepProblems(run string) []string {
	lines := strings.Split(run, "\n")
	find := func(sub string) int {
		return slices.IndexFunc(lines, func(l string) bool {
			t := strings.TrimSpace(l)
			return !strings.HasPrefix(t, "#") && strings.Contains(l, sub)
		})
	}
	loopEnd := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "done" })
	sums := find("sha256sum")
	var out []string
	if loopEnd < 0 {
		out = append(out, "the platform loop has no done line")
	}
	if sums < 0 {
		out = append(out, "the step has no sha256sum line")
	}
	asset := find(releaseSchemaAsset)
	if asset < 0 {
		return append(out, "the step never writes "+releaseSchemaAsset)
	}
	// The command may continue on following lines; read it as one text.
	end := asset
	for end > 0 && strings.HasSuffix(strings.TrimSpace(lines[end-1]), `\`) {
		end--
	}
	command := strings.Join(lines[end:asset+1], " ")
	for _, want := range []string{
		`RANCHER_CLI_CONFIG_DIR="$(mktemp -d)"`,
		"RANCHER_CLI_CREDENTIAL_STORE=file",
		"dist/rancher-linux-amd64 schema --all -o json",
		"> " + releaseSchemaAsset,
	} {
		if !strings.Contains(command, want) {
			out = append(out, "the schema command lacks "+want)
		}
	}
	if loopEnd >= 0 && asset < loopEnd {
		out = append(out, releaseSchemaAsset+" is written before the platform loop ends")
	}
	if sums >= 0 && asset > sums {
		out = append(out, releaseSchemaAsset+" is written after the checksum line")
	}
	return out
}

func releaseStepRun(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(releaseWorkflowPath)
	require.NoError(t, err)
	var wf workflowFile
	require.NoError(t, yaml.Unmarshal(data, &wf))
	var runs []string
	for _, job := range wf.Jobs {
		for _, s := range job.Steps {
			if s.Name == releaseMatrixStep {
				runs = append(runs, s.Run)
			}
		}
	}
	require.Len(t, runs, 1, "exactly one %q step", releaseMatrixStep)
	return runs[0]
}

// TestReleaseAttachesTheSchemaManifest asserts the release workflow's build
// step writes rancher-schema.json inside the step, after the platform loop
// and before the checksum line, from the runner's own binary.
func TestReleaseAttachesTheSchemaManifest(t *testing.T) {
	assert.Empty(t, releaseSchemaStepProblems(releaseStepRun(t)))
}

// TestReleaseStepCheckCatchesFixtures asserts the check reports the step
// with the schema command removed, moved after the checksum line, placed
// before the loop ends, or lacking the throwaway directory.
func TestReleaseStepCheckCatchesFixtures(t *testing.T) {
	const loop = "for p in a b; do\n  build\ndone\n"
	const schema = "RANCHER_CLI_CONFIG_DIR=\"$(mktemp -d)\" RANCHER_CLI_CREDENTIAL_STORE=file \\\n  dist/rancher-linux-amd64 schema --all -o json > dist/rancher-schema.json\n"
	const sums = "(cd dist && sha256sum -- * > SHA256SUMS)\n"
	assert.Empty(t, releaseSchemaStepProblems(loop+schema+sums))

	assert.Contains(t, releaseSchemaStepProblems(loop+sums), "the step never writes "+releaseSchemaAsset)
	assert.Contains(t, releaseSchemaStepProblems(loop+sums+schema), releaseSchemaAsset+" is written after the checksum line")
	assert.Contains(t, releaseSchemaStepProblems(schema+loop+sums), releaseSchemaAsset+" is written before the platform loop ends")
	bare := "dist/rancher-linux-amd64 schema --all -o json > dist/rancher-schema.json\n"
	assert.NotEmpty(t, releaseSchemaStepProblems(loop+bare+sums))
	commented := "# dist/rancher-schema.json\n"
	assert.Contains(t, releaseSchemaStepProblems(loop+commented+sums), "the step never writes "+releaseSchemaAsset)
}
