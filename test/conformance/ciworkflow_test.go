package conformance

import (
	"bytes"
	"context"
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

	"github.com/rancher/rancher-cli/internal/testfs"
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
	lintCall          = "golangci-lint run --allow-serial-runners"
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
// step), the part step and the CPU model step (keys name and run only, the
// text of linuxCPUCommand, which only prints).
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
	return append(steps, map[string]any{"name": linuxCPUStepName, "run": linuxCPUCommand + "\n"})
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
			out = append(out, fmt.Sprintf("job %s step %d is none of the steps a part job may have (checkout, setup-go, go-cache with restore-from and no cache save in a reader job or go-cache and the cache save in any other, setup-tools, the completion shell tests step, the part step and the CPU model step, each exactly as listed in allowedPartSteps): %v", jobID, i+1, step))
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
// jobs, and the CPU model step, which only prints, in every part job but lint
// (TestEveryLinuxJobStartsByPrintingTheCPU checks where it stands));
// that each part job has exactly the keys runs-on (ubuntu-latest),
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

// racePassStepBodies are the step bodies in the workflows that run the race
// pass of scripts/test: the three ci.yaml race shards and make validate,
// which the release workflow runs.
var racePassStepBodies = []string{testsRace1StepBody, testsRace2StepBody, testsRace3StepBody, "make validate"}

// racePassJobCounts is the number of jobs per workflow file that run the
// race pass through scripts/test. A workflow file not listed has none.
var racePassJobCounts = map[string]int{"ci.yaml": 3, "release.yaml": 1}

var (
	// scriptTestWordRE matches a mention of scripts/test or scripts/ci, in any
	// spelling of the leading ./ and with repeated slashes.
	scriptTestWordRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])scripts/+(\./+)*(test|ci)([^A-Za-z0-9_-]|$)`)
	// scriptValidateWordRE finds each mention of scripts/validate; group 3
	// starts the rest of the command.
	scriptValidateWordRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])scripts/+(\./+)*validate([^A-Za-z0-9_-]|$)`)
	// shellRedirectRE matches a redirection, such as 2>&1 or >/dev/null.
	shellRedirectRE = regexp.MustCompile(`[0-9]*[<>]+&?[^\s;|&]*`)
	// raceWordRE matches the word validate, test or ci in any path form
	// (./test, scripts/validate, /x/ci).
	raceWordRE = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(validate|test|ci)([^A-Za-z0-9_-]|$)`)
	// goTestRE matches the go tool's own test command.
	goTestRE = regexp.MustCompile(`\bgo\s+test\b`)
	// commandSplitRE splits a run line into commands.
	commandSplitRE = regexp.MustCompile(`[;&|]+`)
	// shellAssignmentRE matches a variable assignment before a command.
	shellAssignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	// shellCommandFlagRE matches the -c flag of sh and bash, alone or among others.
	shellCommandFlagRE = regexp.MustCompile(`^-[A-Za-z]*c[A-Za-z]*$`)
	// changeDirRE matches a command that changes the working directory.
	changeDirRE = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(cd|pushd)([^A-Za-z0-9_-]|$)`)
)

// validateSafeParts are the only scripts/validate arguments that do not run
// the race pass. Any other argument, a variable or no argument included,
// counts as a possible race pass run.
var validateSafeParts = []string{"static", "tests-norace", "tests-conformance", "tests-cachemodel"}

// makeFlagsWithArgument are the make flags whose value is the next word.
var makeFlagsWithArgument = []string{"-C", "-f", "-I", "-o", "-W", "--directory", "--file", "--makefile", "--include-dir", "--old-file", "--what-if"}

// makeFlagsWithOptionalNumber are the make flags whose value is the next word
// only when that word is a number (make -j 4, make -j validate).
var makeFlagsWithOptionalNumber = []string{"-j", "-l", "--jobs", "--load-average", "--max-load"}

// makeSafeTarget reports whether a make target is a script that does not
// reach scripts/test: the e2e scripts, the Rancher API field comparison,
// build and lint. Every other target, a variable included, counts as a
// possible race pass run, because every file in scripts/ is a make target
// and ci, test and validate reach scripts/test.
func makeSafeTarget(target string) bool {
	return target == "e2e" || strings.HasPrefix(target, "e2e-") || strings.HasPrefix(target, "rancher-api-fields") ||
		target == "build" || target == "lint"
}

// commandArguments returns the words of the rest of a command: the redirects
// removed, up to the first newline, semicolon, pipe, ampersand or comment.
func commandArguments(rest string) []string {
	rest = shellRedirectRE.ReplaceAllString(rest, " ")
	if end := strings.IndexAny(rest, "\n;|&#"); end >= 0 {
		rest = rest[:end]
	}
	return strings.Fields(rest)
}

// validateRunsRacePass reports whether the body runs scripts/validate with
// anything other than one of validateSafeParts.
func validateRunsRacePass(body string) bool {
	body = strings.ReplaceAll(body, "\\\n", " ")
	for _, m := range scriptValidateWordRE.FindAllStringSubmatchIndex(body, -1) {
		args := commandArguments(body[m[6]:])
		if len(args) == 0 || !slices.Contains(validateSafeParts, strings.Trim(args[0], `"'`)) {
			return true
		}
	}
	return false
}

// makeRunsRacePass reports whether a make word of the body has no target
// (the default target is ci) or a target that makeSafeTarget does not list.
func makeRunsRacePass(body string) bool {
	body = strings.ReplaceAll(body, "\\\n", " ")
	for _, m := range makeWordRE.FindAllStringSubmatchIndex(body, -1) {
		var targets []string
		words := commandArguments(body[m[5]:])
		for i := 0; i < len(words); i++ {
			word := strings.Trim(words[i], `"'`)
			switch {
			case slices.Contains(makeFlagsWithArgument, word):
				i++
			case slices.Contains(makeFlagsWithOptionalNumber, word):
				if i+1 < len(words) {
					if _, err := strconv.ParseFloat(words[i+1], 64); err == nil {
						i++
					}
				}
			case strings.HasPrefix(word, "-"), strings.Contains(word, "="), word == "":
			default:
				targets = append(targets, word)
			}
		}
		if len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool { return !makeSafeTarget(target) }) {
			return true
		}
	}
	return false
}

// variableCommandAllowlist lists, as the trimmed run line, the lines whose
// command word is a variable, a command substitution or an expression that
// variableCommand would refuse, with the reason each cannot run the race
// pass.
var variableCommandAllowlist = map[string]string{
	`"$(go env GOPATH)/bin/golangci-lint" version`: "runs the linter binary the action installed",
}

// commandWrappers are the words that run the next word as a command.
var commandWrappers = []string{"sudo", "env", "command", "exec", "nice", "time", "xargs", "if", "then", "do", "else", "elif", "while", "until", "!"}

// variableCommand reports whether a line of the body has a command word that
// is a variable expansion, a command substitution or a ${{ }} expression
// ($X/test, ${X}, "$(x)", ${{ matrix.cmd }}), or runs sh or bash with -c and
// an argument that holds a $. What such a command runs cannot be read from
// the text, so it counts as a possible race pass run.
func variableCommand(body string) bool {
	body = strings.ReplaceAll(body, "\\\n", " ")
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if _, ok := variableCommandAllowlist[line]; ok {
			continue
		}
		for _, command := range commandSplitRE.Split(line, -1) {
			words := strings.Fields(command)
			for len(words) > 0 {
				word := strings.Trim(words[0], "({\"'")
				if shellAssignmentRE.MatchString(word) || slices.Contains(commandWrappers, word) {
					words = words[1:]
					continue
				}
				break
			}
			if len(words) == 0 {
				continue
			}
			first := strings.Trim(words[0], "({\"'")
			if strings.HasPrefix(first, "$") {
				return true
			}
			if slices.Contains([]string{"sh", "bash", "zsh", "dash"}, first) &&
				slices.ContainsFunc(words[1:], shellCommandFlagRE.MatchString) &&
				strings.Contains(strings.Join(words[1:], " "), "$") {
				return true
			}
		}
	}
	return false
}

// runsRacePass reports whether a workflow run body can run the race pass of
// scripts/test: it mentions scripts/test or scripts/ci, runs scripts/validate
// with anything but a part that does not run it, or runs make with no target
// or a target that is not known to be safe.
func runsRacePass(body string) bool {
	return scriptTestWordRE.MatchString(body) || validateRunsRacePass(body) || makeRunsRacePass(body)
}

// stepRunsRacePass reports whether a step can run the race pass: its run body
// or its shell override does (runsRacePass), or its run body has a command
// word that is a variable (variableCommand, except in a step whose shell is
// PowerShell, cmd or Python, where a leading $ is not a shell variable), or
// the step runs in another directory (a working-directory, or a cd or pushd
// in the body) and the body names validate, test or ci in any path form or a
// make word, which the directory can turn into scripts/validate,
// scripts/test or scripts/ci. The go tool's own go test is not counted. A
// workflow or job working-directory default reaches the step through
// withDefaultDirectories.
func stepRunsRacePass(step workflowStep) bool {
	if runsRacePass(step.Run) || runsRacePass(step.Shell) {
		return true
	}
	shell, _, _ := strings.Cut(strings.TrimSpace(step.Shell), " ")
	if !slices.Contains([]string{"pwsh", "powershell", "cmd", "python"}, shell) && variableCommand(step.Run) {
		return true
	}
	if step.WorkingDirectory == "" && !changeDirRE.MatchString(step.Run) {
		return false
	}
	body := goTestRE.ReplaceAllString(step.Run+"\n"+step.Shell, " ")
	return raceWordRE.MatchString(body) || makeWordRE.MatchString(body)
}

// defaultDirectory returns the run.working-directory of a defaults block, or
// the empty string.
func defaultDirectory(defaults yaml.Node) string {
	var d struct {
		Run struct {
			WorkingDirectory string `yaml:"working-directory"`
		} `yaml:"run"`
	}
	if defaults.Kind == 0 || defaults.Decode(&d) != nil {
		return ""
	}
	return d.Run.WorkingDirectory
}

// withDefaultDirectories returns the workflow with the working-directory
// default of the workflow, and the one of each job (which takes precedence),
// set on every step that sets none of its own, so that the step matcher sees
// that the step runs in another directory.
func withDefaultDirectories(wf workflowFile) workflowFile {
	workflowDir := defaultDirectory(wf.Defaults)
	jobs := map[string]workflowJob{}
	for id, job := range wf.Jobs {
		dir := workflowDir
		if jobDir := defaultDirectory(job.Defaults); jobDir != "" {
			dir = jobDir
		}
		job.Steps = slices.Clone(job.Steps)
		for i := range job.Steps {
			if job.Steps[i].WorkingDirectory == "" {
				job.Steps[i].WorkingDirectory = dir
			}
		}
		jobs[id] = job
	}
	wf.Jobs = jobs
	return wf
}

// TestRaceTimeoutMatcherRules asserts which steps the matcher for the race
// pass accepts and refuses. A step it cannot prove safe is refused: an
// unknown or variable scripts/validate argument or make target, and a step
// that runs in another directory.
func TestRaceTimeoutMatcherRules(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"make", "make ci", "make test", "make validate", "make -j4 validate", "make -C . validate",
		"make e2e ci", "make e2e validate", "$MAKE validate", "${MAKE} test", "$(MAKE) ci", "gmake validate",
		"/usr/bin/make validate", "make validate 2>&1", `bash -c "make validate"`, "sudo make test",
		"make e2e && make", "make e2e \\\nci", "cat <<EOF\nmake validate\nEOF", "make\n",
		"make -j 4", "make --jobs 4", "make --jobs=4", "make -j 4 validate", "make $TARGET", "make ${{ matrix.target }}",
		"make unknown", "make e2e unknown", "make -l 2.5",
		"./scripts/test", "scripts/test -pass=race", "bash scripts/./test", "./scripts/./ci", "bash scripts/ci",
		"./scripts/validate", "scripts/validate\n", "./scripts/validate tests", "./scripts/validate tests-race",
		"./scripts/validate tests-race-1", "./scripts/validate tests-race-3 && echo ok", "./scripts/./validate tests-race-2",
		"cat <<EOF\n./scripts/validate tests-race-1\nEOF",
		`./scripts/validate "$part"`, "./scripts/validate $PART", "./scripts/validate tests-race-${{ matrix.shard }}",
		"./scripts/validate ${{ matrix.part }}", "bash -c './scripts/validate'", "./scripts/validate tests-race-1 2>&1 | tee x",
		"./scripts/validate tests-race-1 >log", "for p in tests-race-1; do ./scripts/validate $p; done",
		"./scripts/validate static && ./scripts/validate tests-race-1", "./scripts/validate tests-bogus",
		"./scripts/validate \\\ntests-race-1",
		"./scripts//test", "./scripts//validate tests-race-1", "scripts/.//ci", "bash scripts///test", "scripts//./validate",
	} {
		assert.True(t, runsRacePass(body), "%q must be matched", body)
		assert.True(t, stepRunsRacePass(workflowStep{Run: body}), "%q must be matched as a step", body)
	}
	for _, body := range []string{
		"./scripts/validate static", "./scripts/validate tests-conformance", "./scripts/validate tests-cachemodel",
		"./scripts/validate tests-norace", "./scripts/validate static 2>&1 | tee x", "./scripts/validate static >log",
		"bash -c './scripts/validate static'", `./scripts/validate "static"`, "./scripts/validate \\\nstatic",
		"./scripts/validate static && ./scripts/validate tests-conformance",
		"make e2e", "make e2e\n", "make -j4 e2e", "make -j 4 e2e", "make --jobs 4 e2e", "make --jobs=4 e2e",
		"make --load-average 2 e2e", "make -j e2e", "make -C . e2e", "make build", "make lint",
		"make e2e-fidelity", "make e2e-fidelity-update", "make rancher-api-fields", "make rancher-api-fields-update",
		"make e2e && make e2e", "make e2e 2>&1", "scripts/fuzz", "./scripts/fuzz --selftest",
		"bash scripts/e2e", "go test ./...", "go test -race -count=1 ./internal/cli/completions/", "echo make-it",
		"echo done", "./scripts/check-release-tag \"$TAG\"", "scripts/release-upsert dist/*", "./scripts/build",
		"scripts/test-helper", "scripts/testdata", "./scripts/validate-x", "echo invalidate",
		"(cd dist && sha256sum -- * > SHA256SUMS)", "cd x && go test ./...",
	} {
		assert.False(t, runsRacePass(body), "%q must not be matched", body)
		assert.False(t, stepRunsRacePass(workflowStep{Run: body}), "%q must not be matched as a step", body)
	}

	for name, step := range map[string]workflowStep{
		"a variable path":                 {Run: "S=scripts; $S/test"},
		"a variable command":              {Run: "$CMD"},
		"a braced variable path":          {Run: "${S}/test"},
		"a quoted variable path":          {Run: `"$S"/test`},
		"an expression":                   {Run: "${{ matrix.cmd }}"},
		"an expression with arguments":    {Run: "${{ matrix.cmd }} tests-race-1"},
		"sh -c with a variable":           {Run: `sh -c "$CMD"`},
		"bash -c with a variable":         {Run: `bash -c "$CMD"`},
		"bash -c with an expression":      {Run: `bash -c "${{ matrix.cmd }}"`},
		"bash -ec with a variable":        {Run: `bash -ec "$CMD"`},
		"an assignment then a variable":   {Run: "FOO=1 $CMD"},
		"sudo then a variable":            {Run: "sudo $CMD"},
		"a variable after &&":             {Run: "echo x && $CMD"},
		"a variable after then":           {Run: "if true; then $X/test; fi"},
		"a variable on the second line":   {Run: "echo x\n$CMD arg"},
		"a command substitution":          {Run: "$(echo ./scripts/test)"},
		"a variable in a bash step":       {Shell: "bash", Run: "$CMD"},
		"a shell that runs scripts/test":  {Shell: "bash scripts/test {0}", Run: "echo hi"},
		"a shell that runs make validate": {Shell: "make validate --", Run: "echo hi"},
		"a shell with a path form":        {Shell: "./scripts//validate {0}", Run: "echo hi"},
		"cd then ./test":                  {Run: "cd scripts && ./test"},
		"cd then validate":                {Run: "cd scripts && ./validate tests-race-1"},
		"pushd then ./ci":                 {Run: "pushd scripts; ./ci; popd"},
		"working-directory ./test":        {WorkingDirectory: "scripts", Run: "./test"},
		"working-directory validate":      {WorkingDirectory: "scripts", Run: "./validate tests-race-1"},
		"working-directory make":          {WorkingDirectory: "sub", Run: "gmake"},
		"working-directory path form":     {WorkingDirectory: "scripts", Run: "bash ./validate"},
		"cd then an absolute path":        {Run: "cd /x && /x/test"},
		"cd in a subshell then ./test":    {Run: "(cd scripts; ./test)"},
	} {
		assert.True(t, stepRunsRacePass(step), "must be matched: %s", name)
	}
	for name, step := range map[string]workflowStep{
		"an echo of a variable":                 {Run: `echo "${shell}" $HOME`},
		"an assignment of a variable":           {Run: `status=1; log="${RUNNER_TEMP}/x.log"`},
		"a variable as an argument":             {Run: `cat "$log"`},
		"assignments before a command":          {Run: `GOOS="$goos" GOARCH="$goarch" ./scripts/build`},
		"bash -c with no variable":              {Run: "bash -c 'echo hi'"},
		"the allowlisted linter line":           {Run: `"$(go env GOPATH)/bin/golangci-lint" version`},
		"a PowerShell variable":                 {Shell: "pwsh", Run: "$ErrorActionPreference = 'Stop'\n$x = 1"},
		"a bash shell":                          {Shell: "bash", Run: "go install x"},
		"a bash shell with a flag":              {Shell: "bash -e {0}", Run: "go install x"},
		"working-directory with unrelated body": {WorkingDirectory: "dist", Run: "sha256sum -- *"},
		"working-directory with go test":        {WorkingDirectory: "x", Run: "go test ./..."},
		"cd with an unrelated body":             {Run: "cd dist && sha256sum -- * > SHA256SUMS"},
		"a step with no body":                   {Uses: "actions/checkout@x", WorkingDirectory: "x"},
	} {
		assert.False(t, stepRunsRacePass(step), "must not be matched: %s", name)
	}
}

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

// TestRaceTimeoutFitsEveryJobThatRunsIt asserts, over every workflow file
// that runs scripts/test, that the timeout scripts/test gives each go test
// call of the race pass is longer than go test's default of 10 minutes and at
// least 5 minutes shorter than the timeout-minutes of every job that runs the
// race pass, so that a hung test fails with go test's report of the running
// tests before the runner cancels the job; and that the number of such jobs
// per file is the one in racePassJobCounts, so that a new job or file that
// runs the race pass fails here until it is listed. The rule bounds one go
// test call, as the fresh rule does: a race shard job makes two calls. The
// Windows jobs run their own go test -race lines, outside scripts/test and
// outside this rule.
func TestRaceTimeoutFitsEveryJobThatRunsIt(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile(noRaceScriptPath)
	require.NoError(t, err)
	minutes := raceTimeoutOf(t, string(script))
	paths, err := filepath.Glob(".github/workflows/*.y*ml")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		assert.Empty(t, raceTimeoutProblems(minutes, readWorkflow(t, path), racePassJobCounts[filepath.Base(path)]), path)
	}
	for file := range racePassJobCounts {
		assert.Contains(t, paths, ".github/workflows/"+file, "racePassJobCounts lists a workflow file that does not exist")
	}
}

// compositeAction holds the steps of a composite action file.
type compositeAction struct {
	Runs struct {
		Steps []workflowStep `yaml:"steps"`
	} `yaml:"runs"`
}

// actionRacePassProblems returns the steps of a composite action that can
// run the race pass. An action cannot be tied to the timeout-minutes of the
// job that calls it, so none may run it. A reusable workflow (uses:
// ./.github/workflows/x) is a workflow file and is checked on its own with
// the job counts.
func actionRacePassProblems(path string, steps []workflowStep) []string {
	var out []string
	for i, step := range steps {
		if stepRunsRacePass(step) {
			out = append(out, fmt.Sprintf("%s step %d can run the race pass: an action cannot be checked against the timeout-minutes of its job", path, i+1))
		}
	}
	return out
}

// TestRaceTimeoutActionsRunNoRacePass asserts that no step of a composite
// action under .github/actions can run the race pass of scripts/test.
func TestRaceTimeoutActionsRunNoRacePass(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(".github/actions/*/action.y*ml")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var action compositeAction
		require.NoError(t, yaml.Unmarshal(data, &action), path)
		require.NotEmpty(t, action.Runs.Steps, path)
		assert.Empty(t, actionRacePassProblems(path, action.Runs.Steps), path)
	}
}

// TestRaceTimeoutActionRules asserts what actionRacePassProblems accepts and
// refuses.
func TestRaceTimeoutActionRules(t *testing.T) {
	t.Parallel()
	for name, steps := range map[string][]workflowStep{
		"scripts/test":            {{Run: "scripts/test -pass=race"}},
		"make validate":           {{Run: "make validate"}},
		"a race shard":            {{Run: "./scripts/validate tests-race-1"}},
		"a variable part":         {{Run: "./scripts/validate $PART"}},
		"a working directory":     {{WorkingDirectory: "scripts", Run: "./test"}},
		"the second of two steps": {{Run: "go install x"}, {Run: "make"}},
	} {
		assert.NotEmpty(t, actionRacePassProblems("a/action.yml", steps), "must refuse: %s", name)
	}
	assert.Empty(t, actionRacePassProblems("a/action.yml", []workflowStep{{Run: "go install x"}, {Run: "./scripts/validate static"}, {Uses: "actions/cache@x"}}))
}

// passTimeoutProblems checks a timeout of the given minutes, for the named
// pass of scripts/test, against every job of the workflow with a step that
// runsPass accepts; the workflow must have wantJobs such jobs.
func passTimeoutProblems(pass string, minutes int, wf workflowFile, runsPass func(step workflowStep) bool, wantJobs int) []string {
	var out []string
	var jobs []string
	for _, id := range slices.Sorted(maps.Keys(wf.Jobs)) {
		job := wf.Jobs[id]
		if !slices.ContainsFunc(job.Steps, runsPass) {
			continue
		}
		jobs = append(jobs, id)
		if minutes <= 10 {
			out = append(out, fmt.Sprintf("the %s timeout of %dm is not longer than go test's default of 10m", pass, minutes))
		}
		if limit := job.TimeoutMinutes - 5; minutes > limit {
			out = append(out, fmt.Sprintf("the %s timeout of %dm is more than job %s's timeout-minutes %d minus 5", pass, minutes, id, job.TimeoutMinutes))
		}
	}
	if len(jobs) != wantJobs {
		out = append(out, fmt.Sprintf("%d jobs run the %s pass %q, want %d", len(jobs), pass, jobs, wantJobs))
	}
	return out
}

// freshTimeoutProblems checks a fresh timeout of the given minutes against
// every job of the workflow with a step that runs the fresh pass; the
// workflow must have exactly one such job.
func freshTimeoutProblems(minutes int, wf workflowFile) []string {
	return passTimeoutProblems("fresh", minutes, wf, func(step workflowStep) bool {
		return slices.Contains(freshPassStepBodies, strings.TrimSpace(step.Run))
	}, 1)
}

// raceTimeoutProblems checks a race timeout of the given minutes against
// every job of the workflow with a step that runs the race pass; the
// workflow must have wantJobs such jobs. A step whose body the matcher takes
// for a race pass run but that is not one of racePassStepBodies is a problem
// of its own: its job is not known to fit. A working-directory default of
// the workflow or the job counts as the step's own.
func raceTimeoutProblems(minutes int, wf workflowFile, wantJobs int) []string {
	wf = withDefaultDirectories(wf)
	var out []string
	for _, id := range slices.Sorted(maps.Keys(wf.Jobs)) {
		for i, step := range wf.Jobs[id].Steps {
			if stepRunsRacePass(step) && !slices.Contains(racePassStepBodies, strings.TrimSpace(step.Run)) {
				out = append(out, fmt.Sprintf("job %s step %d can run the race pass but is not one of %q", id, i+1, racePassStepBodies))
			}
		}
	}
	return append(out, passTimeoutProblems("race", minutes, wf, func(step workflowStep) bool {
		return slices.Contains(racePassStepBodies, strings.TrimSpace(step.Run)) || stepRunsRacePass(step)
	}, wantJobs)...)
}

// timeoutRulesWorkflow returns a workflow with one job per limit, each with
// one step that runs body.
func timeoutRulesWorkflow(body string, limits ...int) workflowFile {
	w := workflowFile{Jobs: map[string]workflowJob{}}
	for i, limit := range limits {
		w.Jobs["job"+strconv.Itoa(i)] = workflowJob{TimeoutMinutes: limit, Steps: []workflowStep{{Run: body}}}
	}
	return w
}

// withDefaults returns the workflow with a run.working-directory default at
// the workflow level and at the level of every job; an empty directory sets
// none.
func withDefaults(wf workflowFile, workflowDir, jobDir string) workflowFile {
	node := func(dir string) yaml.Node {
		var doc yaml.Node
		if dir == "" {
			return doc
		}
		if err := yaml.Unmarshal([]byte("run:\n  working-directory: "+dir+"\n"), &doc); err != nil {
			panic(err)
		}
		return *doc.Content[0]
	}
	wf.Defaults = node(workflowDir)
	jobs := map[string]workflowJob{}
	for id, job := range wf.Jobs {
		job.Defaults = node(jobDir)
		jobs[id] = job
	}
	wf.Jobs = jobs
	return wf
}

// TestFreshTimeoutProblemsRules asserts what freshTimeoutProblems accepts and
// refuses.
func TestFreshTimeoutProblemsRules(t *testing.T) {
	t.Parallel()
	wf := func(limits ...int) workflowFile { return timeoutRulesWorkflow(testsConformanceStepBody, limits...) }
	for _, c := range []struct {
		name    string
		minutes int
		wf      workflowFile
		want    string
	}{
		{name: "fits", minutes: 25, wf: wf(30)},
		{name: "make validate in a job too short", minutes: 25, wf: timeoutRulesWorkflow("make validate", 20), want: "minus 5"},
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

// TestRaceTimeoutProblemsRules asserts what raceTimeoutProblems accepts and
// refuses: a race shard job and make validate, a timeout too close to the job
// limit or not above go test's default, a job count other than the one the
// file is expected to have, and a run body that can run the race pass but is
// not listed.
func TestRaceTimeoutProblemsRules(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		minutes int
		wf      workflowFile
		jobs    int
		want    string
	}{
		{name: "shards fit", minutes: 20, wf: timeoutRulesWorkflow(testsRace1StepBody, 30, 30, 30), jobs: 3},
		{name: "make validate fits", minutes: 20, wf: timeoutRulesWorkflow("make validate", 40), jobs: 1},
		{name: "the limit minus 5", minutes: 25, wf: timeoutRulesWorkflow(testsRace2StepBody, 30), jobs: 1},
		{name: "too close to the job limit", minutes: 26, wf: timeoutRulesWorkflow(testsRace1StepBody, 30), jobs: 1, want: "minus 5"},
		{name: "release job too short", minutes: 20, wf: timeoutRulesWorkflow("make validate", 22), jobs: 1, want: "minus 5"},
		{name: "a shard job with a shorter limit", minutes: 20, wf: timeoutRulesWorkflow(testsRace2StepBody, 24), jobs: 1, want: "minus 5"},
		{name: "go test's default", minutes: 10, wf: timeoutRulesWorkflow(testsRace1StepBody, 30), jobs: 1, want: "not longer than go test's default"},
		{name: "fewer jobs than expected", minutes: 20, wf: timeoutRulesWorkflow(testsRace1StepBody, 30, 30), jobs: 3, want: "2 jobs run"},
		{name: "a job in a file that has none", minutes: 20, wf: timeoutRulesWorkflow(testsRace1StepBody, 30), jobs: 0, want: "1 jobs run"},
		{name: "make test in a file that has none", minutes: 20, wf: timeoutRulesWorkflow("make test", 10), jobs: 0, want: "1 jobs run"},
		{name: "make test is not listed", minutes: 20, wf: timeoutRulesWorkflow("make test", 30), jobs: 1, want: "is not one of"},
		{name: "scripts/test is not listed", minutes: 20, wf: timeoutRulesWorkflow("./scripts/test -pass=race", 30), jobs: 1, want: "is not one of"},
		{name: "a workflow default directory", minutes: 20, wf: withDefaults(timeoutRulesWorkflow("./test", 30), "scripts", ""), jobs: 1, want: "is not one of"},
		{name: "a job default directory", minutes: 20, wf: withDefaults(timeoutRulesWorkflow("./validate tests-race-1", 30), "", "scripts"), jobs: 1, want: "is not one of"},
		{name: "a job default directory with a short limit", minutes: 20, wf: withDefaults(timeoutRulesWorkflow("./test", 22), "", "scripts"), jobs: 0, want: "minus 5"},
		{name: "a default directory and an unrelated body", minutes: 20, wf: withDefaults(timeoutRulesWorkflow("sha256sum -- *", 10), "dist", "dist"), jobs: 0},
		{name: "no default directory", minutes: 20, wf: withDefaults(timeoutRulesWorkflow("./test", 10), "", ""), jobs: 0},
		{name: "static does not run the race pass", minutes: 20, wf: timeoutRulesWorkflow(staticStepBody, 10), jobs: 0},
		{name: "make e2e does not run the race pass", minutes: 20, wf: timeoutRulesWorkflow("make e2e", 10), jobs: 0},
		{name: "go test does not run scripts/test", minutes: 20, wf: timeoutRulesWorkflow("go test -race ./...", 10), jobs: 0},
	} {
		problems := raceTimeoutProblems(c.minutes, c.wf, c.jobs)
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
	assert.Empty(t, partStepProblems(job, testsConformanceStepBody, []map[string]any{{"name": linuxCPUStepName, "run": linuxCPUCommand + "\n"}}), "the CPU model step")
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
		"a CPU step with an if":               {{"name": linuxCPUStepName, "run": linuxCPUCommand + "\n", "if": "always()"}},
		"a CPU step with an env":              {{"name": linuxCPUStepName, "run": linuxCPUCommand + "\n", "env": map[string]any{"GOFLAGS": "-run=^$"}}},
		"a CPU step with a shell":             {{"name": linuxCPUStepName, "run": linuxCPUCommand + "\n", "shell": "bash"}},
		"a CPU step with another command":     {{"name": linuxCPUStepName, "run": "echo done\n"}},
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
// golangci-lint version check before the lint call, and that the call waits
// for another lint run instead of exiting (--allow-serial-runners).
func TestLintScriptChecksTheVersion(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(lintScript)
	require.NoError(t, err)
	script := string(data)
	check := strings.Index(script, versionScript)
	run := strings.Index(script, lintCall)
	require.GreaterOrEqual(t, run, 0, "%s must run %s", lintScript, lintCall)
	require.GreaterOrEqual(t, check, 0, "%s must call %s", lintScript, versionScript)
	assert.Less(t, check, run, "%s must check the version before %s", lintScript, lintCall)
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

// lintCallFlagRE finds the whole word --allow-serial-runners.
var lintCallFlagRE = regexp.MustCompile(`(^|[\s;&|(])--allow-serial-runners($|[\s;&|)"'])`)

// lintCallRE finds a golangci-lint call that runs the linter: the word run
// after any other words (golangci-lint -v run, golangci-lint --color never
// run), with any white space between.
var lintCallRE = regexp.MustCompile(`golangci-lint(?:\s+\S+)*?\s+run\b`)

// lintActionAllowlist lists, as "file:line text", the lines that use the
// golangci-lint action, with the reason each is allowed. The action runs the
// linter without the flag, so none is allowed today.
var lintActionAllowlist = map[string]string{}

// lintCallWithoutFlag reports whether a line runs the lint call, in code or
// in a comment, with a call that lacks --allow-serial-runners after it. A
// line with two calls needs the flag after each.
func lintCallWithoutFlag(line string) bool {
	calls := lintCallRE.FindAllStringIndex(line, -1)
	for i, call := range calls {
		end := len(line)
		if i+1 < len(calls) {
			end = calls[i+1][0]
		}
		if !lintCallFlagRE.MatchString(line[call[1]:end]) {
			return true
		}
	}
	return false
}

// lintActionUse reports whether a line uses the golangci-lint action.
func lintActionUse(line string) bool {
	return strings.Contains(line, "golangci-lint-action")
}

// TestGolangciLintCallMatcherRules asserts which lines count as a lint call
// without --allow-serial-runners.
func TestGolangciLintCallMatcherRules(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"golangci-lint run",
		"    golangci-lint run",
		"golangci-lint run ./...",
		"golangci-lint run --fast",
		"golangci-lint run --allow-serial-runnersX",
		"golangci-lint run --allow-serial-runners-not",
		"# golangci-lint run",
		"# call golangci-lint run to lint",
		"golangci-lint run --allow-serial-runners && golangci-lint run",
		"run: golangci-lint run ./internal/...",
		"golangci-lint  run",
		"golangci-lint\trun --fast",
		"golangci-lint -v run",
		"golangci-lint --color never run ./...",
		"golangci-lint -v run --allow-serial-runnersX",
		"golangci-lint -v run --allow-serial-runners && golangci-lint  run",
	} {
		assert.True(t, lintCallWithoutFlag(line), "%q must be refused", line)
	}
	for _, line := range []string{
		lintCall,
		"    " + lintCall,
		"golangci-lint run --fast --allow-serial-runners",
		"golangci-lint run --allow-serial-runners --fast",
		"golangci-lint run --allow-serial-runners ./...",
		"golangci-lint  run --allow-serial-runners",
		"golangci-lint -v run --allow-serial-runners",
		"golangci-lint --color never run --allow-serial-runners ./...",
		"golangci-lint run --allow-serial-runners; echo done",
		lintCall + " && " + lintCall,
		"golangci-lint version",
		"golangci-lint",
		"./scripts/golangci-lint-version",
		"echo run golangci-lint",
		"",
	} {
		assert.False(t, lintCallWithoutFlag(line), "%q must be accepted", line)
	}
	for _, line := range []string{
		"uses: golangci/golangci-lint-action@v8",
		"      - uses: golangci/golangci-lint-action # x",
	} {
		assert.True(t, lintActionUse(line), "%q must count as a use of the action", line)
	}
	for _, line := range []string{"golangci-lint run", "./scripts/golangci-lint-version", "uses: actions/checkout@x"} {
		assert.False(t, lintActionUse(line), "%q must not count as a use of the action", line)
	}
}

// TestGolangciLintCallsAllowSerialRunners asserts that every line that runs
// golangci-lint in scripts/, .githooks/, .github/, the Makefile and
// install.sh passes --allow-serial-runners (a call with other words before
// run included, such as golangci-lint -v run, and none uses the golangci-lint
// action unless lintActionAllowlist lists it), so that a second lint run, from
// another session or job on the same machine, waits for the first instead of
// exiting with an error. The test files are outside the scan: they name the
// call as data. The scan finds at least the calls of scripts/lint and
// scripts/validate.
func TestGolangciLintCallsAllowSerialRunners(t *testing.T) {
	t.Parallel()
	files, err := repositoryFiles()
	require.NoError(t, err)
	found := map[string]int{}
	for _, file := range files {
		if !strings.HasPrefix(file, "scripts/") && !strings.HasPrefix(file, ".githooks/") &&
			!strings.HasPrefix(file, ".github/") && file != "Makefile" && file != "install.sh" {
			continue
		}
		info, err := os.Stat(file)
		require.NoError(t, err)
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for n, line := range strings.Split(string(data), "\n") {
			if lintActionUse(line) {
				_, allowed := lintActionAllowlist[file+":"+strings.TrimSpace(line)]
				assert.True(t, allowed, "%s:%d uses the golangci-lint action, which runs the linter without --allow-serial-runners: %s", file, n+1, strings.TrimSpace(line))
			}
			if !lintCallRE.MatchString(line) {
				continue
			}
			found[file]++
			assert.False(t, lintCallWithoutFlag(line), "%s:%d runs golangci-lint without --allow-serial-runners: %s", file, n+1, strings.TrimSpace(line))
		}
	}
	assert.Equal(t, 1, found[lintScript], "%s holds one lint call", lintScript)
	assert.Equal(t, 1, found[validateScript], "%s holds one lint call", validateScript)
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

// writeTestFile writes a data file, with its parent directories, that no test
// runs.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// writeTestScript writes an executable file, with its parent directories,
// through the writer in internal/testfs.
func writeTestScript(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	testfs.WriteExecutable(t, path, []byte(content))
}

func copyRepositoryFile(t *testing.T, rel, destRoot string) {
	t.Helper()
	data, err := os.ReadFile(rel)
	require.NoError(t, err)
	writeTestScript(t, filepath.Join(destRoot, rel), string(data))
}

// scriptCommand returns the command that runs the script at path with args
// through /bin/bash, the interpreter every repository script names on its
// first line. Bash reads the script, so the operating system never starts the
// file as a program and a copy that was just written cannot fail with "text
// file busy".
func scriptCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/bash", append([]string{path}, args...)...)
}

// runScript runs the script at path as a program: its first line names the
// interpreter and the file must have the execute bit. It passes args and adds
// env to the process environment, and returns the exit code and stderr. Use it
// only for the repository's own scripts; use runCopiedScript for a copy a test
// wrote.
func runScript(t *testing.T, dir, path string, env []string, args ...string) (int, string) {
	t.Helper()
	return runCommandStderr(t, exec.CommandContext(t.Context(), path, args...), dir, env)
}

// runCopiedScript runs the script at path, a copy a test just wrote, through
// scriptCommand, with args and with env added to the process environment. It
// returns the exit code and stderr.
func runCopiedScript(t *testing.T, dir, path string, env []string, args ...string) (int, string) {
	t.Helper()
	return runCommandStderr(t, scriptCommand(t.Context(), path, args...), dir, env)
}

func runCommandStderr(t *testing.T, cmd *exec.Cmd, dir string, env []string) (int, string) {
	t.Helper()
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return exitCodeOf(t, err), stderr.String()
}

// TestScriptCommandRunsThroughBash asserts scriptCommand builds a bash
// command line and that a copy written without the execute bit runs through
// runCopiedScript and exits with the status it sets.
func TestScriptCommandRunsThroughBash(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	cmd := scriptCommand(t.Context(), "/tmp/a script", "-x", "y")
	assert.Equal(t, []string{"/bin/bash", "/tmp/a script", "-x", "y"}, cmd.Args)

	dir := t.TempDir()
	path := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/bash\necho \"$1\" >&2\nexit 7\n"), 0o644))

	code, stderr := runCopiedScript(t, dir, path, nil, "seen")

	assert.Equal(t, 7, code)
	assert.Equal(t, "seen\n", stderr)
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
	writeTestFile(t, filepath.Join(root, setupToolsFile), "run: "+installPathPrefix+pin+"\n")
	writeTestScript(t, filepath.Join(root, "install.sh"), "#!/bin/sh\n:\n")
	writeTestScript(t, filepath.Join(root, "scripts/test"), stubScript("scripts/test", ""))
	writeTestScript(t, filepath.Join(root, "scripts/build"), stubScript("scripts/build",
		"mkdir -p bin\ntouch bin/rancher bin/rancher-credential-vault\nchmod +x bin/rancher bin/rancher-credential-vault"))

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
		writeTestScript(t, filepath.Join(stubs, name), stubScript(name, body))
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
			code, stderr := runCopiedScript(t, root, filepath.Join(root, validateScript), full, args...)
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
		lint := index(all, lintCall)
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
		assert.Contains(t, static, lintCall, ci)
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
		assert.NotContains(t, tests, lintCall, ci)
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
		writeTestScript(t, filepath.Join(bin, "golangci-lint"), "#!/bin/bash\nprintf '%s\\n' '"+versionOutput+"'\n")
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
			writeTestFile(t, filepath.Join(root, setupToolsFile), c.action)
		}
		env, _ := versionPath(t, releaseShape(bare))
		code, stderr := runCopiedScript(t, "", filepath.Join(root, versionScript), env)
		assert.Equal(t, 1, code, c.name)
		assert.Contains(t, stderr, setupToolsFile, c.name)
		assert.Equal(t, 1, strings.Count(stderr, "\n"), "%s: want one line on stderr, got %q", c.name, stderr)
	}

	out, err := exec.CommandContext(t.Context(), "git", "ls-files", "-s", "--", versionScript).Output()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(out), "100755 "), "%s must be tracked with mode 100755, git has %q", versionScript, strings.TrimSpace(string(out)))
}
