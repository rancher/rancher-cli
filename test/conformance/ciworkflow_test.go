package conformance

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	ciWorkflowPath    = ".github/workflows/ci.yaml"
	setupToolsAction  = "$/.github/actions/setup-tools"
	setupToolsFile    = ".github/actions/setup-tools/action.yml"
	validateScript    = "scripts/validate"
	versionScript     = "scripts/golangci-lint-version"
	lintScript        = "scripts/lint"
	posixOnlyReason   = "runs bash scripts and relies on a POSIX PATH and exec bits"
	installPathPrefix = "go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"
)

// workflowFile holds the parts of a workflow file the checks below read.
type workflowFile struct {
	Env      yaml.Node              `yaml:"env"`
	Defaults yaml.Node              `yaml:"defaults"`
	Jobs     map[string]workflowJob `yaml:"jobs"`
}

// The continue-on-error fields are nodes so that an absent field (a zero
// node) differs from any value, true or an expression included.
type workflowJob struct {
	Name            string         `yaml:"name"`
	Uses            string         `yaml:"uses"`
	RunsOn          yaml.Node      `yaml:"runs-on"`
	Env             yaml.Node      `yaml:"env"`
	Defaults        yaml.Node      `yaml:"defaults"`
	Needs           yaml.Node      `yaml:"needs"`
	If              string         `yaml:"if"`
	ContinueOnError yaml.Node      `yaml:"continue-on-error"`
	TimeoutMinutes  int            `yaml:"timeout-minutes"`
	Strategy        yaml.Node      `yaml:"strategy"`
	Steps           []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	ID               string            `yaml:"id"`
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
	Env              yaml.Node         `yaml:"env"`
	Run              string            `yaml:"run"`
	Shell            string            `yaml:"shell"`
	WorkingDirectory string            `yaml:"working-directory"`
	If               string            `yaml:"if"`
	ContinueOnError  yaml.Node         `yaml:"continue-on-error"`
	With             map[string]string `yaml:"with"`
}

// needsNames lists the jobs a job names in needs, which is a string or a
// list of strings.
func (j workflowJob) needsNames() []string {
	switch j.Needs.Kind {
	case yaml.ScalarNode:
		return []string{j.Needs.Value}
	case yaml.SequenceNode:
		var names []string
		for _, n := range j.Needs.Content {
			names = append(names, n.Value)
		}
		return names
	}
	return nil
}

func readWorkflow(t *testing.T, path string) workflowFile {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var wf workflowFile
	require.NoError(t, yaml.Unmarshal(data, &wf), path)
	require.NotEmpty(t, wf.Jobs, "%s holds no jobs", path)
	return wf
}

// The rules for the run bodies of ci.yaml are an allowlist: the six part
// steps are exact, every other body must not mention validate or
// scripts/ci, and a make word may only be followed by the e2e target.
// A list of forbidden spellings would always miss one.
const (
	staticStepBody           = "./scripts/validate static"
	testsRace1StepBody       = "./scripts/validate tests-race-1"
	testsRace2StepBody       = "./scripts/validate tests-race-2"
	testsRace3StepBody       = "./scripts/validate tests-race-3"
	testsConformanceStepBody = "./scripts/validate tests-conformance"
	testsCacheModelStepBody  = "./scripts/validate tests-cachemodel"
)

// ciPartStepBodies lists the run body of every part step of ci.yaml.
var ciPartStepBodies = []string{
	staticStepBody,
	testsRace1StepBody, testsRace2StepBody, testsRace3StepBody,
	testsConformanceStepBody, testsCacheModelStepBody,
}

var (
	// envWriteRE matches a run body that writes the environment file or the
	// path file of later steps, or the file go env -w writes: each can change
	// what a later part step runs.
	envWriteRE     = regexp.MustCompile(`GITHUB_(?:ENV|PATH)|\bgo\s+env\b[^\n]*\s-w\b`)
	validateWordRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])(validate|scripts/(\./)*ci)([^A-Za-z0-9_]|$)`)
	makeWordRE     = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(g?make|\$MAKE|\$\{MAKE\}|\$\(MAKE\))([^A-Za-z0-9_-]|$)`)
	// e2e must be the only target: every script is a make target, so
	// "make e2e ci" would also run the whole gate.
	makeTargetRE = regexp.MustCompile(`^ +e2e *($|\n|;|&|\|)`)
)

// ciRunBodyProblems returns what is wrong with a run body of ci.yaml that
// is not one of the part steps. The allowed make targets are listed
// here: e2e only.
func ciRunBodyProblems(body string) []string {
	var problems []string
	if envWriteRE.MatchString(body) {
		problems = append(problems, "the body writes GITHUB_ENV or GITHUB_PATH or runs go env -w: no ci.yaml run body may change what a later step runs")
	}
	if validateWordRE.MatchString(body) {
		problems = append(problems, "the word validate or scripts/ci appears; only the part steps may run the gate")
	}
	for _, m := range makeWordRE.FindAllStringSubmatchIndex(body, -1) {
		if !makeTargetRE.MatchString(body[m[5]:]) {
			problems = append(problems, "make "+strings.TrimSpace(strings.SplitN(body[m[5]:], "\n", 2)[0])+": the allowed make targets are listed in ciRunBodyProblems")
		}
	}
	return problems
}

// TestCIRunBodyRules asserts what the run body rules of ci.yaml accept
// and refuse.
func TestCIRunBodyRules(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"make", "make ci", "make validate", `bash -o pipefail -c "make validate"`,
		"timeout 600 make validate", "sudo make validate", `eval "make validate"`,
		"make 2>&1", "make -C . e2e", "make -j4 e2e", "gmake e2e2", "$MAKE validate",
		"${MAKE} ci", "$(MAKE) ci", "/usr/bin/make ci", "make e2e && make",
		"cat <<EOF\nmake validate\nEOF", `echo "C:\\" ; make validate`,
		"./scripts/validate static || true", "./scripts/validate", "bash scripts/ci",
		"make\nvalidate", "echo x; make\n", "make e2e ci", "make e2e all",
		"make e2e \\\nci", "bash scripts/./ci", "# make validation",
		`echo "GOFLAGS=-run=^$" >> "$GITHUB_ENV"`, "echo /x >> $GITHUB_PATH", "go env -w GOFLAGS=-run=^$", "go   env -w GOFLAGS=x", "go env GOPATH && go env -w GOFLAGS=x",
	} {
		assert.NotEmpty(t, ciRunBodyProblems(body), "%q must be refused", body)
	}
	for _, body := range []string{
		"make e2e", "make e2e\n", "make   e2e", "bash scripts/e2e", "go test ./...",
		"sh -n install.sh", "echo done", "echo invalidate", "echo make-it", "make e2e && echo ok",
		"go build ./...",
	} {
		assert.Empty(t, ciRunBodyProblems(body), "%q must be accepted", body)
	}
}

// isCacheSaveUses reports whether a uses value is the cache save action,
// in any letter case and with any ref.
func isCacheSaveUses(uses string) bool {
	name, _, _ := strings.Cut(strings.ToLower(uses), "@")
	return name == "actions/cache/save"
}

// completionStepBody is the exact body of the step of the third race shard
// job that runs the completion script tests under every shell, with the race
// detector. The race shards leave that package out, so this step is its race
// run.
const completionStepBody = `log="${RUNNER_TEMP}/completions.log"
if ! go test -race -count=1 -v ./internal/cli/completions/ > "$log" 2>&1; then
  cat "$log"
  exit 1
fi
grep -E '^(--- |ok)' "$log"
echo "shells used:"
grep -ho 'shell [a-z]*: .*' "$log" | sort | uniq -c
status=0
for shell in bash zsh fish pwsh; do
  if ! grep -q "shell ${shell}: " "$log"; then
    echo "no completion test used ${shell}" >&2
    status=1
  fi
done
if grep -E '^\s*--- SKIP' "$log"; then
  echo "a completion shell test skipped" >&2
  status=1
fi
exit "$status"
`

// readRawSteps decodes the steps of every job of a workflow as plain maps,
// so that a step is compared whole, with every key it has.
func readRawSteps(t *testing.T, path string) map[string][]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var wf struct {
		Jobs map[string]struct {
			Steps []map[string]any `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &wf), path)
	out := map[string][]map[string]any{}
	for id, job := range wf.Jobs {
		out[id] = job.Steps
	}
	return out
}

// readRawJobs decodes every job of a workflow as a plain map, so that a job
// is checked with every key it has.
func readRawJobs(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var wf struct {
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &wf), path)
	return wf.Jobs
}

// partJobKeyProblems returns what is wrong with the keys of a part job: it
// has exactly runs-on, timeout-minutes and steps, and runs on ubuntu-latest.
// Any other key (container, services, env, defaults, strategy, a runner
// list) can change the environment the steps run in.
func partJobKeyProblems(jobID string, job map[string]any) []string {
	var out []string
	if keys := slices.Sorted(maps.Keys(job)); !slices.Equal(keys, []string{"runs-on", "steps", "timeout-minutes"}) {
		out = append(out, fmt.Sprintf("job %s has the keys %v and must have exactly runs-on, timeout-minutes and steps", jobID, keys))
	}
	if runsOn := job["runs-on"]; runsOn != "ubuntu-latest" {
		out = append(out, fmt.Sprintf("job %s runs on %v and must run on ubuntu-latest", jobID, runsOn))
	}
	return out
}

// completionStep is the whole completion shell tests step.
var completionStep = map[string]any{"name": "Completion shell tests", "run": completionStepBody}

// completionStepCount returns how many of the steps are the completion shell
// tests step, matched whole.
func completionStepCount(steps []map[string]any) int {
	n := 0
	for _, step := range steps {
		if reflect.DeepEqual(step, completionStep) {
			n++
		}
	}
	return n
}

// allowedPartSteps lists every step a part job may have, each as the whole
// step: checkout, setup-go, go-cache and the cache save with the commits
// recorded in cacheUsesPins, the tools install the part needs (the shells
// only for the tests-race-3 part, which also has the completion shell tests
// step), and the part step.
// The part jobs goCacheReaders lists (test-2, test-3, cachemodel) have
// go-cache with restore-from in place of go-cache without it, and no cache
// save. Any other step in a part job can change what the part runs.
func allowedPartSteps(jobID, part string) []map[string]any {
	pinned := func(name string) string { return name + "@" + cacheUsesPins[name] }
	steps := []map[string]any{
		{"uses": pinned("actions/checkout"), "with": map[string]any{"persist-credentials": false}},
		{"uses": pinned("actions/setup-go"), "with": map[string]any{"go-version-file": "go.mod", "cache": false}},
	}
	if reader, ok := goCacheReaders[jobID]; ok {
		steps = append(steps, map[string]any{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": jobID, "restore-from": reader.source}})
	} else {
		steps = append(steps,
			map[string]any{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": jobID}},
			map[string]any{"uses": pinned("actions/cache/save"), "if": expectedSaveIf(jobID, "go-cache", "cache-hit"), "with": map[string]any{
				"path": goCacheOutputExpr("go-cache", "path"),
				"key":  goCacheOutputExpr("go-cache", "key"),
			}})
	}
	steps = append(steps, map[string]any{"run": part})
	switch part {
	case staticStepBody:
		steps = append(steps, map[string]any{"uses": setupToolsAction, "with": map[string]any{"shells": "false"}})
	case testsRace1StepBody, testsRace2StepBody:
		steps = append(steps,
			map[string]any{"uses": setupToolsAction, "with": map[string]any{"lint-tools": "false", "shells": "false"}})
	case testsRace3StepBody:
		steps = append(steps,
			map[string]any{"uses": setupToolsAction, "with": map[string]any{"lint-tools": "false"}},
			completionStep)
	}
	return steps
}

// partStepProblems returns the steps of a part job that are none of the
// allowed steps of its part, and a missing or repeated completion shell tests
// step in the job of the tests-race-3 part.
func partStepProblems(jobID, part string, steps []map[string]any) []string {
	var out []string
	if n := completionStepCount(steps); part == testsRace3StepBody && n != 1 {
		out = append(out, fmt.Sprintf("job %s has the completion shell tests step %d times and must have it exactly once, the race shards leave that package out", jobID, n))
	}
	allowed := allowedPartSteps(jobID, part)
	for i, step := range steps {
		if !slices.ContainsFunc(allowed, func(a map[string]any) bool { return reflect.DeepEqual(a, step) }) {
			out = append(out, fmt.Sprintf("job %s step %d is none of the steps a part job may have (checkout, setup-go, go-cache with restore-from and no cache save in a reader job or go-cache and the cache save in any other, setup-tools, the completion shell tests step and the part step, each exactly as listed in allowedPartSteps): %v", jobID, i+1, step))
		}
	}
	return out
}

// TestCIRunsEveryValidatePartOnce asserts that ci.yaml runs the static,
// tests-race-1, tests-race-2, tests-race-3, tests-conformance and
// tests-cachemodel parts of scripts/validate, each as the whole run body of
// exactly one step, in six separate jobs that do not name each other in
// needs; that no other run body mentions validate or scripts/ci or runs make
// with a target but e2e (TestCIRunBodyRules); that no job or step can be
// turned off, except that a cache save step may carry an if
// (TestOnlyMainPushesSaveCaches checks its expression); that the workflow,
// the six part jobs and the part steps set no env, which could select no
// test (GOFLAGS=-run=^$) and still pass; that the workflow and the six part
// jobs set no defaults, which could change the shell that runs a part; that
// no other run body writes the environment file, the path file or the go env
// file; that every step of the six part jobs is one of the listed steps,
// each matched whole (the completion shell tests step exactly once, in the
// job of tests-race-3, and in no other part job; go-cache with restore-from
// and no cache save in test-2, test-3 and cachemodel, which restore the entry
// of another job, go-cache without it and the cache save in the other part
// jobs); that each part job has exactly the keys runs-on (ubuntu-latest),
// timeout-minutes and steps; and that each job installs only the tools its
// part uses (the shells only in the job of tests-race-3, whose completion
// shell tests step is the only one that starts them in the race shards' jobs).
// TestSharedActionsHoldNoEnvWrites holds the two local actions the part jobs
// call (go-cache and setup-tools) to the same environment-write rule.
//
// The remaining limit: the steps and keys of the part jobs are listed and
// the two local actions are held to the environment-write rule, but what an
// earlier step or an action does outside that rule (a file it replaces, a
// program it puts on PATH, what a third-party action does inside itself) is
// left to review.
func TestCIRunsEveryValidatePartOnce(t *testing.T) {
	t.Parallel()
	wf := readWorkflow(t, ciWorkflowPath)

	assert.Zero(t, wf.Env.Kind, "ci.yaml must set no workflow-level env: an env can change what every part runs")
	assert.Zero(t, wf.Defaults.Kind, "ci.yaml must set no workflow-level defaults: a shell can turn a part into a no-op")
	partJobs := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(wf.Jobs)) {
		for i, step := range wf.Jobs[name].Steps {
			body := strings.TrimSpace(step.Run)
			if !slices.Contains(ciPartStepBodies, body) {
				assert.Empty(t, ciRunBodyProblems(step.Run), "job %s step %d: the body is not one of the allowed part bodies", name, i+1)
				continue
			}
			partJobs[body] = append(partJobs[body], name)
			assert.Empty(t, step.Shell, "job %s step %d must not change the shell", name, i+1)
			assert.Empty(t, step.WorkingDirectory, "job %s step %d must not change the directory", name, i+1)
			assert.Zero(t, step.Env.Kind, "job %s step %d must set no env: an env can change what the part runs", name, i+1)
		}
	}
	jobOf := map[string]string{}
	for _, body := range ciPartStepBodies {
		assert.Len(t, partJobs[body], 1, "%q must be the run body of exactly one step (found %d)", body, len(partJobs[body]))
		require.Len(t, partJobs[body], 1)
		jobOf[body] = partJobs[body][0]
	}
	var jobNames []string
	for _, body := range ciPartStepBodies {
		jobNames = append(jobNames, jobOf[body])
	}
	require.Len(t, slices.Compact(slices.Sorted(slices.Values(jobNames))), len(ciPartStepBodies), "the parts must run in different jobs, got %q", jobNames)
	for _, a := range jobNames {
		for _, b := range jobNames {
			if a != b {
				assert.NotContains(t, wf.Jobs[a].needsNames(), b, "job %s must not wait for job %s", a, b)
			}
		}
	}

	for _, c := range []struct {
		part, needs string
		drops       []string
		setups      int
	}{
		{staticStepBody, "lint-tools", []string{"shells"}, 1},
		{testsRace1StepBody, "", []string{"lint-tools", "shells"}, 1},
		{testsRace2StepBody, "", []string{"lint-tools", "shells"}, 1},
		{testsRace3StepBody, "shells", []string{"lint-tools"}, 1},
		{testsConformanceStepBody, "", nil, 0},
		{testsCacheModelStepBody, "", nil, 0},
	} {
		name := jobOf[c.part]
		job := wf.Jobs[name]
		assert.Empty(t, job.If, "job %s must not be conditional", name)
		assert.Zero(t, job.Env.Kind, "job %s must set no env: an env can change what the part runs", name)
		assert.Zero(t, job.Defaults.Kind, "job %s must set no defaults: a shell can turn the part into a no-op", name)
		assert.Zero(t, job.ContinueOnError.Kind, "job %s must not set continue-on-error", name)
		setups := 0
		for i, step := range job.Steps {
			if !isCacheSaveUses(step.Uses) {
				assert.Empty(t, step.If, "job %s step %d must not be conditional", name, i+1)
			}
			assert.Zero(t, step.ContinueOnError.Kind, "job %s step %d must not set continue-on-error", name, i+1)
			if step.Uses != setupToolsAction {
				continue
			}
			setups++
			if v, ok := step.With[c.needs]; c.needs != "" && ok {
				assert.Equal(t, "true", v, "job %s runs the %s part and needs %s installed", name, c.part, c.needs)
			}
			for _, drop := range c.drops {
				assert.Equal(t, "false", step.With[drop], "job %s does not use %s and must not install it", name, drop)
			}
		}
		assert.Equal(t, c.setups, setups, "job %s must use %s %d times for the %s part", name, setupToolsAction, c.setups, c.part)
	}

	rawSteps := readRawSteps(t, ciWorkflowPath)
	rawJobs := readRawJobs(t, ciWorkflowPath)
	for _, body := range ciPartStepBodies {
		assert.Empty(t, partJobKeyProblems(jobOf[body], rawJobs[jobOf[body]]))
		assert.Empty(t, partStepProblems(jobOf[body], body, rawSteps[jobOf[body]]))
		assert.Equal(t, 30, wf.Jobs[jobOf[body]].TimeoutMinutes, "job %s timeout-minutes", jobOf[body])
	}
	require.Contains(t, wf.Jobs, "cross-compile")
	assert.Equal(t, 20, wf.Jobs["cross-compile"].TimeoutMinutes, "job cross-compile timeout-minutes")
}

// freshPassStepBodies are the step bodies in the workflows that run the
// fresh pass of scripts/test: the ci.yaml conformance part and make
// validate, which the release workflow runs.
var freshPassStepBodies = []string{testsConformanceStepBody, "make validate"}

// TestFreshTimeoutFitsEveryJobThatRunsIt asserts that the timeout scripts/test
// gives the fresh pass is longer than go test's default of 10 minutes and at
// least 5 minutes shorter than the timeout-minutes of every job that runs the
// fresh pass: the ci.yaml conformance job, where a slow run then fails with
// go test's report of the running tests before the runner cancels the job,
// and the release workflow's validate job, where make validate runs other
// parts before and after the fresh pass, so the rule bounds only the fresh
// pass there.
func TestFreshTimeoutFitsEveryJobThatRunsIt(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	minutes := freshTimeoutOf(t, string(script))
	for _, path := range []string{ciWorkflowPath, ".github/workflows/release.yaml"} {
		assert.Empty(t, freshTimeoutProblems(minutes, readWorkflow(t, path)), path)
	}
}

// freshTimeoutProblems checks a fresh timeout of the given minutes against
// every job of the workflow with a step that runs the fresh pass; the
// workflow must have exactly one such job.
func freshTimeoutProblems(minutes int, wf workflowFile) []string {
	var out []string
	jobs := 0
	for id, job := range wf.Jobs {
		if !slices.ContainsFunc(job.Steps, func(s workflowStep) bool {
			return slices.Contains(freshPassStepBodies, strings.TrimSpace(s.Run))
		}) {
			continue
		}
		jobs++
		if minutes <= 10 {
			out = append(out, fmt.Sprintf("the fresh timeout of %dm is not longer than go test's default of 10m", minutes))
		}
		if limit := job.TimeoutMinutes - 5; minutes > limit {
			out = append(out, fmt.Sprintf("the fresh timeout of %dm is more than job %s's timeout-minutes %d minus 5", minutes, id, job.TimeoutMinutes))
		}
	}
	if jobs != 1 {
		out = append(out, fmt.Sprintf("%d jobs run one of %q, want 1", jobs, freshPassStepBodies))
	}
	return out
}

// TestFreshTimeoutProblemsRules asserts what freshTimeoutProblems accepts and
// refuses.
func TestFreshTimeoutProblemsRules(t *testing.T) {
	t.Parallel()
	wf := func(limits ...int) workflowFile {
		w := workflowFile{Jobs: map[string]workflowJob{}}
		for i, limit := range limits {
			w.Jobs["conformance"+strconv.Itoa(i)] = workflowJob{TimeoutMinutes: limit, Steps: []workflowStep{{Run: testsConformanceStepBody}}}
		}
		return w
	}
	for _, c := range []struct {
		name    string
		minutes int
		wf      workflowFile
		want    string
	}{
		{name: "fits", minutes: 25, wf: wf(30)},
		{name: "make validate in a job too short", minutes: 25, wf: workflowFile{Jobs: map[string]workflowJob{
			"validate": {TimeoutMinutes: 20, Steps: []workflowStep{{Run: "make validate"}}},
		}}, want: "minus 5"},
		{name: "too close to the job limit", minutes: 26, wf: wf(30), want: "minus 5"},
		{name: "the job limit itself", minutes: 30, wf: wf(30), want: "minus 5"},
		{name: "go test's default", minutes: 10, wf: wf(30), want: "not longer than go test's default"},
		{name: "no job runs the fresh pass", minutes: 25, wf: wf(), want: "0 jobs run"},
		{name: "two jobs run the fresh pass", minutes: 25, wf: wf(30, 30), want: "2 jobs run"},
	} {
		problems := freshTimeoutProblems(c.minutes, c.wf)
		if c.want == "" {
			assert.Empty(t, problems, c.name)
			continue
		}
		assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, c.want) }), "%s: %q", c.name, problems)
	}
}

// TestPartJobStepRules asserts what the step allowlist of the part jobs
// accepts and refuses: the listed steps, each whole, and nothing else.
func TestPartJobStepRules(t *testing.T) {
	t.Parallel()
	const job = "conformance"
	good := func() []map[string]any {
		a := allowedPartSteps(job, testsConformanceStepBody)
		return []map[string]any{a[0], a[1], a[2], a[4], a[3]}
	}
	assert.Empty(t, partStepProblems(job, testsConformanceStepBody, good()), "the listed steps")
	cacheModel := allowedPartSteps("cachemodel", testsCacheModelStepBody)
	assert.Empty(t, partStepProblems("cachemodel", testsCacheModelStepBody, cacheModel), "the steps of the tests-cachemodel part")
	for i, part := range []string{testsRace1StepBody, testsRace2StepBody} {
		id := "test-" + strconv.Itoa(i+1)
		assert.Empty(t, partStepProblems(id, part, allowedPartSteps(id, part)), "the steps of %s, with its tools step without the shells and no completion step", part)
	}
	race3 := allowedPartSteps("test-3", testsRace3StepBody)
	assert.Empty(t, partStepProblems("test-3", testsRace3StepBody, race3), "the steps of the tests-race-3 part, with its tools and completion steps")
	assert.NotEmpty(t, partStepProblems("test-3", testsRace3StepBody, slices.DeleteFunc(slices.Clone(race3), func(step map[string]any) bool {
		return reflect.DeepEqual(step, completionStep)
	})), "a tests-race-3 job without the completion step")
	assert.NotEmpty(t, partStepProblems("test-3", testsRace3StepBody, append(slices.Clone(race3), completionStep)), "a tests-race-3 job with the completion step twice")
	static := allowedPartSteps("lint", staticStepBody)
	assert.Empty(t, partStepProblems("lint", staticStepBody, static), "the steps of the static part")

	for _, id := range []string{"test-2", "test-3", "cachemodel"} {
		part := map[string]string{"test-2": testsRace2StepBody, "test-3": testsRace3StepBody, "cachemodel": testsCacheModelStepBody}[id]
		saver := allowedPartSteps("conformance", testsConformanceStepBody)
		plainGoCache := map[string]any{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": id}}
		save := maps.Clone(saver[3])
		save["if"] = expectedSaveIf(id, "go-cache", "cache-hit")
		for _, a := range allowedPartSteps(id, part) {
			uses, _ := a["uses"].(string)
			assert.False(t, isCacheSaveUses(uses), "%s is a reader: its allowed steps hold no cache save", id)
		}
		source := goCacheReaders[id].source
		assert.NotEmpty(t, partStepProblems(id, part, []map[string]any{plainGoCache}), "%s with a go-cache step that has no restore-from", id)
		assert.NotEmpty(t, partStepProblems(id, part, []map[string]any{save}), "%s with a cache save", id)
		assert.NotEmpty(t, partStepProblems(id, part, []map[string]any{
			{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": id, "restore-from": "lint"}},
		}), "%s with another source", id)
		assert.NotEmpty(t, partStepProblems(id, part, []map[string]any{
			{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": id, "restore-from": source, "setup-go-key": "true"}},
		}), "%s with setup-go-key", id)
	}
	assert.NotEmpty(t, partStepProblems(job, testsConformanceStepBody, []map[string]any{
		{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": job, "restore-from": "test-1"}},
	}), "a saver part job with restore-from")

	setupGoWith := func(key string, value any) map[string]any {
		steps := good()
		step := maps.Clone(steps[1])
		step[key] = value
		steps[1] = step
		return steps[1]
	}
	refused := map[string][]map[string]any{
		"a step that copies over a script":    append(good()[:3:3], map[string]any{"run": "cp /bin/true scripts/test"}, good()[3]),
		"a step that writes the go env file":  append(good()[:3:3], map[string]any{"run": `printf 'GOFLAGS=-run=^$\n' > "$(go env GOENV)"`}, good()[3]),
		"a setup-go with an env":              {setupGoWith("env", map[string]any{"A": "b"})},
		"a setup-go with an if":               {setupGoWith("if", "always()")},
		"a part step with a shell":            {{"run": testsConformanceStepBody, "shell": "bash"}},
		"a part step with an env":             {{"run": testsConformanceStepBody, "env": map[string]any{"GOFLAGS": "-run=^$"}}},
		"a save whose if is another":          {{"uses": "actions/cache/save@" + cacheUsesPins["actions/cache/save"], "if": "always()", "with": map[string]any{"path": goCacheOutputExpr("go-cache", "path"), "key": goCacheOutputExpr("go-cache", "key")}}},
		"a go-cache with another name":        {{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": "other"}}},
		"a go-cache that passes setup-go-key": {{"id": "go-cache", "uses": goCacheAction, "with": map[string]any{"name": job, "setup-go-key": "true"}}},
		"a checkout at another commit":        {{"uses": "actions/checkout@" + cacheUsesPins["actions/setup-go"], "with": map[string]any{"persist-credentials": false}}},
	}
	for name, steps := range refused {
		assert.NotEmpty(t, partStepProblems(job, testsConformanceStepBody, steps), "must refuse: %s", name)
	}
	okJob := map[string]any{"runs-on": "ubuntu-latest", "timeout-minutes": 30, "steps": []any{}}
	assert.Empty(t, partJobKeyProblems(job, okJob), "a job with the three keys")
	for name, edit := range map[string]func(map[string]any){
		"a container with an env": func(j map[string]any) {
			j["container"] = map[string]any{"image": "ubuntu:24.04", "env": map[string]any{"GOFLAGS": "-run=^$"}}
		},
		"a bare container":  func(j map[string]any) { j["container"] = "ubuntu:24.04" },
		"a runner list":     func(j map[string]any) { j["runs-on"] = []any{"self-hosted", "linux"} },
		"another runner":    func(j map[string]any) { j["runs-on"] = "windows-latest" },
		"a services key":    func(j map[string]any) { j["services"] = map[string]any{} },
		"an env":            func(j map[string]any) { j["env"] = map[string]any{"A": "b"} },
		"defaults":          func(j map[string]any) { j["defaults"] = map[string]any{} },
		"a name":            func(j map[string]any) { j["name"] = "x" },
		"no timeout":        func(j map[string]any) { delete(j, "timeout-minutes") },
		"a strategy":        func(j map[string]any) { j["strategy"] = map[string]any{} },
		"an if":             func(j map[string]any) { j["if"] = "always()" },
		"continue-on-error": func(j map[string]any) { j["continue-on-error"] = true },
	} {
		j := maps.Clone(okJob)
		edit(j)
		assert.NotEmpty(t, partJobKeyProblems(job, j), "must refuse: %s", name)
	}
	assert.NotEmpty(t, partStepProblems("test-3", testsRace3StepBody, []map[string]any{{"name": "Completion shell tests", "run": completionStepBody + "true\n"}}), "a completion step with another body")
	assert.NotEmpty(t, partStepProblems("test-3", testsRace3StepBody, []map[string]any{{"name": "Completion shell tests", "run": strings.Replace(completionStepBody, " -race", "", 1)}}), "a completion step without -race")
	assert.Empty(t, partStepProblems("test-3", testsRace3StepBody, []map[string]any{completionStep}), "the completion step in the job of tests-race-3")
	for i, part := range []string{testsRace1StepBody, testsRace2StepBody, testsConformanceStepBody, testsCacheModelStepBody} {
		assert.NotEmpty(t, partStepProblems("job", part, []map[string]any{completionStep}), "the completion step runs only in the job of tests-race-3, not for %s (case %d)", part, i)
	}
	toolsWith := func(with map[string]any) []map[string]any {
		return []map[string]any{{"uses": setupToolsAction, "with": with}}
	}
	for _, part := range []string{testsRace1StepBody, testsRace2StepBody} {
		assert.Empty(t, partStepProblems("job", part, toolsWith(map[string]any{"lint-tools": "false", "shells": "false"})), "tools without the lint tools and the shells")
		assert.NotEmpty(t, partStepProblems("job", part, toolsWith(map[string]any{"lint-tools": "false"})), "tools with the shells, the default")
		assert.NotEmpty(t, partStepProblems("job", part, toolsWith(map[string]any{"lint-tools": "false", "shells": "true"})), "tools with the shells set to true")
	}
	assert.Empty(t, partStepProblems("job", testsRace3StepBody, append(toolsWith(map[string]any{"lint-tools": "false"}), completionStep)), "tools with the shells and the completion step")
	assert.NotEmpty(t, partStepProblems("job", testsRace3StepBody, append(toolsWith(map[string]any{"lint-tools": "false", "shells": "false"}), completionStep)), "tools without the shells in the job of tests-race-3")
	assert.NotEmpty(t, partStepProblems(job, testsConformanceStepBody, []map[string]any{{"uses": setupToolsAction, "with": map[string]any{"shells": "false"}}}), "tools the conformance part does not use")
}

// goPathStepBody is the exact body of the one step of setup-tools that may
// write the path file.
const goPathStepBody = `echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`

// sharedActionEnvProblems returns the run bodies of a local action that
// write the environment file, the path file or the go env file, except the
// step of setup-tools that puts GOPATH/bin on PATH, matched by its name and
// exact body.
func sharedActionEnvProblems(actionPath string, steps []workflowStep) []string {
	var out []string
	for i, step := range steps {
		if !envWriteRE.MatchString(step.Run) {
			continue
		}
		if actionPath == setupToolsFile && step.Name == "Put GOPATH/bin on PATH" && strings.TrimSpace(step.Run) == goPathStepBody {
			continue
		}
		out = append(out, fmt.Sprintf("%s step %d: a run body of a local action the part jobs call must not write GITHUB_ENV or GITHUB_PATH or run go env -w", actionPath, i+1))
	}
	return out
}

// TestSharedActionsHoldNoEnvWrites asserts that no run body of go-cache or
// setup-tools writes the environment file, the path file or the go env file,
// except the setup-tools step that puts GOPATH/bin on PATH.
func TestSharedActionsHoldNoEnvWrites(t *testing.T) {
	t.Parallel()
	for _, path := range []string{goCacheActionFile, setupToolsFile} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var action struct {
			Runs struct {
				Steps []workflowStep `yaml:"steps"`
			} `yaml:"runs"`
		}
		require.NoError(t, yaml.Unmarshal(data, &action), path)
		require.NotEmpty(t, action.Runs.Steps, path)
		assert.Empty(t, sharedActionEnvProblems(path, action.Runs.Steps), path)
	}

	goPath := workflowStep{Name: "Put GOPATH/bin on PATH", Run: goPathStepBody}
	assert.Empty(t, sharedActionEnvProblems(setupToolsFile, []workflowStep{goPath, {Run: "go install x"}}), "the one allowed step")
	for name, c := range map[string]struct {
		path  string
		steps []workflowStep
	}{
		"a second path write in setup-tools":   {setupToolsFile, []workflowStep{goPath, {Run: `echo /x >> "$GITHUB_PATH"`}}},
		"an environment write in setup-tools":  {setupToolsFile, []workflowStep{{Run: `echo "GOFLAGS=-run=^$" >> "$GITHUB_ENV"`}}},
		"the path step with another body":      {setupToolsFile, []workflowStep{{Name: "Put GOPATH/bin on PATH", Run: goPathStepBody + ` && echo y >> "$GITHUB_ENV"`}}},
		"the path step under another name":     {setupToolsFile, []workflowStep{{Name: "Other", Run: goPathStepBody}}},
		"the path step in go-cache":            {goCacheActionFile, []workflowStep{goPath}},
		"go env -w in go-cache":                {goCacheActionFile, []workflowStep{{Run: "go env -w GOFLAGS=-run=^$"}}},
		"go env -w with extra spaces in tools": {setupToolsFile, []workflowStep{{Run: "go   env  -w GOFLAGS=x"}}},
	} {
		assert.NotEmpty(t, sharedActionEnvProblems(c.path, c.steps), "must refuse: %s", name)
	}
}

// TestSetupToolsInputs asserts that both inputs of the setup-tools action
// default to the string "true", that the step that puts GOPATH/bin on
// PATH is unconditional, and that no run body takes an input (a workflow
// expression in a run body is a script injection).
func TestSetupToolsInputs(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(setupToolsFile)
	require.NoError(t, err)
	var action struct {
		Inputs map[string]struct {
			Default yaml.Node `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	require.NoError(t, yaml.Unmarshal(data, &action))
	for _, name := range []string{"lint-tools", "shells"} {
		require.Contains(t, action.Inputs, name)
		def := action.Inputs[name].Default
		assert.Equal(t, "true", def.Value, "input %s default", name)
		assert.Equal(t, "!!str", def.ShortTag(), "input %s default must be the string \"true\"", name)
	}
	pathSteps := 0
	for _, step := range action.Runs.Steps {
		if strings.Contains(step.Run, "GITHUB_PATH") {
			pathSteps++
			assert.Empty(t, step.If, "the GITHUB_PATH step must run with every input combination")
		}
		assert.NotContains(t, step.Run, "${{ inputs", "step %q takes an input in its run body", step.Name)
	}
	assert.Equal(t, 1, pathSteps, "want one step that writes GITHUB_PATH")
}

// TestLintScriptChecksTheVersion asserts that scripts/lint runs the
// golangci-lint version check before golangci-lint run.
func TestLintScriptChecksTheVersion(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(lintScript)
	require.NoError(t, err)
	script := string(data)
	check := strings.Index(script, versionScript)
	run := strings.Index(script, "golangci-lint run")
	require.GreaterOrEqual(t, run, 0, "%s no longer runs golangci-lint", lintScript)
	require.GreaterOrEqual(t, check, 0, "%s must call %s", lintScript, versionScript)
	assert.Less(t, check, run, "%s must check the version before golangci-lint run", lintScript)
}

// pinRE finds the golangci-lint version the setup-tools action installs;
// the same expression scripts/golangci-lint-version uses.
var pinRE = regexp.MustCompile(`golangci-lint/v2/cmd/golangci-lint@(v[0-9]+\.[0-9]+\.[0-9]+)`)

// pinVersionRE finds a version number that is not part of a longer word
// or number (go1.27.1 is not one), with an optional v.
var pinVersionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9.])v?[0-9]+\.[0-9]+\.[0-9]+`)

// isPinCopy reports whether a line names golangci-lint and a version, in
// any spelling: an install line, an action input, an environment
// variable, a module requirement.
func isPinCopy(line string) bool {
	return strings.Contains(line, "golangci-lint") && pinVersionRE.MatchString(line)
}

// repositoryPin returns the pinned golangci-lint version, with its v.
func repositoryPin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(setupToolsFile)
	require.NoError(t, err)
	m := pinRE.FindSubmatch(data)
	require.NotNil(t, m, "no golangci-lint pin in %s", setupToolsFile)
	return string(m[1])
}

// TestPinCopyRule asserts which lines count as a copy of the pin. The
// sample versions are built from parts so this file holds no such line.
func TestPinCopyRule(t *testing.T) {
	t.Parallel()
	version := "2" + ".14" + ".0"
	goVersion := "go1" + ".27" + ".1"
	for _, line := range []string{
		installPathPrefix + "v" + version,
		"go install golangci-lint@v" + version,
		"version: v" + version + " # golangci-lint",
		"golangci-lint_version=" + version,
		"golangci-lint v" + version,
		"github.com/golangci/golangci-lint v" + version,
	} {
		assert.True(t, isPinCopy(line), "%q must count as a copy of the pin", line)
	}
	for _, line := range []string{
		"golangci-lint has version built with " + goVersion,
		"golangci-lint@latest",
		"golangci-lint run",
		"version: v" + version,
		"x" + version + " golangci-lint",
		"golangci-lint (v2 cmd) on 2026-09-24T10:54:53Z",
	} {
		assert.False(t, isPinCopy(line), "%q must not count as a copy of the pin", line)
	}
}

// TestGolangciLintPinHasOneCopy asserts that exactly one line in the
// tracked files names golangci-lint with a version, and that it is the
// install line in the setup-tools action. A second copy, a comment
// included, can drift from the install line.
func TestGolangciLintPinHasOneCopy(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	var hits []string
	for _, file := range files {
		info, err := os.Stat(file)
		require.NoError(t, err)
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for line := range strings.SplitSeq(string(data), "\n") {
			if isPinCopy(line) {
				hits = append(hits, file+":"+strings.TrimSpace(line))
			}
		}
	}
	require.Len(t, hits, 1, "want one line naming golangci-lint with a version, got %q", hits)
	assert.True(t, strings.HasPrefix(hits[0], setupToolsFile+":"), "the pin must live in %s, found %q", setupToolsFile, hits[0])
	assert.Contains(t, hits[0], installPathPrefix)
}

// exitCodeOf returns the exit code of a finished command, 0 for no error.
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	exit, ok := errors.AsType[*exec.ExitError](err)
	require.True(t, ok, "command did not run: %v", err)
	return exit.ExitCode()
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func copyRepositoryFile(t *testing.T, rel, destRoot string) {
	t.Helper()
	data, err := os.ReadFile(rel)
	require.NoError(t, err)
	writeTestFile(t, filepath.Join(destRoot, rel), string(data), 0o755)
}

// runScript runs path with args, with env added to the process
// environment, and returns its exit code and stderr.
func runScript(t *testing.T, dir, path string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return exitCodeOf(t, err), stderr.String()
}

// stubScript is a command that appends its name and arguments to the file
// named by STUB_LOG, exits 3 when that line is the value of STUB_FAIL,
// and otherwise runs body.
func stubScript(name, body string) string {
	return "#!/bin/bash\nline=\"" + name + " $*\"\nprintf '%s\\n' \"$line\" >> \"$STUB_LOG\"\n" +
		"if [ \"${STUB_FAIL:-}\" = \"$line\" ]; then exit 3; fi\n" + body + "\n"
}

// dispatchLayout builds a copy of the gate in a temp directory: the real
// scripts/validate and scripts/golangci-lint-version, the pin line in the
// setup-tools action, stubs for the scripts the gate calls, and a stub
// directory with every tool it runs. Every external command that
// scripts/validate runs needs a stub in bodies: the stub directory comes
// first on PATH, so a command without one runs for real in a directory
// that is not the repository, or is not found. It returns the layout root,
// the environment to run in and the log file the stubs write.
func dispatchLayout(t *testing.T) (root string, env []string, logPath string) {
	t.Helper()
	root = t.TempDir()
	const pin = "v1.2.3"
	copyRepositoryFile(t, validateScript, root)
	copyRepositoryFile(t, versionScript, root)
	writeTestFile(t, filepath.Join(root, setupToolsFile), "run: "+installPathPrefix+pin+"\n", 0o644)
	writeTestFile(t, filepath.Join(root, "install.sh"), "#!/bin/sh\n:\n", 0o755)
	writeTestFile(t, filepath.Join(root, "scripts/test"), stubScript("scripts/test", ""), 0o755)
	writeTestFile(t, filepath.Join(root, "scripts/build"), stubScript("scripts/build",
		"mkdir -p bin\ntouch bin/rancher bin/rancher-credential-vault\nchmod +x bin/rancher bin/rancher-credential-vault"), 0o755)

	stubs := filepath.Join(root, "stubs")
	bodies := map[string]string{
		"go":    `if [ "$1" = env ]; then echo linux; fi`,
		"git":   `if [ "$1" = --no-pager ]; then exit 1; fi`,
		"gofmt": "",
		"golangci-lint": `if [ "$1" = version ]; then
  echo "golangci-lint has version ${STUB_LINT_VERSION} built with go1.27.1 from 0000000 on 2026-01-01T00:00:00Z"
fi`,
		"shellcheck": "",
		"actionlint": "",
	}
	for name, body := range bodies {
		writeTestFile(t, filepath.Join(stubs, name), stubScript(name, body), 0o755)
	}
	logPath = filepath.Join(root, "stub.log")
	env = []string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_LOG=" + logPath,
		"STUB_LINT_VERSION=" + strings.TrimPrefix(pin, "v"),
	}
	return root, env, logPath
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

// TestValidateRunsThePartsInOrder asserts that scripts/validate with no
// argument runs the static part (vet, the shard check, the lint version
// check, lint) and then the tests part (the race pass, the passes without the
// race detector, then the build); that each part alone runs only its own
// commands, with the exact call each part makes: tests-race-1, -2 and -3 call
// the race pass with -shard=1/3, 2/3 and 3/3 (three different calls that
// together name every shard), tests-conformance the pass of the fresh
// packages then the build, tests-cachemodel the pass of the cached norace
// packages; that the shard check runs once, after go vet, in the static part
// only; that the tests part logs the tests-race log followed by the
// tests-norace log, line for line, and the no-argument run the static log
// followed by the tests log; that a failing command ends the run with a
// non-zero exit before any later command, except that a failing race pass
// still runs the passes without the race detector and then ends the part
// before the build; that each of the CI parts alone exits non-zero when its
// own test call fails, and tests-norace and tests-conformance then run no
// build; that a failing shard check fails the static part and the
// no-argument run before any test; and that any other arguments exit 2 with a
// usage line naming every part. Every comparison runs with CI set and empty.
func TestValidateRunsThePartsInOrder(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip(posixOnlyReason)
	}
	const (
		raceCall       = "scripts/test -vet=off -pass=race"
		noRaceCall     = "scripts/test -vet=off -pass=norace"
		freshCall      = "scripts/test -vet=off -pass=fresh"
		cacheModelCall = "scripts/test -vet=off -pass=norace-cached"
		shardCheckCall = "scripts/test -shard-check"
		buildCall      = "scripts/build"
		goOSCall       = "go env GOOS"
	)
	shardCall := func(i int) string { return raceCall + " -shard=" + strconv.Itoa(i) + "/3" }
	root, env, logPath := dispatchLayout(t)
	for _, ci := range []string{"CI=1", "CI="} {
		run := func(extraEnv []string, args ...string) (int, string, []string) {
			_ = os.Remove(logPath)
			full := append(append(slices.Clone(env), ci), extraEnv...)
			code, stderr := runScript(t, root, filepath.Join(root, validateScript), full, args...)
			return code, stderr, readLog(t, logPath)
		}
		index := func(log []string, line string) int {
			t.Helper()
			i := slices.Index(log, line)
			require.GreaterOrEqual(t, i, 0, "%s: %q is not in the log %q", ci, line, log)
			return i
		}
		hasPrefix := func(log []string, prefix string) bool {
			return slices.ContainsFunc(log, func(line string) bool { return strings.HasPrefix(line, prefix) })
		}

		code, stderr, all := run(nil)
		require.Zero(t, code, "%s: %s", ci, stderr)
		vet := index(all, "go vet ./...")
		shardCheck := index(all, shardCheckCall)
		version := index(all, "golangci-lint version")
		lint := index(all, "golangci-lint run")
		race := index(all, raceCall)
		noRace := index(all, noRaceCall)
		build := index(all, buildCall)
		assert.Less(t, vet, shardCheck, "%s: the shard check runs after go vet", ci)
		assert.Equal(t, 1, strings.Count(strings.Join(all, "\n"), shardCheckCall), "%s: the shard check runs once", ci)
		assert.Less(t, vet, lint, ci)
		assert.Less(t, version, lint, ci)
		assert.Less(t, lint, race, ci)
		assert.Less(t, shardCheck, race, ci)
		assert.Less(t, race, noRace, ci)
		assert.Less(t, noRace, build, ci)

		code, stderr, static := run(nil, "static")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Contains(t, static, "go vet ./...", ci)
		assert.Contains(t, static, "golangci-lint run", ci)
		assert.Equal(t, 1, strings.Count(strings.Join(static, "\n"), shardCheckCall), "%s: the static part runs the shard check once", ci)
		assert.Less(t, index(static, "go vet ./..."), index(static, shardCheckCall), "%s: the shard check runs after go vet", ci)
		assert.NotContains(t, static, raceCall, ci)
		assert.NotContains(t, static, noRaceCall, ci)
		assert.NotContains(t, static, buildCall, ci)

		code, stderr, raceLog := run(nil, "tests-race")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Equal(t, []string{raceCall}, raceLog, "%s: tests-race", ci)

		code, stderr, noRaceLog := run(nil, "tests-norace")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Equal(t, []string{noRaceCall, buildCall, goOSCall}, noRaceLog, "%s: tests-norace", ci)

		var shardCalls []string
		for i := 1; i <= 3; i++ {
			code, stderr, shardLog := run(nil, "tests-race-"+strconv.Itoa(i))
			require.Zero(t, code, "%s: %s", ci, stderr)
			assert.Equal(t, []string{shardCall(i)}, shardLog, "%s: tests-race-%d", ci, i)
			shardCalls = append(shardCalls, shardLog...)
		}
		assert.Len(t, slices.Compact(slices.Sorted(slices.Values(shardCalls))), 3, "%s: the three shards make three different calls", ci)
		for _, want := range []string{"-shard=1/3", "-shard=2/3", "-shard=3/3"} {
			assert.Equal(t, 1, strings.Count(strings.Join(shardCalls, "\n"), want), "%s: one shard calls %s", ci, want)
		}

		code, stderr, conformanceLog := run(nil, "tests-conformance")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Equal(t, []string{freshCall, buildCall, goOSCall}, conformanceLog, "%s: tests-conformance", ci)

		code, stderr, cacheModelLog := run(nil, "tests-cachemodel")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Equal(t, []string{cacheModelCall}, cacheModelLog, "%s: tests-cachemodel", ci)

		code, stderr, tests := run(nil, "tests")
		require.Zero(t, code, "%s: %s", ci, stderr)
		assert.Equal(t, slices.Concat(raceLog, noRaceLog), tests, "%s: tests must log tests-race then tests-norace", ci)
		assert.NotContains(t, tests, "go vet ./...", ci)
		assert.NotContains(t, tests, "golangci-lint run", ci)
		assert.NotContains(t, tests, shardCheckCall, ci)

		assert.Equal(t, slices.Concat(static, tests), all, "%s: no argument must log static then tests", ci)

		code, _, log := run([]string{"STUB_FAIL=go vet ./..."})
		assert.NotZero(t, code, "%s: a failing go vet must fail the run", ci)
		assert.Contains(t, log, "go vet ./...", ci)
		assert.False(t, hasPrefix(log, "scripts/test"), "%s: no test runs after a failing static part, got %q", ci, log)
		assert.False(t, hasPrefix(log, buildCall), "%s: no build after a failing static part, got %q", ci, log)

		for _, args := range [][]string{nil, {"static"}} {
			code, _, log = run([]string{"STUB_FAIL=" + shardCheckCall}, args...)
			assert.NotZero(t, code, "%s: a failing shard check must fail the run (arguments %q)", ci, args)
			assert.Contains(t, log, shardCheckCall, ci)
			assert.NotContains(t, log, raceCall, "%s: no test runs after a failing shard check (arguments %q)", ci, args)
			assert.NotContains(t, log, buildCall, "%s: no build after a failing shard check (arguments %q)", ci, args)
		}

		code, _, log = run([]string{"STUB_FAIL=" + raceCall}, "tests")
		assert.NotZero(t, code, "%s: a failing race pass must fail the part", ci)
		assert.Contains(t, log, raceCall, ci)
		assert.Contains(t, log, noRaceCall, "%s: the passes without the race detector still run", ci)
		assert.NotContains(t, log, buildCall, "%s: no build after failing tests", ci)

		code, _, log = run([]string{"STUB_FAIL=" + noRaceCall}, "tests")
		assert.NotZero(t, code, "%s: a failing pass without the race detector must fail the part", ci)
		assert.NotContains(t, log, buildCall, "%s: no build after failing tests", ci)

		code, _, log = run([]string{"STUB_FAIL=" + raceCall}, "tests-race")
		assert.NotZero(t, code, "%s: a failing race pass must fail the tests-race part", ci)
		assert.Equal(t, []string{raceCall}, log, "%s: tests-race", ci)

		code, _, log = run([]string{"STUB_FAIL=" + noRaceCall}, "tests-norace")
		assert.NotZero(t, code, "%s: a failing pass without the race detector must fail the tests-norace part", ci)
		assert.Equal(t, []string{noRaceCall}, log, "%s: tests-norace runs no build after a failing pass", ci)

		for i := 1; i <= 3; i++ {
			code, _, log = run([]string{"STUB_FAIL=" + shardCall(i)}, "tests-race-"+strconv.Itoa(i))
			assert.NotZero(t, code, "%s: a failing shard must fail the tests-race-%d part, a check ci.yaml reports", ci, i)
			assert.Equal(t, []string{shardCall(i)}, log, "%s: tests-race-%d", ci, i)
		}

		code, _, log = run([]string{"STUB_FAIL=" + freshCall}, "tests-conformance")
		assert.NotZero(t, code, "%s: a failing pass of the fresh packages must fail the tests-conformance part", ci)
		assert.Equal(t, []string{freshCall}, log, "%s: tests-conformance runs no build after a failing pass", ci)

		code, _, log = run([]string{"STUB_FAIL=" + cacheModelCall}, "tests-cachemodel")
		assert.NotZero(t, code, "%s: a failing cache model pass must fail the tests-cachemodel part", ci)
		assert.Equal(t, []string{cacheModelCall}, log, "%s: tests-cachemodel", ci)

		for _, args := range [][]string{{"bogus"}, {"static", "tests"}} {
			code, stderr, log = run(nil, args...)
			assert.Equal(t, 2, code, "%s: arguments %q", ci, args)
			for _, part := range []string{"static", "tests", "tests-race", "tests-norace", "tests-race-1", "tests-race-2", "tests-race-3", "tests-conformance", "tests-cachemodel"} {
				assert.Contains(t, stderr, part, "%s: arguments %q", ci, args)
			}
			assert.Empty(t, log, "%s: arguments %q must run nothing", ci, args)
		}
	}
}

// versionPath builds a PATH directory for scripts/golangci-lint-version
// that holds only dirname (the one external command the script runs) and,
// when versionOutput is not empty, a golangci-lint that prints it, so no
// other golangci-lint on the machine is visible. It returns the
// environment for the run and the directory.
func versionPath(t *testing.T, versionOutput string) (env []string, dir string) {
	t.Helper()
	bin := t.TempDir()
	dirname, err := exec.LookPath("dirname")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(dirname, filepath.Join(bin, "dirname")))
	if versionOutput != "" {
		writeTestFile(t, filepath.Join(bin, "golangci-lint"), "#!/bin/bash\nprintf '%s\\n' '"+versionOutput+"'\n", 0o755)
	}
	return []string{"PATH=" + bin}, bin
}

// TestGolangciLintVersionCheck asserts that scripts/golangci-lint-version
// accepts the installed version when it equals the pin, in both output
// shapes of golangci-lint version, refuses any other version (a
// pre-release of the pinned one included) naming both versions, the
// binary found and the install command, and passes when golangci-lint is
// not installed; and that it fails with one message when the pin or the
// installed version cannot be read.
func TestGolangciLintVersionCheck(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip(posixOnlyReason)
	}
	script, err := filepath.Abs(versionScript)
	require.NoError(t, err)
	pin := repositoryPin(t)
	bare := strings.TrimPrefix(pin, "v")
	releaseShape := func(v string) string {
		return "golangci-lint has version " + v + " built with go1.27.1 from 114493f on 2026-09-24T10:54:53Z"
	}
	installShape := func(v string) string {
		return "golangci-lint has version " + v + ` built with go1.27.1 from (unknown, modified: ?, mod sum: "h1:abc=") on (unknown)`
	}

	for name, output := range map[string]string{"release build": releaseShape(bare), "go install build": installShape(bare)} {
		env, _ := versionPath(t, output)
		code, stderr := runScript(t, "", script, env)
		assert.Zero(t, code, "%s: %s", name, stderr)
	}

	for name, other := range map[string]string{"another version": "0.0.1", "a pre-release of the pin": bare + "-rc1"} {
		env, dir := versionPath(t, releaseShape(other))
		code, stderr := runScript(t, "", script, env)
		assert.Equal(t, 1, code, name)
		assert.Contains(t, stderr, "error: "+dir+"/golangci-lint is golangci-lint "+other, name)
		assert.Contains(t, stderr, bare, name)
		assert.Contains(t, stderr, installPathPrefix+pin, name)
		assert.Contains(t, stderr, "first on PATH", name)
	}

	env, _ := versionPath(t, "golangci-lint has version "+bare+"\nbuilt with go1.27.1")
	code, stderr := runScript(t, "", script, env)
	assert.Zero(t, code, "a version at the end of a line: %s", stderr)

	env, _ = versionPath(t, "")
	code, stderr = runScript(t, "", script, env)
	assert.Zero(t, code, "no golangci-lint installed: %s", stderr)

	env, _ = versionPath(t, "something else entirely")
	code, stderr = runScript(t, "", script, env)
	assert.Equal(t, 1, code, "an output with no version")
	assert.Contains(t, stderr, "something else entirely")
	assert.Contains(t, stderr, setupToolsFile)
	assert.Contains(t, stderr, installPathPrefix+pin)

	for _, c := range []struct {
		name, action string
		hasFile      bool
	}{
		{"no pin file", "", false},
		{"no install line", "name: setup-tools\n", true},
		{"no version in line", "run: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest\n", true},
	} {
		root := t.TempDir()
		copyRepositoryFile(t, versionScript, root)
		if c.hasFile {
			writeTestFile(t, filepath.Join(root, setupToolsFile), c.action, 0o644)
		}
		env, _ := versionPath(t, releaseShape(bare))
		code, stderr := runScript(t, "", filepath.Join(root, versionScript), env)
		assert.Equal(t, 1, code, c.name)
		assert.Contains(t, stderr, setupToolsFile, c.name)
		assert.Equal(t, 1, strings.Count(stderr, "\n"), "%s: want one line on stderr, got %q", c.name, stderr)
	}

	out, err := exec.CommandContext(t.Context(), "git", "ls-files", "-s", "--", versionScript).Output()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "100755 "), "%s must be tracked with mode 100755, git has %q", versionScript, strings.TrimSpace(string(out)))
}
