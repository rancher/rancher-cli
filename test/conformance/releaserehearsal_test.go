package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const rehearsalCheckScript = "scripts/release-rehearsal-check"

// anyRunText stands for a run step whose text another check asserts.
const anyRunText = "*"

// releaseTriggerProblems returns what is wrong with the triggers of the
// release workflow: exactly a push of a tag that matches v* and nothing on
// branches, and a manual run with exactly one input, tag, a required string
// without a default. Any other trigger or input would let a run with the
// attestation scopes start in a way the jobs do not expect.
func releaseTriggerProblems(wf gateWorkflow) []string {
	var on map[string]any
	if err := wf.On.Decode(&on); err != nil {
		return []string{"on is not a mapping of events: " + err.Error()}
	}
	var out []string
	events := slices.Sorted(maps.Keys(on))
	if !slices.Equal(events, []string{"push", "workflow_dispatch"}) {
		out = append(out, fmt.Sprintf("on has the events %v, want push and workflow_dispatch", events))
	}
	push, _ := on["push"].(map[string]any)
	if keys := slices.Sorted(maps.Keys(push)); !slices.Equal(keys, []string{"tags"}) {
		out = append(out, fmt.Sprintf("on.push has the keys %v, want tags only", keys))
	}
	if tags, _ := push["tags"].([]any); !slices.Equal(tags, []any{"v*"}) {
		out = append(out, fmt.Sprintf("on.push.tags is %v, want [v*]", push["tags"]))
	}
	dispatch, _ := on["workflow_dispatch"].(map[string]any)
	if keys := slices.Sorted(maps.Keys(dispatch)); !slices.Equal(keys, []string{"inputs"}) {
		out = append(out, fmt.Sprintf("on.workflow_dispatch has the keys %v, want inputs only", keys))
	}
	inputs, _ := dispatch["inputs"].(map[string]any)
	if names := slices.Sorted(maps.Keys(inputs)); !slices.Equal(names, []string{"tag"}) {
		out = append(out, fmt.Sprintf("on.workflow_dispatch has the inputs %v, want tag only", names))
	}
	tag, _ := inputs["tag"].(map[string]any)
	if tag["type"] != "string" {
		out = append(out, fmt.Sprintf("input tag has the type %v, want string", tag["type"]))
	}
	if tag["required"] != true {
		out = append(out, fmt.Sprintf("input tag has required %v, want true", tag["required"]))
	}
	if _, has := tag["default"]; has {
		out = append(out, "input tag has a default")
	}
	for key := range tag {
		if !slices.Contains([]string{"type", "required", "description"}, key) {
			out = append(out, "input tag sets "+key)
		}
	}
	return out
}

// stepSpec is one step a job of the release workflow has and no other: an
// action by name and the at sign that starts its commit, or a run text; the
// exact env and with maps (an absent map means none).
type stepSpec struct {
	uses string
	run  string
	env  map[string]string
	with map[string]string
}

// stringMap reads a mapping node of scalars.
func stringMap(n yaml.Node) (map[string]string, error) {
	out := map[string]string{}
	if n.Kind == 0 {
		return out, nil
	}
	return out, n.Decode(&out)
}

// stepListProblems returns the differences between the steps of a job and
// the steps it must have, in order.
func stepListProblems(job string, steps []gateStep, want []stepSpec) []string {
	var out []string
	if len(steps) != len(want) {
		out = append(out, fmt.Sprintf("job %s has %d steps, want %d", job, len(steps), len(want)))
	}
	for i, spec := range want {
		if i >= len(steps) {
			break
		}
		step := steps[i]
		where := fmt.Sprintf("job %s step %d", job, i+1)
		switch {
		case spec.uses != "":
			if !strings.HasPrefix(step.Uses, spec.uses) || step.Run != "" {
				out = append(out, fmt.Sprintf("%s is uses %q run %q, want uses %s", where, step.Uses, step.Run, spec.uses))
			}
		case spec.run == anyRunText:
			if step.Uses != "" || strings.TrimSpace(step.Run) == "" {
				out = append(out, fmt.Sprintf("%s is uses %q run %q, want a run step", where, step.Uses, step.Run))
			}
		default:
			if step.Uses != "" || strings.TrimSpace(step.Run) != spec.run {
				out = append(out, fmt.Sprintf("%s is uses %q run %q, want run %q", where, step.Uses, step.Run, spec.run))
			}
		}
		for field, node := range map[string]yaml.Node{"env": step.Env, "with": step.With} {
			got, err := stringMap(node)
			if err != nil {
				out = append(out, where+": "+field+" is not a mapping of scalars: "+err.Error())
				continue
			}
			wantMap := spec.env
			if field == "with" {
				wantMap = spec.with
			}
			if !maps.Equal(got, wantMap) {
				out = append(out, fmt.Sprintf("%s has %s %v, want %v", where, field, got, wantMap))
			}
		}
	}
	return out
}

const (
	checkoutUses       = "actions/checkout@"
	setupGoUses        = "actions/setup-go@"
	attestUses         = "actions/attest-build-provenance@"
	uploadArtifactUses = "actions/upload-artifact@"
	releaseBuildRun    = `scripts/release-build "$TAG"`
	rehearsalCheckRun  = `scripts/release-rehearsal-check "$TAG" "$GITHUB_SHA"`
	releaseUpsertRun   = "scripts/release-upsert dist/*"
)

var (
	attestWith   = map[string]string{"subject-path": "dist/rancher-*\ndist/install.sh\ndist/install.ps1\n"}
	checkoutWith = map[string]string{"persist-credentials": "false"}
	setupGoWith  = map[string]string{"go-version-file": "go.mod", "cache": "false"}
)

func uploadWith(name, path string) map[string]string {
	return map[string]string{
		"name":              name,
		"path":              path,
		"retention-days":    "30",
		"if-no-files-found": "error",
		"overwrite":         "true",
	}
}

// releaseStepListProblems returns what is wrong with the steps of the two
// jobs that build: each has exactly its allowed steps in order, so that no
// other command can run in them and the comparison with the rehearsal sits
// after the build and before the attestation and the draft release.
//
// rehearse refuses a ref other than main, checks out, sets up Go, builds,
// attests and keeps two artifacts; its steps hold no token and no secret, so
// it cannot create a release or a ref. publish checks out, sets up Go,
// builds, compares with the rehearsal, attests and creates the draft.
// Both attest the same files.
func releaseStepListProblems(wf gateWorkflow) []string {
	rehearse := []stepSpec{
		{run: anyRunText},
		{uses: checkoutUses, with: checkoutWith},
		{uses: setupGoUses, with: setupGoWith},
		{run: releaseBuildRun, env: map[string]string{"TAG": "${{ inputs.tag }}"}},
		{uses: attestUses, with: attestWith},
		{uses: uploadArtifactUses, with: uploadWith("release-rehearsal-${{ inputs.tag }}", "dist/")},
		{uses: uploadArtifactUses, with: uploadWith("release-rehearsal-sums-${{ inputs.tag }}", "dist/SHA256SUMS")},
	}
	tagEnv := map[string]string{"TAG": "${{ github.ref_name }}"}
	tokenEnv := map[string]string{"GH_TOKEN": "${{ github.token }}", "TAG": "${{ github.ref_name }}"}
	publish := []stepSpec{
		{uses: checkoutUses, with: checkoutWith},
		{uses: setupGoUses, with: setupGoWith},
		{run: releaseBuildRun, env: tagEnv},
		{run: rehearsalCheckRun, env: tokenEnv},
		{uses: attestUses, with: attestWith},
		{run: releaseUpsertRun, env: tokenEnv},
	}
	out := stepListProblems("rehearse", wf.Jobs["rehearse"].Steps, rehearse)
	out = append(out, stepListProblems("publish", wf.Jobs["publish"].Steps, publish)...)

	return out
}

// releaseStatusWordRE matches the ways a step or job could go on after a
// failure or run after a cancellation.
var releaseStatusWordRE = regexp.MustCompile(`continue-on-error|\b(always|failure|cancelled)\(\)`)

// releaseTextProblems returns what a scan of the release workflow text finds:
// no continue-on-error and no always(), failure() or cancelled() anywhere,
// no secrets context, and no expression inside any run text (an input or a
// ref name would then become part of the shell command; zizmor refuses the
// attacker-controlled contexts only, so this scan is the rule for all).
func releaseTextProblems(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		if releaseStatusWordRE.MatchString(code) {
			out = append(out, "a status control: "+code)
		}
		if strings.Contains(code, "secrets.") || strings.HasPrefix(code, "secrets:") {
			out = append(out, "the secrets context: "+code)
		}
	}
	var wf gateWorkflow
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		return append(out, err.Error())
	}
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		for i, step := range wf.Jobs[name].Steps {
			if strings.Contains(step.Run, "${{") {
				out = append(out, fmt.Sprintf("job %s step %d has an expression in its run text", name, i+1))
			}
		}
	}
	return out
}

// releaseGroup is the concurrency group of the release workflow: a manual
// run and a tag push never share one, so a new rehearsal cannot cancel a tag
// run that waits in the queue.
const releaseGroup = "release-${{ github.event_name }}-${{ inputs.tag || github.ref_name }}"

var (
	// releaseStepKeys are the only keys a step of rehearse or publish may
	// have. Others change how the step runs (shell, working-directory) or
	// whether a failure stops the run.
	releaseStepKeys = []string{"name", "uses", "run", "env", "with"}
	// releaseJobKeys are the only keys rehearse and publish may have: no env
	// and no defaults, which would reach every step.
	releaseJobKeys = []string{"if", "needs", "runs-on", "timeout-minutes", "permissions", "steps"}
	// validateJobKeys are the only keys validate may have; it has no
	// condition, so a run of the gate cannot be skipped.
	validateJobKeys = []string{"needs", "runs-on", "timeout-minutes", "permissions", "steps"}
	// releaseKeyedJobs are the jobs whose keys and step keys are checked,
	// with the job keys each may have.
	releaseKeyedJobs = map[string][]string{
		"validate": validateJobKeys,
		"rehearse": releaseJobKeys,
		"publish":  releaseJobKeys,
	}
)

// releaseKeyProblems returns what the keys of the release workflow let
// through that the step checks do not see: defaults at the workflow level or
// on any job, env at the workflow level, a job key of rehearse or publish
// beyond its allowed keys (validate, rehearse and publish), a step key beyond
// releaseStepKeys on those jobs, and another concurrency group or
// cancel-in-progress.
func releaseKeyProblems(text string) []string {
	var wf struct {
		Defaults    any            `yaml:"defaults"`
		Env         any            `yaml:"env"`
		Concurrency map[string]any `yaml:"concurrency"`
		Jobs        map[string]struct {
			Defaults any              `yaml:"defaults"`
			Steps    []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	var raw struct {
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		return []string{err.Error()}
	}
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return []string{err.Error()}
	}
	var out []string
	if wf.Defaults != nil {
		out = append(out, "the workflow sets defaults")
	}
	if wf.Env != nil {
		out = append(out, "the workflow sets env")
	}
	if wf.Concurrency["group"] != releaseGroup || wf.Concurrency["cancel-in-progress"] != false || len(wf.Concurrency) != 2 {
		out = append(out, fmt.Sprintf("concurrency is %v, want group %q and cancel-in-progress false", wf.Concurrency, releaseGroup))
	}
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		if wf.Jobs[name].Defaults != nil {
			out = append(out, "job "+name+" sets defaults")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(releaseKeyedJobs)) {
		allowed := releaseKeyedJobs[name]
		for key := range raw.Jobs[name] {
			if !slices.Contains(allowed, key) {
				out = append(out, fmt.Sprintf("job %s sets %s, allowed: %v", name, key, allowed))
			}
		}
		for i, step := range wf.Jobs[name].Steps {
			for key := range step {
				if !slices.Contains(releaseStepKeys, key) {
					out = append(out, fmt.Sprintf("job %s step %d sets %s, allowed: %v", name, i+1, key, releaseStepKeys))
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// TestReleaseWorkflowShape asserts the triggers, the jobs and the steps of
// the release workflow: a push of a tag and a manual run with one input; the
// jobs zizmor, validate, rehearse and publish, each with the steps it may
// have; the status controls and expressions the text scan refuses; and the
// exact permissions of every job.
func TestReleaseWorkflowShape(t *testing.T) {
	t.Parallel()
	wf := readGateWorkflow(t, releaseWorkflowPath)
	assert.Empty(t, releaseTriggerProblems(wf))
	assert.Empty(t, releaseStepListProblems(wf))
	assert.Empty(t, releaseTextProblems(readText(t, releaseWorkflowPath)))
	assert.Empty(t, releaseKeyProblems(readText(t, releaseWorkflowPath)))
	assert.Empty(t, releaseGateProblems(wf))
	assert.Empty(t, releasePermissionProblems(wf))
	assert.Empty(t, releaseStepProblems(wf))
	assert.Equal(t, 20, parseTimeout(t, "rehearse"), "rehearse has timeout-minutes 20")
}

func parseTimeout(t *testing.T, job string) int {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			TimeoutMinutes int `yaml:"timeout-minutes"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(readText(t, releaseWorkflowPath)), &wf))
	return wf.Jobs[job].TimeoutMinutes
}

// TestReleaseTriggerRules asserts each change of the triggers that the check
// refuses, on the text of the release workflow.
func TestReleaseTriggerRules(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseWorkflowPath)
	for _, c := range []struct{ name, old, repl string }{
		{"a pull request trigger", "on:\n  push:\n", "on:\n  pull_request:\n  push:\n"},
		{"a schedule", "on:\n  push:\n", "on:\n  schedule:\n    - cron: \"0 0 * * *\"\n  push:\n"},
		{"branches on push", "    tags: [\"v*\"]\n", "    tags: [\"v*\"]\n    branches: [main]\n"},
		{"another tag pattern", "    tags: [\"v*\"]\n", "    tags: [\"*\"]\n"},
		{"a second input", "        required: true\n", "        required: true\n      other:\n        type: string\n"},
		{"a default for tag", "        required: true\n", "        required: true\n        default: v0.0.0\n"},
		{"tag not required", "        required: true\n", "        required: false\n"},
		{"tag of another type", "        type: string\n", "        type: boolean\n"},
		{"tag with options", "        required: true\n", "        required: true\n        options: [v1.0.0]\n"},
		{"no manual run", "  workflow_dispatch:\n    inputs:\n      tag:\n        description: The planned release tag, for example v0.1.0-beta.2\n        type: string\n        required: true\n", ""},
	} {
		require.Equal(t, 1, strings.Count(text, c.old), c.name)
		wf := parseGateWorkflow(t, strings.Replace(text, c.old, c.repl, 1))
		assert.NotEmpty(t, releaseTriggerProblems(wf), c.name)
	}
}

// mutateSteps returns a copy of the workflow with the steps of one job
// replaced by what change returns.
func mutateSteps(wf gateWorkflow, job string, change func([]gateStep) []gateStep) gateWorkflow {
	jobs := maps.Clone(wf.Jobs)
	j := jobs[job]
	j.Steps = change(slices.Clone(j.Steps))
	jobs[job] = j
	wf.Jobs = jobs
	return wf
}

func runStep(run string) gateStep { return gateStep{Run: run} }

func envNode(t *testing.T, text string) yaml.Node {
	t.Helper()
	var n yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(text), &n))
	return *n.Content[0]
}

// TestReleaseStepListRules asserts each change of the steps of rehearse and
// publish that the check refuses: a step removed, moved, added or changed;
// another command in a step; a token in a job that must not hold one.
func TestReleaseStepListRules(t *testing.T) {
	t.Parallel()
	wf := readGateWorkflow(t, releaseWorkflowPath)
	require.Empty(t, releaseStepListProblems(wf))

	remove := func(i int) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { return slices.Delete(s, i, i+1) }
	}
	swap := func(i, j int) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { s[i], s[j] = s[j], s[i]; return s }
	}
	appendStep := func(run string) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { return append(s, runStep(run)) }
	}
	setRun := func(i int, run string) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { s[i].Run = run; return s }
	}
	setUses := func(i int, uses string) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { s[i].Uses = uses; return s }
	}
	setEnv := func(i int, env yaml.Node) func([]gateStep) []gateStep {
		return func(s []gateStep) []gateStep { s[i].Env = env; return s }
	}
	token := envNode(t, "GH_TOKEN: ${{ github.token }}\nTAG: ${{ inputs.tag }}\n")

	for _, c := range []struct {
		name   string
		job    string
		change func([]gateStep) []gateStep
	}{
		{"the comparison removed from publish", "publish", remove(3)},
		{"the comparison after the attestation", "publish", swap(3, 4)},
		{"the comparison before the build", "publish", swap(2, 3)},
		{"the comparison ignores its result", "publish", setRun(3, rehearsalCheckRun+" || true")},
		{"the comparison with another commit", "publish", setRun(3, `scripts/release-rehearsal-check "$TAG" "${{ github.event.head_commit.id }}"`)},
		{"the comparison with no commit", "publish", setRun(3, `scripts/release-rehearsal-check "$TAG"`)},
		{"the comparison then true", "publish", setRun(3, rehearsalCheckRun+"; true")},
		{"publish builds in a loop", "publish", setRun(2, "for p in linux/amd64; do GOOS=linux VERSION=\"$TAG\" ./scripts/build; done")},
		{"publish builds with go", "publish", setRun(2, "go build -o dist/rancher ./cmd/rancher")},
		{"publish uploads with gh", "publish", appendStep("gh release upload \"$TAG\" dist/*")},
		{"the upsert dropped from publish", "publish", remove(5)},
		{"publish with the upsert changed", "publish", setRun(5, "scripts/release-upsert dist/* || true")},
		{"the upsert in rehearse", "rehearse", appendStep(releaseUpsertRun)},
		{"gh release create in rehearse", "rehearse", appendStep("gh release create \"$TAG\" --draft dist/*")},
		{"a release created through the API in rehearse", "rehearse", appendStep("gh api -X POST repos/$GITHUB_REPOSITORY/releases -f tag_name=\"$TAG\"")},
		{"curl in rehearse", "rehearse", appendStep("curl -X POST https://api.github.com/repos/o/r/releases")},
		{"rehearse without the ref refusal", "rehearse", remove(0)},
		{"rehearse with the refusal after the checkout", "rehearse", swap(0, 1)},
		{"rehearse builds in a loop", "rehearse", setRun(3, "for p in linux/amd64; do GOOS=linux VERSION=\"$TAG\" ./scripts/build; done")},
		{"rehearse without the full artifact", "rehearse", remove(5)},
		{"rehearse without the sums artifact", "rehearse", remove(6)},
		{"rehearse with an unpinned-name action", "rehearse", setUses(5, "actions/download-artifact@v4")},
		{"a token in the build step of rehearse", "rehearse", setEnv(3, token)},
		{"a token in an artifact step of rehearse", "rehearse", setEnv(5, token)},
	} {
		changed := mutateSteps(wf, c.job, c.change)
		assert.NotEmpty(t, releaseStepListProblems(changed), c.name)
	}
}

// TestReleaseKeyRules asserts each key the check refuses: a shell or a
// working directory on a step, defaults at the workflow or a job, env at the
// workflow or on rehearse or publish, another job key, and another
// concurrency group.
func TestReleaseKeyRules(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseWorkflowPath)
	require.Empty(t, releaseKeyProblems(text))
	const compare = "      - name: Compare with the rehearsal\n"
	const defaults = "    defaults:\n      run:\n        shell: 'true {0}'\n"
	for _, c := range []struct{ name, old, repl string }{
		{"a shell on the comparison step", compare, compare + "        shell: 'true {0}'\n"},
		{"a working directory on the comparison step", compare, compare + "        working-directory: install\n"},
		{"a shell on a step of rehearse", "      - name: Build release assets\n        env:\n          TAG: ${{ inputs.tag }}\n", "      - name: Build release assets\n        shell: 'true {0}'\n        env:\n          TAG: ${{ inputs.tag }}\n"},
		{"defaults on publish", "  publish:\n", "  publish:\n" + defaults},
		{"defaults on rehearse", "  rehearse:\n", "  rehearse:\n" + defaults},
		{"defaults on validate", "  validate:\n", "  validate:\n" + defaults},
		{"env on validate", "  validate:\n", "  validate:\n    env:\n      BASH_ENV: /tmp/x\n"},
		{"a condition on validate", "  validate:\n", "  validate:\n    if: github.event_name == 'push'\n"},
		{"a shell on the make validate step", "      - run: make validate\n", "      - shell: 'true {0}'\n        run: make validate\n"},
		{"a working directory on the make e2e step", "      - run: make e2e\n", "      - working-directory: install\n        run: make e2e\n"},
		{"defaults at the workflow", "concurrency:\n", "defaults:\n  run:\n    shell: 'true {0}'\n\nconcurrency:\n"},
		{"env on rehearse", "  rehearse:\n", "  rehearse:\n    env:\n      GH_TOKEN: ${{ github.token }}\n"},
		{"env on publish", "  publish:\n", "  publish:\n    env:\n      GH_TOKEN: ${{ github.token }}\n"},
		{"env at the workflow", "concurrency:\n", "env:\n  GH_TOKEN: ${{ github.token }}\n\nconcurrency:\n"},
		{"a container on publish", "  publish:\n", "  publish:\n    container: alpine\n"},
		{"the group of the old form", "group: release-${{ github.event_name }}-", "group: release-"},
		{"a group shared by the events", "group: release-${{ github.event_name }}-${{ inputs.tag || github.ref_name }}", "group: release"},
		{"cancel in progress", "cancel-in-progress: false", "cancel-in-progress: true"},
	} {
		require.Equal(t, 1, strings.Count(text, c.old), c.name)
		assert.NotEmpty(t, releaseKeyProblems(strings.Replace(text, c.old, c.repl, 1)), c.name)
	}
}

// TestReleaseTextRules asserts what the text scan accepts and refuses.
func TestReleaseTextRules(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseWorkflowPath)
	for _, c := range []struct{ name, old, repl string }{
		{"continue-on-error", "    timeout-minutes: 40\n", "    timeout-minutes: 40\n    continue-on-error: true\n"},
		{"always", "    if: github.event_name == 'push'\n", "    if: always()\n"},
		{"failure", "    if: github.event_name == 'push'\n", "    if: failure()\n"},
		{"cancelled", "    if: github.event_name == 'push'\n", "    if: cancelled() || github.event_name == 'push'\n"},
		{"secrets context", "          TAG: ${{ inputs.tag }}\n", "          TAG: ${{ secrets.TOKEN }}\n"},
		{"an expression in a run text", "        run: scripts/release-upsert dist/*\n", "        run: scripts/release-upsert dist/* ${{ github.ref_name }}\n"},
	} {
		require.Positive(t, strings.Count(text, c.old), c.name)
		assert.NotEmpty(t, releaseTextProblems(strings.Replace(text, c.old, c.repl, 1)), c.name)
	}
	assert.Empty(t, releaseTextProblems("# always() and continue-on-error in a comment\njobs:\n  a:\n    steps:\n      - run: echo ok\n"))
}

// refusalRefs are the values of GITHUB_REF a run of the release workflow can
// have, with GITHUB_REF_NAME as the runner sets it. Only the branch main is
// accepted; a tag named main has the ref name main and is refused.
var refusalRefs = []struct {
	name, ref, refName string
	unset              bool
	accepted           bool
}{
	{name: "main", ref: "refs/heads/main", refName: "main", accepted: true},
	{name: "another branch", ref: "refs/heads/x", refName: "x"},
	{name: "a tag named main", ref: "refs/tags/main", refName: "main"},
	{name: "a branch that starts with main", ref: "refs/heads/main2", refName: "main2"},
	{name: "a branch below main", ref: "refs/heads/main/x", refName: "main/x"},
	{name: "a release tag", ref: "refs/tags/v0.1.0", refName: "v0.1.0"},
	{name: "a pull request", ref: "refs/pull/1/merge", refName: "1/merge"},
	{name: "an empty ref", ref: "", refName: ""},
	{name: "no ref", unset: true},
}

// refusalProblems runs the text of the refusal step with sh for each value of
// GITHUB_REF, reading the ref only from the environment: main passes, every
// other value stops with a non-zero exit and one plain line on standard
// error, and nothing else is printed.
func refusalProblems(t *testing.T, sh, run string) []string {
	t.Helper()
	var out []string
	for _, c := range refusalRefs {
		cmd := exec.CommandContext(t.Context(), sh, "-c", run)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		if !c.unset {
			cmd.Env = append(cmd.Env, "GITHUB_REF="+c.ref, "GITHUB_REF_NAME="+c.refName)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		exit := 0
		if err := cmd.Run(); err != nil {
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			if !ok {
				return append(out, err.Error())
			}
			exit = exitErr.ExitCode()
		}
		line := strings.TrimSpace(stderr.String())
		switch {
		case c.accepted && (exit != 0 || stderr.Len() > 0):
			out = append(out, fmt.Sprintf("%s: exit %d, stderr %q, want exit 0 and no output", c.name, exit, stderr.String()))
		case !c.accepted && (exit == 0 || line == "" || strings.Contains(line, "\n") || stdout.Len() > 0):
			out = append(out, fmt.Sprintf("%s: exit %d, stderr %q, stdout %q, want a non-zero exit and one line on stderr", c.name, exit, stderr.String(), stdout.String()))
		}
	}
	return out
}

// TestRehearseRefusesEveryRefButMain asserts the first step of the rehearse
// job, run with sh, lets only refs/heads/main through and says why in one
// plain line otherwise, and that the check fails for a step that drops the
// refusal, accepts a prefix or a glob of main, reads the ref name, reads the
// ref from an expression, or fails without a message. The refusal guards
// against a dispatch from the wrong ref; it is not a security boundary.
func TestRehearseRefusesEveryRefButMain(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	const sh = "/bin/sh"
	wf := readGateWorkflow(t, releaseWorkflowPath)
	steps := wf.Jobs["rehearse"].Steps
	require.NotEmpty(t, steps)
	run := steps[0].Run
	assert.Empty(t, refusalProblems(t, sh, run))

	const refusal = `echo "error: a rehearsal runs from main only" >&2; exit 1`
	for _, c := range []struct{ name, run string }{
		{"no refusal", "exit 0"},
		{"a glob of main", `case "$GITHUB_REF" in refs/heads/main*) ;; *) ` + refusal + ` ;; esac`},
		{"every branch", `case "$GITHUB_REF" in refs/heads/*) ;; *) ` + refusal + ` ;; esac`},
		{"the ref name", `[ "$GITHUB_REF_NAME" = main ] || { ` + refusal + `; }`},
		{"the ref from an expression", `[ "${{ github.ref }}" = refs/heads/main ] || { ` + refusal + `; }`},
		{"a refusal with no message", `[ "$GITHUB_REF" = refs/heads/main ] || exit 1`},
		{"a refusal with two lines", `[ "$GITHUB_REF" = refs/heads/main ] || { echo "error" >&2; echo "main only" >&2; exit 1; }`},
		{"a refusal that exits zero", `[ "$GITHUB_REF" = refs/heads/main ] || { echo "error: main only" >&2; exit 0; }`},
		{"a message on standard output", `[ "$GITHUB_REF" = refs/heads/main ] || { echo "error: main only"; exit 1; }`},
		{"a refusal of main", `[ "$GITHUB_REF" != refs/heads/main ] || { ` + refusal + `; }`},
	} {
		assert.NotEmpty(t, refusalProblems(t, sh, c.run), c.name)
	}
}

// releaseBuildScriptProblems returns what is wrong with the text of
// scripts/release-build: it stops on the first failure, changes to the
// repository root, checks the tag, exports LC_ALL=C and GOFLAGS=-buildvcs=false
// before it builds or writes the sums, and removes dist before it builds,
// so that a build on a tag and a build on main give the same files in the
// same order.
func releaseBuildScriptProblems(text string) []string {
	code := shellCodeOf(text)
	index := func(re string) int {
		r := regexp.MustCompile(re)
		return slices.IndexFunc(code, func(l string) bool { return r.MatchString(strings.TrimSpace(l)) })
	}
	var out []string
	pos := map[string]int{
		"set":     index(`^set -[a-z]*e[a-z]*( |$)`),
		"cd":      index(`^cd "\$\(dirname "\$0"\)/\.\."$`),
		"check":   index(`^\./scripts/check-release-tag "\$TAG"$`),
		"lcall":   index(`^export LC_ALL=C$`),
		"goflags": index(`^export GOFLAGS=-buildvcs=false$`),
		"clean":   index(`^rm -rf dist$`),
		"loop":    index(`^for platform in .*; do$`),
		"build":   slices.IndexFunc(code, buildCallRE.MatchString),
		"sums":    slices.IndexFunc(code, func(l string) bool { return strings.Contains(l, "sha256sum") }),
	}
	for _, name := range []string{"set", "cd", "check", "lcall", "goflags", "clean", "loop", "build", "sums"} {
		if pos[name] < 0 {
			out = append(out, "scripts/release-build has no "+name+" line")
		}
	}
	if len(out) > 0 {
		return out
	}
	before := func(first, second string) {
		if pos[first] > pos[second] {
			out = append(out, fmt.Sprintf("the %s line comes after the %s line", first, second))
		}
	}
	before("set", "cd")
	before("cd", "clean")
	before("check", "clean")
	before("goflags", "build")
	before("lcall", "sums")
	before("clean", "loop")
	before("loop", "build")
	before("build", "sums")
	return out
}

// TestReleaseBuildScriptGivesTheSameBytes asserts scripts/release-build keeps
// the settings that make the build of a tag and the build of main equal, and
// what the check refuses.
func TestReleaseBuildScriptGivesTheSameBytes(t *testing.T) {
	t.Parallel()
	text := readText(t, releaseBuildScript)
	assert.Empty(t, releaseBuildScriptProblems(text))

	for _, c := range []struct{ name, old, repl string }{
		{"no buildvcs flag", "export GOFLAGS=-buildvcs=false\n", ""},
		{"the buildvcs flag on the build only", "export GOFLAGS=-buildvcs=false\n", ""},
		{"no locale", "export LC_ALL=C\n", ""},
		{"the locale on the sums command only", "export LC_ALL=C\n", ""},
		{"no clean dist", "rm -rf dist\n", ""},
		{"no set -e", "set -euo pipefail\n", ""},
		{"no tag check", "./scripts/check-release-tag \"$TAG\"\n", ""},
		{"the clean after the loop", "rm -rf dist\nmkdir -p dist\n", "mkdir -p dist\n"},
	} {
		require.Equal(t, 1, strings.Count(text, c.old), c.name)
		changed := strings.Replace(text, c.old, c.repl, 1)
		switch c.name {
		case "the buildvcs flag on the build only":
			changed = strings.Replace(changed, `VERSION="$TAG" ./scripts/build`, `GOFLAGS=-buildvcs=false VERSION="$TAG" ./scripts/build`, 1)
		case "the locale on the sums command only":
			changed = strings.Replace(changed, "(cd dist && sha256sum", "(cd dist && LC_ALL=C sha256sum", 1)
		case "the clean after the loop":
			changed = strings.Replace(changed, "(cd dist && sha256sum", "rm -rf dist\n(cd dist && sha256sum", 1)
		}
		assert.NotEmpty(t, releaseBuildScriptProblems(changed), c.name)
	}
}
