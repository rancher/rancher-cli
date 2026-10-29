package conformance

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	zizmorWorkflowPath = ".github/workflows/zizmor.yaml"
	zizmorCallRef      = "$/.github/workflows/zizmor.yaml"
)

// gateWorkflow holds the parts of a workflow file the release gate checks
// read. Fields that must be absent are nodes: a zero node is an absent
// field, whatever value the field would carry.
type gateWorkflow struct {
	On          yaml.Node          `yaml:"on"`
	Env         yaml.Node          `yaml:"env"`
	Permissions yaml.Node          `yaml:"permissions"`
	Jobs        map[string]gateJob `yaml:"jobs"`
}

type gateJob struct {
	Needs           yaml.Node  `yaml:"needs"`
	If              yaml.Node  `yaml:"if"`
	ContinueOnError yaml.Node  `yaml:"continue-on-error"`
	Uses            string     `yaml:"uses"`
	Env             yaml.Node  `yaml:"env"`
	With            yaml.Node  `yaml:"with"`
	Secrets         yaml.Node  `yaml:"secrets"`
	Permissions     yaml.Node  `yaml:"permissions"`
	Steps           []gateStep `yaml:"steps"`
}

type gateStep struct {
	Name            string    `yaml:"name"`
	Uses            string    `yaml:"uses"`
	Run             string    `yaml:"run"`
	If              yaml.Node `yaml:"if"`
	ContinueOnError yaml.Node `yaml:"continue-on-error"`
	Env             yaml.Node `yaml:"env"`
	With            yaml.Node `yaml:"with"`
}

func (j gateJob) needsNames() []string { return workflowJob{Needs: j.Needs}.needsNames() }

func readGateWorkflow(t *testing.T, path string) gateWorkflow {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var wf gateWorkflow
	require.NoError(t, yaml.Unmarshal(data, &wf), path)
	require.NotEmpty(t, wf.Jobs, "%s holds no jobs", path)
	return wf
}

// reaches reports whether job from names target through needs, directly or
// through other jobs.
func (wf gateWorkflow) reaches(from, target string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for _, n := range wf.Jobs[name].needsNames() {
			if n == target || walk(n) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// releaseGateProblems returns what is wrong with the job graph of the
// release workflow: the zizmor job calls the zizmor workflow of this
// repository with nothing around the call, every other job waits for it
// directly or through another job, validate waits for it, publish waits for
// validate, and no job is conditional.
func releaseGateProblems(wf gateWorkflow) []string {
	var out []string
	zizmor, ok := wf.Jobs["zizmor"]
	if !ok {
		out = append(out, "no job named zizmor")
	} else {
		if zizmor.Uses != zizmorCallRef {
			out = append(out, fmt.Sprintf("job zizmor uses %q, want %q", zizmor.Uses, zizmorCallRef))
		}
		for field, node := range map[string]yaml.Node{
			"with":              zizmor.With,
			"secrets":           zizmor.Secrets,
			"continue-on-error": zizmor.ContinueOnError,
		} {
			if node.Kind != 0 {
				out = append(out, "job zizmor sets "+field)
			}
		}
		if len(zizmor.needsNames()) > 0 {
			out = append(out, "job zizmor must not wait for another job")
		}
	}
	for name, job := range wf.Jobs {
		if job.If.Kind != 0 {
			out = append(out, "job "+name+" is conditional")
		}
		if name != "zizmor" && ok && !wf.reaches(name, "zizmor") {
			out = append(out, "job "+name+" does not wait for zizmor")
		}
	}
	if !slices.Contains(wf.Jobs["validate"].needsNames(), "zizmor") {
		out = append(out, "job validate does not name zizmor in needs")
	}
	if !slices.Contains(wf.Jobs["publish"].needsNames(), "validate") {
		out = append(out, "job publish does not name validate in needs")
	}
	slices.Sort(out)
	return out
}

var shaRefRE = regexp.MustCompile(`@[0-9a-f]{40}$`)

// unpinnedUses lists the third-party uses of a workflow, job level and step
// level, that do not end in a full commit SHA. The self-repository form and
// the relative form are the repository's own.
func unpinnedUses(wf gateWorkflow) []string {
	var out []string
	check := func(where, uses string) {
		if uses == "" || strings.HasPrefix(uses, "$/") || strings.HasPrefix(uses, "./") {
			return
		}
		if !shaRefRE.MatchString(uses) {
			out = append(out, where+": "+uses)
		}
	}
	for name, job := range wf.Jobs {
		check("job "+name, job.Uses)
		for _, s := range job.Steps {
			check("job "+name, s.Uses)
		}
	}
	slices.Sort(out)
	return out
}

// hasTrigger reports whether the on mapping of a workflow names the event.
func (wf gateWorkflow) hasTrigger(event string) bool {
	switch wf.On.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(wf.On.Content); i += 2 {
			if wf.On.Content[i].Value == event {
				return true
			}
		}
	case yaml.ScalarNode:
		return wf.On.Value == event
	case yaml.SequenceNode:
		for _, n := range wf.On.Content {
			if n.Value == event {
				return true
			}
		}
	}
	return false
}

const gateWorkflowOK = `
on:
  push:
    tags: ["v*"]
jobs:
  zizmor:
    permissions:
      contents: read
    uses: $/.github/workflows/zizmor.yaml
  validate:
    needs: zizmor
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
  publish:
    needs: validate
`

func parseGateWorkflow(t *testing.T, text string) gateWorkflow {
	t.Helper()
	var wf gateWorkflow
	require.NoError(t, yaml.Unmarshal([]byte(text), &wf))
	return wf
}

// TestReleaseGateProblemsRules asserts the job graph check accepts the
// release shape and refuses each way of weakening the zizmor gate.
func TestReleaseGateProblemsRules(t *testing.T) {
	t.Parallel()
	edit := func(old, repl string) string {
		require.Contains(t, gateWorkflowOK, old)
		return strings.Replace(gateWorkflowOK, old, repl, 1)
	}
	assert.Empty(t, releaseGateProblems(parseGateWorkflow(t, gateWorkflowOK)))

	for _, c := range []struct {
		name, text, want string
	}{
		{"no zizmor job", strings.ReplaceAll(gateWorkflowOK, "zizmor", "lint"), "no job named zizmor"},
		{"with", edit("    uses: $/.github/workflows/zizmor.yaml\n", "    uses: $/.github/workflows/zizmor.yaml\n    with:\n      a: b\n"), "job zizmor sets with"},
		{"secrets", edit("    uses: $/.github/workflows/zizmor.yaml\n", "    uses: $/.github/workflows/zizmor.yaml\n    secrets: inherit\n"), "job zizmor sets secrets"},
		{"relative form", edit("$/.github/workflows/zizmor.yaml", "./.github/workflows/zizmor.yaml"), "job zizmor uses"},
		{"another workflow", edit("$/.github/workflows/zizmor.yaml", "$/.github/workflows/other.yaml"), "job zizmor uses"},
		{"if on zizmor", edit("    uses: $/.github/workflows/zizmor.yaml\n", "    if: always()\n    uses: $/.github/workflows/zizmor.yaml\n"), "job zizmor is conditional"},
		{"if on publish", edit("  publish:\n", "  publish:\n    if: always()\n"), "job publish is conditional"},
		{"continue-on-error", edit("    uses: $/.github/workflows/zizmor.yaml\n", "    continue-on-error: true\n    uses: $/.github/workflows/zizmor.yaml\n"), "job zizmor sets continue-on-error"},
		{"validate without needs", edit("    needs: zizmor\n", ""), "job validate does not name zizmor in needs"},
		{"publish waits for zizmor only", edit("    needs: validate\n", "    needs: zizmor\n"), "job publish does not name validate in needs"},
		{"a job with no needs", gateWorkflowOK + "  extra:\n    runs-on: ubuntu-latest\n", "job extra does not wait for zizmor"},
		{"zizmor waits for another job", edit("  zizmor:\n", "  zizmor:\n    needs: publish\n"), "job zizmor must not wait for another job"},
	} {
		problems := releaseGateProblems(parseGateWorkflow(t, c.text))
		assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }), "%s: %q not in %q", c.name, c.want, problems)
	}
	assert.NotEmpty(t, releaseGateProblems(parseGateWorkflow(t, gateWorkflowOK+"  extra:\n    runs-on: ubuntu-latest\n")))
}

// TestUnpinnedUsesRules asserts the pin check accepts a full SHA and the
// repository's own references and refuses a version tag or a branch.
func TestUnpinnedUsesRules(t *testing.T) {
	t.Parallel()
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	ok := fmt.Sprintf("jobs:\n  a:\n    steps:\n      - uses: actions/checkout@%s\n      - uses: $/.github/actions/setup-tools\n  b:\n    uses: $/.github/workflows/zizmor.yaml\n", sha)
	assert.Empty(t, unpinnedUses(parseGateWorkflow(t, ok)))
	for _, bad := range []string{
		"actions/checkout@v7",
		"actions/checkout@main",
		"actions/checkout@" + sha[:39],
		"actions/checkout@" + sha + "0",
		"actions/checkout",
	} {
		text := "jobs:\n  a:\n    steps:\n      - uses: " + bad + "\n"
		assert.NotEmpty(t, unpinnedUses(parseGateWorkflow(t, text)), bad)
	}
	assert.NotEmpty(t, unpinnedUses(parseGateWorkflow(t, "jobs:\n  a:\n    uses: o/r/.github/workflows/w.yaml@v1\n")))
}

// TestReleaseRunsZizmorBeforeAnyBuild asserts the release workflow calls
// the zizmor workflow of this repository before every other job, that the
// zizmor workflow can be called, and that every third-party action in both
// files is pinned to a full commit SHA.
func TestReleaseRunsZizmorBeforeAnyBuild(t *testing.T) {
	t.Parallel()
	release := readGateWorkflow(t, releaseWorkflowPath)
	assert.Empty(t, releaseGateProblems(release))
	assert.Empty(t, unpinnedUses(release))

	zizmor := readGateWorkflow(t, zizmorWorkflowPath)
	assert.True(t, zizmor.hasTrigger("workflow_call"), "%s must be callable with workflow_call", zizmorWorkflowPath)
	assert.Empty(t, unpinnedUses(zizmor))
}

// zizmorActionInputs are the inputs the zizmor action runs with: findings
// gate the job through its exit code, and annotations carry them.
var zizmorActionInputs = map[string]string{"advanced-security": "false", "annotations": "true"}

// zizmorSteps are the steps of the zizmor job by action and name: a
// checkout, then the zizmor action. A step that runs a command, or a third
// step of any kind, could change what the action sees or whether it runs.
var zizmorSteps = []struct{ uses, name string }{
	{"actions/checkout@", ""},
	{"zizmorcore/zizmor-action@", ""},
}

// zizmorConfigFiles are the places zizmor looks for a configuration file
// of its own. None exists: a file there could disable audits on the tagged
// commit without touching the workflow.
var zizmorConfigFiles = []string{
	"zizmor.yml", "zizmor.yaml", ".zizmor.yml", ".zizmor.yaml",
	".github/zizmor.yml", ".github/zizmor.yaml",
}

// zizmorWorkflowProblems returns what could turn the called workflow off or
// weaken it without failing the release: a condition or continue-on-error
// on a job or a step, steps other than the checkout and the zizmor action,
// an env at the workflow, job or step level (zizmor reads its settings from
// the environment too), and any input of the action other than the two it
// runs with today.
func zizmorWorkflowProblems(wf gateWorkflow) []string {
	var out []string
	if wf.Env.Kind != 0 {
		out = append(out, "the workflow sets env")
	}
	for name, job := range wf.Jobs {
		if job.If.Kind != 0 {
			out = append(out, "job "+name+" is conditional")
		}
		if job.ContinueOnError.Kind != 0 {
			out = append(out, "job "+name+" sets continue-on-error")
		}
		if job.Env.Kind != 0 {
			out = append(out, "job "+name+" sets env")
		}
		if len(job.Steps) != len(zizmorSteps) {
			out = append(out, fmt.Sprintf("job %s has %d steps, want %d", name, len(job.Steps), len(zizmorSteps)))
		}
		for i, step := range job.Steps {
			where := fmt.Sprintf("job %s step %d", name, i+1)
			if step.If.Kind != 0 {
				out = append(out, where+" is conditional")
			}
			if step.ContinueOnError.Kind != 0 {
				out = append(out, where+" sets continue-on-error")
			}
			if step.Env.Kind != 0 {
				out = append(out, where+" sets env")
			}
			if step.Run != "" {
				out = append(out, where+" runs a command")
			}
			if i < len(zizmorSteps) && (!strings.HasPrefix(step.Uses, zizmorSteps[i].uses) || step.Name != zizmorSteps[i].name) {
				out = append(out, fmt.Sprintf("%s is %q named %q, want %q named %q", where, step.Uses, step.Name, zizmorSteps[i].uses, zizmorSteps[i].name))
			}
			if strings.HasPrefix(step.Uses, "zizmorcore/zizmor-action@") {
				inputs := map[string]string{}
				if err := step.With.Decode(&inputs); err != nil {
					out = append(out, where+": the with inputs are not a map of strings: "+err.Error())
				} else if !maps.Equal(inputs, zizmorActionInputs) {
					out = append(out, fmt.Sprintf("%s runs the zizmor action with %v, want %v", where, inputs, zizmorActionInputs))
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// releaseStepProblems returns what could let the release workflow go on
// after a failed step: any job or step with a condition or continue-on-error,
// and a validate job that does not run make validate and make e2e.
func releaseStepProblems(wf gateWorkflow) []string {
	var out []string
	for name, job := range wf.Jobs {
		if job.ContinueOnError.Kind != 0 {
			out = append(out, "job "+name+" sets continue-on-error")
		}
		for i, step := range job.Steps {
			where := fmt.Sprintf("job %s step %d", name, i+1)
			if step.If.Kind != 0 {
				out = append(out, where+" is conditional")
			}
			if step.ContinueOnError.Kind != 0 {
				out = append(out, where+" sets continue-on-error")
			}
		}
	}
	for _, want := range []string{"make validate", "make e2e"} {
		if !slices.ContainsFunc(wf.Jobs["validate"].Steps, func(s gateStep) bool { return strings.TrimSpace(s.Run) == want }) {
			out = append(out, "job validate has no step that runs only "+want)
		}
	}
	slices.Sort(out)
	return out
}

var refWriteRE = regexp.MustCompile(`\bgit\s+([^\n|;&]*\s)?(push|tag)\b|\bgh\s+api\b.*(\brefs\b|createRef|/git/tags)`)

// shellCodeOf returns the lines of a shell script without comments, with
// every line that ends in a backslash joined to the next: a comment starts
// at a # outside quotes at the start of a word.
func shellCodeOf(text string) []string {
	var out []string
	var joined string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(shellWithoutComment(line), " \t")
		if code, ok := strings.CutSuffix(line, "\\"); ok {
			joined += code + " "
			continue
		}
		out = append(out, joined+line)
		joined = ""
	}
	if joined != "" {
		out = append(out, joined)
	}
	return out
}

// shellWithoutComment returns line up to the shell comment it carries: the
// first # outside single or double quotes that starts a word (at the start of
// the line or after a space or tab).
func shellWithoutComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// refWriteProblems returns the commands of a step or script that write a
// reference: git push, git tag, or gh api on a refs path, a tag object or a
// GraphQL createRef, also when the command continues on the next line. The
// release workflow creates a release only through gh release create with
// --verify-tag, which needs the tag to exist. The scan reads the text it is
// given: a step that runs another script is not followed, and the one script
// the workflow runs, release-upsert, is scanned on its own.
func refWriteProblems(text string) []string {
	var out []string
	for _, command := range shellCodeOf(text) {
		if refWriteRE.MatchString(command) {
			out = append(out, strings.TrimSpace(command))
		}
	}
	return out
}

// TestZizmorWorkflowCannotBeDisarmed asserts the zizmor workflow has no
// condition, continue-on-error or env on a job or a step, only a checkout
// and the zizmor action as steps, the action with its two inputs, and no
// zizmor configuration file in the repository; the table shows what the
// check accepts and refuses.
func TestZizmorWorkflowCannotBeDisarmed(t *testing.T) {
	t.Parallel()
	assert.Empty(t, zizmorWorkflowProblems(readGateWorkflow(t, zizmorWorkflowPath)), zizmorWorkflowPath)
	for _, path := range zizmorConfigFiles {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "%s must not exist", path)
	}

	const ok = `
jobs:
  zizmor:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - uses: zizmorcore/zizmor-action@cc914d7f3750a2d13d75c7f184a1060aa0e9d482
        with:
          advanced-security: false
          annotations: true
`
	assert.Empty(t, zizmorWorkflowProblems(parseGateWorkflow(t, ok)))
	for _, c := range []struct{ name, old, repl string }{
		{"continue-on-error on the action step", "        with:\n", "        continue-on-error: true\n        with:\n"},
		{"if on the job", "  zizmor:\n", "  zizmor:\n    if: github.event_name != 'workflow_call'\n"},
		{"continue-on-error on the job", "  zizmor:\n", "  zizmor:\n    continue-on-error: true\n"},
		{"if on the checkout step", "      - uses: actions/checkout", "      - if: false\n        uses: actions/checkout"},
		{"advanced-security on", "advanced-security: false", "advanced-security: true"},
		{"a severity floor", "annotations: true\n", "annotations: true\n          min-severity: high\n"},
		{"an input dropped", "          annotations: true\n", ""},
		{"a command before the action", "      - uses: zizmorcore", "      - run: exit 0\n      - uses: zizmorcore"},
		{"a command that removes the workflows", "      - uses: zizmorcore", "      - run: rm -rf .github/workflows\n      - uses: zizmorcore"},
		{"env on the action step", "        with:\n", "        env:\n          ZIZMOR_CONFIG: /dev/null\n        with:\n"},
		{"env on the job", "  zizmor:\n", "  zizmor:\n    env:\n      ZIZMOR_CONFIG: /dev/null\n"},
		{"env on the workflow", "jobs:\n", "env:\n  ZIZMOR_CONFIG: /dev/null\njobs:\n"},
		{"a named action step", "      - uses: zizmorcore", "      - name: audit\n        uses: zizmorcore"},
		{"an input that is a list", "          annotations: true\n", "          annotations: [true]\n"},
	} {
		require.Contains(t, ok, c.old, c.name)
		text := strings.Replace(ok, c.old, c.repl, 1)
		assert.NotEmpty(t, zizmorWorkflowProblems(parseGateWorkflow(t, text)), c.name)
	}
	noAction := strings.Replace(ok, "zizmorcore/zizmor-action@cc914d7f3750a2d13d75c7f184a1060aa0e9d482", "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", 1)
	assert.NotEmpty(t, zizmorWorkflowProblems(parseGateWorkflow(t, noAction)))

	// A bumped version, a changed checkout input and a reflowed file pass.
	bumped := strings.Replace(ok, "zizmor-action@cc914d7f3750a2d13d75c7f184a1060aa0e9d482", "zizmor-action@0000000000000000000000000000000000000000 # v9.9.9", 1)
	bumped = strings.Replace(bumped, "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n", "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n        with:\n          persist-credentials: false\n", 1)
	assert.Empty(t, zizmorWorkflowProblems(parseGateWorkflow(t, bumped)))
}

// TestReleaseStepsCannotBeSkippedOrWriteRefs asserts that no job or step of
// the release workflow can be skipped or can fail without stopping the run,
// that validate runs make validate and make e2e as steps of their own, and
// that nothing in the workflow or in release-upsert writes a reference.
func TestReleaseStepsCannotBeSkippedOrWriteRefs(t *testing.T) {
	t.Parallel()
	release := readGateWorkflow(t, releaseWorkflowPath)
	assert.Empty(t, releaseStepProblems(release))
	for name, job := range release.Jobs {
		for i, step := range job.Steps {
			assert.Empty(t, refWriteProblems(step.Run), "job %s step %d", name, i+1)
		}
	}
	assert.Empty(t, refWriteProblems(readText(t, upsertScript)))
}

// TestReleaseStepRules asserts what the step and reference checks accept
// and refuse.
func TestReleaseStepRules(t *testing.T) {
	t.Parallel()
	const ok = `
jobs:
  validate:
    steps:
      - run: make validate
      - run: make e2e
`
	assert.Empty(t, releaseStepProblems(parseGateWorkflow(t, ok)))
	for _, c := range []struct{ name, old, repl string }{
		{"continue-on-error on make validate", "      - run: make validate\n", "      - continue-on-error: true\n        run: make validate\n"},
		{"if on make e2e", "      - run: make e2e\n", "      - if: always()\n        run: make e2e\n"},
		{"continue-on-error on the job", "  validate:\n", "  validate:\n    continue-on-error: true\n"},
		{"make validate missing", "      - run: make validate\n", ""},
		{"make validate with more", "      - run: make validate\n", "      - run: make validate || true\n"},
	} {
		require.Contains(t, ok, c.old, c.name)
		text := strings.Replace(ok, c.old, c.repl, 1)
		assert.NotEmpty(t, releaseStepProblems(parseGateWorkflow(t, text)), c.name)
	}

	for _, text := range []string{
		"git push origin HEAD:refs/heads/x",
		"git push --tags",
		"git tag v1.0.0",
		"git -C dist tag v1",
		"gh api repos/o/r/git/refs -f ref=refs/tags/v1",
		"echo hi && git push",
		"git \\\n  push origin x",
		"gh api -X POST \\\n  repos/$GITHUB_REPOSITORY/git/refs -f ref=refs/tags/v1",
		"gh api graphql -f query='mutation { createRef(input: {}) { clientMutationId } }'",
		"gh api -X POST repos/o/r/git/tags -f tag=v1",
		"echo \"x # y\" && git push",
	} {
		assert.NotEmpty(t, refWriteProblems(text), text)
	}
	for _, text := range []string{
		`gh api "repos/$GITHUB_REPOSITORY/releases" --paginate`,
		"gh release create v1 --draft --verify-tag",
		"git describe --tags",
		"echo tag",
		"scripts/check-release-tag \"$TAG\"",
		"# never git push or git tag here",
		"make validate # git push is not run\nmake e2e",
		"echo done \\\n  # git tag\n",
		"gh api repos/o/r/releases \\\n  --paginate",
	} {
		assert.Empty(t, refWriteProblems(text), text)
	}
}

// permissionsOf reads a permissions node: the scope map, or the scalar
// text (read-all, write-all). An absent node returns present false.
func permissionsOf(n yaml.Node) (scopes map[string]string, scalar string, present bool) {
	switch n.Kind {
	case yaml.MappingNode:
		scopes = map[string]string{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			scopes[n.Content[i].Value] = n.Content[i+1].Value
		}
		return scopes, "", true
	case yaml.ScalarNode:
		return nil, n.Value, true
	}
	return nil, "", false
}

// permissionProblems checks one permissions node against the exact scopes
// wanted; want nil means the empty map.
func permissionProblems(where string, n yaml.Node, want map[string]string) []string {
	scopes, scalar, present := permissionsOf(n)
	switch {
	case !present:
		return []string{where + " declares no permissions"}
	case scalar != "":
		return []string{where + " permissions is the scalar " + scalar}
	case !maps.Equal(want, scopes):
		return []string{fmt.Sprintf("%s permissions are %v, want %v", where, scopes, want)}
	}
	return nil
}

var (
	readOnly     = map[string]string{"contents": "read"}
	publishScope = map[string]string{"contents": "write", "id-token": "write", "attestations": "write"}
)

// releasePermissionProblems returns what is wrong with the permission split
// of the release workflow: read only at the workflow level, zizmor and
// validate read only, publish alone holds the write scopes it needs, and
// every job declares its own permissions.
func releasePermissionProblems(wf gateWorkflow) []string {
	out := permissionProblems("workflow", wf.Permissions, readOnly)
	if _, ok := wf.Jobs["zizmor"]; !ok {
		out = append(out, "no job named zizmor")
	}
	for name, job := range wf.Jobs {
		want := readOnly
		if name == "publish" {
			want = publishScope
		}
		out = append(out, permissionProblems("job "+name, job.Permissions, want)...)
	}
	slices.Sort(out)
	return out
}

// zizmorPermissionProblems returns what is wrong with the permissions of
// the zizmor workflow: none at the workflow level, read only in its job.
// The caller's grant cannot exceed what a called workflow declares.
func zizmorPermissionProblems(wf gateWorkflow) []string {
	out := permissionProblems("workflow", wf.Permissions, nil)
	for name, job := range wf.Jobs {
		out = append(out, permissionProblems("job "+name, job.Permissions, readOnly)...)
	}
	slices.Sort(out)
	return out
}

// TestReleasePermissionSplitRules asserts the permission check accepts the
// release split and refuses each widening.
func TestReleasePermissionSplitRules(t *testing.T) {
	t.Parallel()
	const ok = `
permissions:
  contents: read
jobs:
  zizmor:
    permissions:
      contents: read
  validate:
    permissions:
      contents: read
  publish:
    permissions:
      contents: write
      id-token: write
      attestations: write
`
	assert.Empty(t, releasePermissionProblems(parseGateWorkflow(t, ok)))
	for _, c := range []struct{ name, old, repl string }{
		{"workflow write-all", "permissions:\n  contents: read\njobs:", "permissions: write-all\njobs:"},
		{"workflow contents write", "permissions:\n  contents: read\njobs:", "permissions:\n  contents: write\njobs:"},
		{"validate id-token", "  validate:\n    permissions:\n      contents: read\n", "  validate:\n    permissions:\n      contents: read\n      id-token: write\n"},
		{"zizmor without permissions", "  zizmor:\n    permissions:\n      contents: read\n", "  zizmor:\n"},
		{"publish without attestations", "      attestations: write\n", ""},
		{"publish with a scope more", "      attestations: write\n", "      attestations: write\n      packages: write\n"},
	} {
		require.Contains(t, ok, c.old, c.name)
		text := strings.Replace(ok, c.old, c.repl, 1)
		assert.NotEmpty(t, releasePermissionProblems(parseGateWorkflow(t, text)), c.name)
	}
	extra := ok + "  extra:\n    permissions:\n      contents: write\n"
	assert.NotEmpty(t, releasePermissionProblems(parseGateWorkflow(t, extra)))
	noPerms := ok + "  extra:\n    runs-on: ubuntu-latest\n"
	assert.NotEmpty(t, releasePermissionProblems(parseGateWorkflow(t, noPerms)))

	const zizmorOK = "permissions: {}\njobs:\n  zizmor:\n    permissions:\n      contents: read\n"
	assert.Empty(t, zizmorPermissionProblems(parseGateWorkflow(t, zizmorOK)))
	for _, text := range []string{
		"permissions:\n  contents: read\njobs:\n  zizmor:\n    permissions:\n      contents: read\n",
		"jobs:\n  zizmor:\n    permissions:\n      contents: read\n",
		"permissions: {}\njobs:\n  zizmor:\n    permissions:\n      contents: write\n      contents2: read\n",
		"permissions: {}\njobs:\n  zizmor:\n    permissions:\n      contents: read\n      id-token: write\n",
	} {
		assert.NotEmpty(t, zizmorPermissionProblems(parseGateWorkflow(t, text)), text)
	}
}

// TestReleasePermissionSplit asserts the permissions of the release
// workflow and of the zizmor workflow it calls: nothing writes but the
// publish job.
func TestReleasePermissionSplit(t *testing.T) {
	t.Parallel()
	assert.Empty(t, releasePermissionProblems(readGateWorkflow(t, releaseWorkflowPath)))
	assert.Empty(t, zizmorPermissionProblems(readGateWorkflow(t, zizmorWorkflowPath)))
}

var (
	uiFirstWording = []*regexp.Regexp{
		regexp.MustCompile(`(?i)UI/API`),
		regexp.MustCompile(`(?i)(UI|API)-created`),
		regexp.MustCompile(`(?i)release first`),
		regexp.MustCompile(`(?i)pushes the tag`),
	}
	spaceRunRE = regexp.MustCompile(`\s+`)
)

// sectionOf returns the text under a level-2 heading up to the next one.
func sectionOf(t *testing.T, text, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "\n## "+heading+"\n")
	require.True(t, ok, "no heading %q", heading)
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

// bulletsOf splits a section into its list items, each with whitespace
// reduced to one space and the leading dash removed.
func bulletsOf(section string) []string {
	var out []string
	for item := range strings.SplitSeq("\n"+section, "\n- ") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, spaceRunRE.ReplaceAllString(item, " "))
		}
	}
	return out
}

// leadingComment returns the comment lines at the top of a file, before
// the first line that is not a comment, a shebang or empty, whitespace
// reduced to one space.
func leadingComment(text, stop string) string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, stop) {
			break
		}
		if strings.HasPrefix(line, "#") {
			out = append(out, strings.TrimLeft(line, "# "))
		}
	}
	return spaceRunRE.ReplaceAllString(strings.Join(out, " "), " ")
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// TestReleaseTextDescribesDraftFirstOnly asserts that the release workflow,
// release-upsert, AGENTS.md, README.md and REFERENCE.md describe one way to
// cut a release, the draft-first way: none uses the wording of a release
// created in the UI before the tag exists, and the text that states the new
// flow is present.
// The forbidden list is a backstop; the required text is the guard.
func TestReleaseTextDescribesDraftFirstOnly(t *testing.T) {
	t.Parallel()
	files := []string{releaseWorkflowPath, upsertScript, "AGENTS.md", "README.md", "REFERENCE.md"}
	texts := map[string]string{}
	for _, path := range files {
		texts[path] = readText(t, path)
		for _, re := range uiFirstWording {
			assert.False(t, re.MatchString(texts[path]), "%s matches %s", path, re)
		}
	}

	header := leadingComment(texts[upsertScript], "set -eu")
	for _, want := range []string{`(?i)published`, `(?i)draft`, `(?i)refuses a published release`} {
		assert.Regexp(t, want, header, "the header of %s", upsertScript)
	}
	assert.Regexp(t, `draft release that holds every asset.*reviewed notes are pasted into it.*then it is published; the notes go in before publishing because watchers' notifications and the releases feed carry the body as it is at publish`,
		leadingComment(texts[releaseWorkflowPath], "on:"), "the header of %s", releaseWorkflowPath)

	// Each rule is matched inside its own bullet, and the bullets that state
	// the order of the cut (tag, draft, notes, publish, edit after) keep it.
	bullets := bulletsOf(sectionOf(t, texts["AGENTS.md"], "Cutting a release"))
	find := func(rule, re string) int {
		var at []int
		for i, b := range bullets {
			if regexp.MustCompile(re).MatchString(b) {
				at = append(at, i)
			}
		}
		if assert.Len(t, at, 1, "AGENTS.md, Cutting a release: one bullet for %s", rule) {
			return at[0]
		}
		return -1
	}
	tag := find("the tag push", `^Pushing a tag .vX\.Y\.Z. or .*on a commit that is already on main`)
	draft := find("a draft that holds every asset", `draft release that holds every asset, with the tag as its title`)
	notes := find("the notes pasted into the draft before publishing", `Paste the reviewed notes into the draft, in the UI or with .gh release edit <tag> --notes-file <file>., before publishing: watchers' notifications and the releases feed carry the body as it is at publish`)
	publish := find("the publish command", `Publish with .gh release edit <tag> --draft=false --prerelease=false --latest.`)
	after := find("assets stay as they are after publishing", `^After publishing, .*Assets cannot be added or changed, and a bad release is fixed only by a new version`)
	find("never move or reuse a pushed tag", `^Never move, delete and push again, or reuse a pushed tag`)
	find("only the ruleset's bypass list changes release tags", `^Only the maintainers on the bypass list of the repository's .release tags. ruleset can create, update or delete a .v\*. tag`)
	immutable := find("published releases are immutable", `Immutable releases are on for the repository: once a release is published, GitHub locks its assets and its tag`)
	find("a flake re-runs, a fix deletes and counts on", `re-run the failed jobs when the cause is a flake .*delete the draft and the tag, fix on main, and cut the next number`)
	assert.True(t, tag <= draft && draft < notes && notes < publish && publish < after,
		"the bullets must keep the order tag, draft, notes, publish, edit after: got %d %d %d %d %d", tag, draft, notes, publish, after)
	assert.Equal(t, after, immutable, "the immutable releases sentence belongs to the bullet about what stays editable after publishing")
}
