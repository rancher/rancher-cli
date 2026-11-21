package conformance

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// harvesterRecipe is the line REFERENCE.md gives for a Harvester cloud
	// credential: a kubeconfig without the server's default entry, piped into
	// the credential create.
	harvesterRecipe = "rancher create kubeconfig --clusters <cluster-id> --include-default-entry false --apply --yes | rancher create cloud-credential harvester <display-name> --set clusterId=<cluster-id> --from-file kubeconfigContent=- --apply"

	harvesterScenario = "test/e2e/scripts/cloud-credential-harvester.txtar"
)

// harvesterScenarioDescription marks the kubeconfig the scenario creates.
const harvesterScenarioDescription = "e2e-harvester-recipe"

// harvesterPlaceholders are the values the scenario uses for the recipe's
// placeholders.
var harvesterPlaceholders = strings.NewReplacer("<cluster-id>", "c-test", "<display-name>", "harvester-cc")

// scenarioRecipe returns the recipe the scenario runs: the exec lines of the
// two commands of the recipe, in order, joined with a pipe. The scenario has
// no shell pipe, so its cp stdout and stdin steps stand for the pipe and are
// left out, as are the previews and the exec lines that check the result. The
// description the scenario gives its kubeconfig, to find it again, is left out
// too.
func scenarioRecipe(script string) string {
	var commands []string
	for line := range strings.SplitSeq(script, "\n") {
		command, ok := strings.CutPrefix(line, "exec ")
		if !ok || strings.HasSuffix(command, " --dry-run") {
			continue
		}
		if strings.HasPrefix(command, "rancher create kubeconfig ") || strings.HasPrefix(command, "rancher create cloud-credential ") {
			commands = append(commands, strings.Replace(command, " --description "+harvesterScenarioDescription, "", 1))
		}
	}
	return strings.Join(commands, " | ")
}

// TestHarvesterRecipeMatchesItsScenario asserts that the Harvester recipe line
// of REFERENCE.md, with its placeholders filled, is the pair of commands the
// end-to-end scenario runs, so the documented recipe is the one that is tested.
func TestHarvesterRecipeMatchesItsScenario(t *testing.T) {
	t.Parallel()
	reference, err := os.ReadFile("REFERENCE.md")
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(reference), harvesterRecipe+"\n"), "REFERENCE.md holds the recipe on a line of its own once")

	scenario, err := os.ReadFile(harvesterScenario)
	require.NoError(t, err)
	assert.Equal(t, harvesterPlaceholders.Replace(harvesterRecipe), scenarioRecipe(string(scenario)))
}

// TestScenarioRecipeSkipsTheOtherCommands asserts the scenario reader keeps
// only the two commands of the recipe, in order.
func TestScenarioRecipeSkipsTheOtherCommands(t *testing.T) {
	t.Parallel()
	script := "exec rancher create resource -f x.yaml --apply\nexec rancher create kubeconfig --a --description e2e-harvester-recipe\ncp stdout kc.yaml\nstdin kc.yaml\nexec rancher create cloud-credential h --b\nexec rancher get kubeconfigs -o json\n"
	assert.Equal(t, "rancher create kubeconfig --a | rancher create cloud-credential h --b", scenarioRecipe(script))
}
