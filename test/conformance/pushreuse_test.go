package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	reuseJobID = "reuse"

	// reuseNeeds and reuseIf are the only needs and if a job of ci.yaml other
	// than the reuse job may carry. The expression starts with !cancelled(),
	// a status function. The job then also runs when reuse is skipped (a
	// manual or pull request run), and runs whenever the output is anything but
	// the string true.
	reuseNeeds = "reuse"
	reuseIf    = "${{ !cancelled() && needs.reuse.outputs.reuse != 'true' }}"

	reuseJobIf  = "github.event_name == 'push'"
	reuseOutput = "${{ steps.find.outputs.reuse }}"
	reuseToken  = "${{ github.token }}"

	reuseRepo = "rancher/rancher-cli"
	reuseSHA  = "0123456789abcdef0123456789abcdef01234567"

	// The stub answers the two gh calls of the find step and nothing else,
	// from canned JSON, and applies no query: the step must filter every field
	// itself. It exits 99 for any other call shape; a changed call can never
	// reach a real gh.
	reuseStubGH = `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
if [ "${1:-}" = api ] && [ "$#" -eq 2 ]; then
  case "$2" in
    repos/*/actions/workflows/ci.yaml/runs\?*)
      if [ -n "${STUB_RUNS_FAIL:-}" ]; then echo "stub gh: HTTP 500" >&2; exit 1; fi
      exec cat "$STUB_DIR/runs.json" ;;
    repos/*/actions/caches\?*)
      if [ -n "${STUB_CACHES_FAIL:-}" ]; then echo "stub gh: HTTP 500" >&2; exit 1; fi
      exec cat "$STUB_DIR/caches.json" ;;
  esac
fi
echo "stub gh: unknown call: $*" >&2
exit 99
`

	// The stub timeout is used only where the real one is missing. It drops its
	// duration and runs the command, as the real one does while the command
	// finishes in time.
	reuseStubTimeout = `#!/bin/sh
shift
exec "$@"
`

	reuseRunsRequest   = "api repos/" + reuseRepo + "/actions/workflows/ci.yaml/runs?event=workflow_dispatch&head_sha=" + reuseSHA + "&status=success&per_page=100"
	reuseCachesRequest = "api repos/" + reuseRepo + "/actions/caches?ref=refs/heads/main&sort=created_at&direction=desc&per_page=100"
)

// reuseFindStep returns the run body and the env of the find step of the
// reuse job of ci.yaml.
func reuseFindStep(t *testing.T) (run string, env map[string]string) {
	t.Helper()
	job, ok := readWorkflow(t, ciWorkflowPath).Jobs[reuseJobID]
	require.True(t, ok, "ci.yaml has no job %s", reuseJobID)
	for _, step := range job.Steps {
		if step.ID == "find" {
			require.NoError(t, step.Env.Decode(&env))
			require.NotEmpty(t, step.Run)
			return step.Run, env
		}
	}
	require.FailNow(t, "the job "+reuseJobID+" has no step find")
	return "", nil
}

// reuseJobProblems returns what is wrong with the jobs of a workflow against
// the rules of the reuse job: the job reuse exists, runs only on a push, has
// the one permission it needs, does no checkout and has one step whose run
// body holds no expression; every other job has the needs and if of the
// reuse job: needs reuse (the string, not a list) and the one if.
func reuseJobProblems(jobs map[string]map[string]any) []string {
	reuse, ok := jobs[reuseJobID]
	if !ok {
		return []string{"the workflow has no job " + reuseJobID}
	}
	var out []string
	if keys := slices.Sorted(maps.Keys(reuse)); !slices.Equal(keys, []string{"if", "outputs", "permissions", "runs-on", "steps", "timeout-minutes"}) {
		out = append(out, fmt.Sprintf("the job reuse has the keys %v and must have exactly if, outputs, permissions, runs-on, steps and timeout-minutes", keys))
	}
	if reuse["if"] != reuseJobIf {
		out = append(out, fmt.Sprintf("the job reuse has the if %v and must have exactly %s: it looks for a manual run on a push only", reuse["if"], reuseJobIf))
	}
	if reuse["runs-on"] != "ubuntu-latest" {
		out = append(out, fmt.Sprintf("the job reuse runs on %v and must run on ubuntu-latest", reuse["runs-on"]))
	}
	if reuse["timeout-minutes"] != 5 {
		out = append(out, fmt.Sprintf("the job reuse has timeout-minutes %v and must have 5", reuse["timeout-minutes"]))
	}
	if !reflect.DeepEqual(reuse["permissions"], map[string]any{"actions": "read"}) {
		out = append(out, fmt.Sprintf("the job reuse has the permissions %v and must have exactly actions: read, which lists runs and caches and nothing else", reuse["permissions"]))
	}
	if !reflect.DeepEqual(reuse["outputs"], map[string]any{"reuse": reuseOutput}) {
		out = append(out, fmt.Sprintf("the job reuse has the outputs %v and must have exactly reuse: %s", reuse["outputs"], reuseOutput))
	}
	steps, _ := reuse["steps"].([]any)
	if len(steps) != 1 {
		out = append(out, fmt.Sprintf("the job reuse has %d steps and must have exactly one, the step find: a checkout or another action does not belong in a job that only reads the API", len(steps)))
	} else {
		step := mapOf(steps[0])
		if keys := slices.Sorted(maps.Keys(step)); !slices.Equal(keys, []string{"env", "id", "run"}) {
			out = append(out, fmt.Sprintf("the step of the job reuse has the keys %v and must have exactly env, id and run", keys))
		}
		if step["id"] != "find" {
			out = append(out, fmt.Sprintf("the step of the job reuse has the id %v and must have find", step["id"]))
		}
		env := mapOf(step["env"])
		if keys := slices.Sorted(maps.Keys(env)); !slices.Equal(keys, []string{"GH_TOKEN", "SAVING_JOBS"}) {
			out = append(out, fmt.Sprintf("the step find has the env names %v and must have exactly GH_TOKEN and SAVING_JOBS", keys))
		}
		if env["GH_TOKEN"] != reuseToken {
			out = append(out, fmt.Sprintf("GH_TOKEN of the step find is %v and must be %s", env["GH_TOKEN"], reuseToken))
		}
		if run, _ := step["run"].(string); strings.Contains(run, "${{") {
			out = append(out, "the run body of the step find holds an expression; pass values through env")
		}
	}
	for _, id := range slices.Sorted(maps.Keys(jobs)) {
		if id == reuseJobID {
			continue
		}
		if jobs[id]["needs"] != reuseNeeds {
			out = append(out, fmt.Sprintf("job %s has needs %v and must have exactly the string %s (not a list)", id, jobs[id]["needs"], reuseNeeds))
		}
		if jobs[id]["if"] != reuseIf {
			out = append(out, fmt.Sprintf("job %s has the if %v and must have exactly %s", id, jobs[id]["if"], reuseIf))
		}
	}
	return out
}

func reuseWorkflowProblems(t *testing.T, text string) []string {
	t.Helper()
	var wf struct {
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(text), &wf))
	return reuseJobProblems(wf.Jobs)
}

const reuseFixture = `
jobs:
  reuse:
    if: github.event_name == 'push'
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions:
      actions: read
    outputs:
      reuse: ${{ steps.find.outputs.reuse }}
    steps:
      - id: find
        env:
          GH_TOKEN: ${{ github.token }}
          SAVING_JOBS: lint
        run: echo reuse=false >> "$GITHUB_OUTPUT"
  lint:
    needs: reuse
    if: ${{ !cancelled() && needs.reuse.outputs.reuse != 'true' }}
    runs-on: ubuntu-latest
    steps:
      - run: go version
  test-1:
    needs: reuse
    if: ${{ !cancelled() && needs.reuse.outputs.reuse != 'true' }}
    runs-on: ubuntu-latest
    steps:
      - run: go version
`

// TestEveryCIJobWaitsForTheReuseJob asserts that the job reuse of ci.yaml has
// exactly the keys, permissions and the one step it is allowed, and that every
// other job, present and future, needs reuse as a string and carries the one
// if that runs the job unless the output of reuse is true. A job without the
// needs and if of the reuse job would run on a push run that reuses a manual
// run or be skipped on a run that must test the commit.
func TestEveryCIJobWaitsForTheReuseJob(t *testing.T) {
	t.Parallel()
	jobs := readRawJobs(t, ciWorkflowPath)
	assert.GreaterOrEqual(t, len(jobs), 14, "ci.yaml holds fewer jobs than the rule expects")
	assert.Empty(t, reuseJobProblems(jobs))

	assert.Empty(t, reuseWorkflowProblems(t, reuseFixture), "the fixture before any edit")
	const (
		needsAndIf = "    needs: reuse\n    if: ${{ !cancelled() && needs.reuse.outputs.reuse != 'true' }}\n"
		lint       = "  lint:\n"
	)
	for _, c := range []struct{ name, old, with string }{
		{"a job without the needs and if of the reuse job", lint + needsAndIf, lint},
		{"a job without needs", lint + "    needs: reuse\n", lint},
		{"a job without if", needsAndIf, "    needs: reuse\n"},
		{"a job with needs as a list", lint + "    needs: reuse\n", lint + "    needs: [reuse]\n"},
		{"a job with needs on another job", lint + "    needs: reuse\n", lint + "    needs: test-1\n"},
		{"a job that runs when the output is false", lint + needsAndIf, lint + "    needs: reuse\n    if: ${{ !cancelled() && needs.reuse.outputs.reuse == 'false' }}\n"},
		{"a job with success() instead of !cancelled()", lint + needsAndIf, lint + "    needs: reuse\n    if: ${{ success() && needs.reuse.outputs.reuse != 'true' }}\n"},
		{"a job that always runs", lint + needsAndIf, lint + "    needs: reuse\n    if: ${{ always() }}\n"},
		{"reuse without the event condition", "    if: github.event_name == 'push'\n", ""},
		{"reuse with another event condition", "    if: github.event_name == 'push'\n", "    if: github.event_name != 'pull_request'\n"},
		{"reuse with another permission", "      actions: read\n", "      actions: read\n      contents: read\n"},
		{"reuse with a checkout step", "      - id: find\n", "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n      - id: find\n"},
		{"reuse with the wrong token", "GH_TOKEN: ${{ github.token }}", "GH_TOKEN: ${{ secrets.TOKEN }}"},
		{"reuse with an expression in the body", "run: echo reuse=false", "run: echo ${{ github.sha }}"},
	} {
		require.Contains(t, reuseFixture, c.old, c.name)
		assert.NotEmpty(t, reuseWorkflowProblems(t, strings.Replace(reuseFixture, c.old, c.with, 1)), "must refuse: %s", c.name)
	}
}

// savingGoCacheNames returns the go-cache name of every job of ci.yaml that
// saves its own entry: its go-cache step passes no restore-from.
func savingGoCacheNames(wf workflowFile) []string {
	var names []string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.Uses == goCacheAction && step.With["restore-from"] == "" {
				names = append(names, step.With["name"])
			}
		}
	}
	slices.Sort(names)
	return names
}

// TestSavingJobsListNamesEveryJobThatSavesACache asserts that SAVING_JOBS in
// the find step names the go-cache name of every job of ci.yaml that saves a
// cache, readers (jobs that only restore) excluded. A new saving job must be
// listed before a push can reuse a run without it. The step looks for a cache
// entry saved on main today for each name and does not reuse a run when one
// has none.
func TestSavingJobsListNamesEveryJobThatSavesACache(t *testing.T) {
	t.Parallel()
	_, env := reuseFindStep(t)
	saving := savingGoCacheNames(readWorkflow(t, ciWorkflowPath))
	require.NotEmpty(t, saving)
	for _, name := range saving {
		assert.NotContains(t, name, "${{", "a saving job with a matrix name cannot be matched to SAVING_JOBS; add the matching here")
	}
	listed := strings.Fields(env["SAVING_JOBS"])
	slices.Sort(listed)
	assert.Equal(t, saving, listed, "SAVING_JOBS and the saving jobs of ci.yaml differ")
}

type reuseRun struct {
	ID           int     `json:"id"`
	HTMLURL      string  `json:"html_url"`
	Path         string  `json:"path"`
	Event        string  `json:"event"`
	HeadSHA      string  `json:"head_sha"`
	Status       string  `json:"status"`
	Conclusion   *string `json:"conclusion"`
	CreatedAt    *string `json:"created_at"`
	RunStartedAt *string `json:"run_started_at"`
}

type reuseCache struct {
	ID  int    `json:"id"`
	Ref string `json:"ref"`
	Key string `json:"key"`
}

// reuseSavedEntry is the entry of the day for one go-cache name, in the form
// the go-cache action saves it.
func reuseSavedEntry(id int, name, day string) reuseCache {
	osName := "Linux"
	if strings.HasPrefix(name, "install-windows") || strings.HasPrefix(name, "e2e-windows") {
		osName = "Windows"
	}
	return reuseCache{ID: id, Ref: "refs/heads/main", Key: "go-" + name + "-" + osName + "-X64-1.27.0-0123abcd-" + day}
}

type reuseCase struct {
	name string
	// event is GITHUB_EVENT_NAME; empty means push.
	event string
	// ago is how long before now the run was created; zero means one hour. The
	// run was always started again an hour ago, as a re-run does.
	ago  time.Duration
	edit func(r *reuseRun)
	// noRun lists no run; rawRuns and rawCaches replace the canned answers.
	noRun                  bool
	rawRuns, rawCaches     string
	runsFail, cachesFail   bool
	caches                 func(savingJobs []string, today, yesterday string) []reuseCache
	want                   bool
	wantSummaryAndRequests bool
	// wantConsole is text the step must print, the reason for a false answer.
	wantConsole string
}

func reuseCases() []reuseCase {
	conclusion := func(c string) func(r *reuseRun) {
		return func(r *reuseRun) { r.Conclusion = new(c) }
	}
	allDays := func(day func(today, yesterday string) string) func(savingJobs []string, today, yesterday string) []reuseCache {
		return func(savingJobs []string, today, yesterday string) []reuseCache {
			var out []reuseCache
			for i, name := range savingJobs {
				out = append(out, reuseSavedEntry(i+1, name, day(today, yesterday)))
			}
			return out
		}
	}
	without := func(drop string) func(savingJobs []string, today, yesterday string) []reuseCache {
		return func(savingJobs []string, today, _ string) []reuseCache {
			var out []reuseCache
			for i, name := range savingJobs {
				if name != drop {
					out = append(out, reuseSavedEntry(i+1, name, today))
				}
			}
			return out
		}
	}
	return []reuseCase{
		{name: "a success run for the commit started an hour ago and today's caches", want: true, wantSummaryAndRequests: true, wantConsole: "this commit already passed in manual run 4242 of ci.yaml"},
		{name: "a run created 23 hours ago", ago: 23 * time.Hour, want: true},
		{name: "a run created 25 hours ago and started again an hour ago", ago: 25 * time.Hour},
		{name: "a path with the ref of the dispatch", edit: func(r *reuseRun) { r.Path = ".github/workflows/ci.yaml@refs/heads/branch" }, want: true},
		{name: "no run", noRun: true},
		{name: "a run of another commit", edit: func(r *reuseRun) { r.HeadSHA = "fedcba9876543210fedcba9876543210fedcba98" }},
		{name: "a push run", edit: func(r *reuseRun) { r.Event = "push" }},
		{name: "a pull request run", edit: func(r *reuseRun) { r.Event = "pull_request" }},
		{name: "a failed run", edit: conclusion("failure")},
		{name: "a cancelled run", edit: conclusion("cancelled")},
		{name: "a run with no conclusion", edit: func(r *reuseRun) { r.Conclusion = nil }},
		{name: "a run in progress with no conclusion", edit: func(r *reuseRun) { r.Status, r.Conclusion = "in_progress", nil }},
		{name: "a run in progress with the conclusion success", edit: func(r *reuseRun) { r.Status = "in_progress" }},
		{name: "a run of ci-windows.yaml", edit: func(r *reuseRun) { r.Path = ".github/workflows/ci-windows.yaml" }},
		{name: "a run of a workflow whose path starts like ci.yaml", edit: func(r *reuseRun) { r.Path = ".github/workflows/ci.yamlx" }},
		{name: "a run with no creation time", edit: func(r *reuseRun) { r.CreatedAt = nil }},
		{name: "a run with an unreadable creation time", edit: func(r *reuseRun) { r.CreatedAt = new("yesterday") }},
		{name: "the run list fails", runsFail: true, wantConsole: "could not list the manual runs of ci.yaml"},
		{name: "the cache list fails", cachesFail: true, wantConsole: "could not list the caches saved on the main branch"},
		{name: "unparsable run JSON", rawRuns: "not json", wantConsole: "could not parse the list of manual runs"},
		{name: "run JSON with no run list", rawRuns: "{}", wantConsole: "could not parse the list of manual runs"},
		{name: "unparsable cache JSON", rawCaches: "{", wantConsole: "could not parse the list of caches saved on the main branch"},
		{name: "cache JSON with no entry list", rawCaches: "{}", wantConsole: "could not parse the list of caches saved on the main branch"},
		{name: "one saving job without an entry of the day", caches: without("lint"), wantConsole: "for the lint job"},
		{name: "the entries of yesterday only", caches: allDays(func(_, yesterday string) string { return yesterday }), wantConsole: "cache entry saved on main today"},
		{name: "only an e2e-windows entry for e2e", caches: without("e2e"), wantConsole: "for the e2e job"},
		{name: "entries of another ref", caches: func(savingJobs []string, today, _ string) []reuseCache {
			var out []reuseCache
			for i, name := range savingJobs {
				e := reuseSavedEntry(i+1, name, today)
				e.Ref = "refs/pull/1/merge"
				out = append(out, e)
			}
			return out
		}, wantConsole: "cache entry saved on main today"},
		{name: "a manual run event", event: "workflow_dispatch"},
		{name: "a pull request event", event: "pull_request"},
	}
}

// reuseTools returns the directory used as PATH, holding the stub gh, links to
// the real jq, date and cat, the real timeout when the machine has one and a
// stub that drops its duration otherwise (macOS has none), and the path of
// bash.
func reuseTools(t *testing.T) (dir, bash string) {
	t.Helper()
	testfs.SkipOnWindows(t, posixOnlyReason)
	dir = t.TempDir()
	for _, name := range []string{"bash", "jq", "date", "cat"} {
		target, err := exec.LookPath(name)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("CI is set: a missing %s fails instead of skipping: %v", name, err)
			}
			t.Skipf("%s not installed: %v", name, err)
		}
		if name == "bash" {
			bash = target
			continue
		}
		testfs.Symlink(t, target, filepath.Join(dir, name))
	}
	testfs.WriteExecutable(t, filepath.Join(dir, "gh"), []byte(reuseStubGH))
	if real, err := exec.LookPath("timeout"); err == nil {
		testfs.Symlink(t, real, filepath.Join(dir, "timeout"))
	} else {
		testfs.WriteExecutable(t, filepath.Join(dir, "timeout"), []byte(reuseStubTimeout))
	}
	return dir, bash
}

type reuseResult struct {
	exit    int
	output  string
	summary string
	calls   []string
	console string
}

// runReuseBody runs the run body of the find step as the runner does (bash -e)
// with a minimal environment and the stub tools, for one case.
func runReuseBody(t *testing.T, dir, bash, run string, savingJobs []string, c reuseCase, today, yesterday string, now time.Time) reuseResult {
	t.Helper()
	data := t.TempDir()
	ago := c.ago
	if ago == 0 {
		ago = time.Hour
	}
	run0 := reuseRun{
		ID:           4242,
		HTMLURL:      "https://github.com/" + reuseRepo + "/actions/runs/4242",
		Path:         ".github/workflows/ci.yaml",
		Event:        "workflow_dispatch",
		HeadSHA:      reuseSHA,
		Status:       "completed",
		Conclusion:   new("success"),
		CreatedAt:    new(now.Add(-ago).Format(time.RFC3339)),
		RunStartedAt: new(now.Add(-time.Hour).Format(time.RFC3339)),
	}
	if c.edit != nil {
		c.edit(&run0)
	}
	runs := []reuseRun{run0}
	if c.noRun {
		runs = nil
	}
	runsJSON, err := json.Marshal(map[string]any{"total_count": len(runs), "workflow_runs": runs})
	require.NoError(t, err)
	if c.rawRuns != "" {
		runsJSON = []byte(c.rawRuns)
	}
	var caches []reuseCache
	if c.caches != nil {
		caches = c.caches(savingJobs, today, yesterday)
	} else {
		for i, name := range savingJobs {
			caches = append(caches, reuseSavedEntry(i+1, name, today))
		}
	}
	cachesJSON, err := json.Marshal(map[string]any{"total_count": len(caches), "actions_caches": caches})
	require.NoError(t, err)
	if c.rawCaches != "" {
		cachesJSON = []byte(c.rawCaches)
	}
	require.NoError(t, os.WriteFile(filepath.Join(data, "runs.json"), runsJSON, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(data, "caches.json"), cachesJSON, 0o600))

	out := filepath.Join(data, "output")
	summary := filepath.Join(data, "summary")
	log := filepath.Join(data, "calls")
	event := c.event
	if event == "" {
		event = "push"
	}
	cmd := exec.CommandContext(t.Context(), bash, "-e", "-c", run)
	cmd.Env = []string{
		"PATH=" + dir,
		"GITHUB_SHA=" + reuseSHA,
		"GITHUB_REPOSITORY=" + reuseRepo,
		"GITHUB_EVENT_NAME=" + event,
		"GITHUB_OUTPUT=" + out,
		"GITHUB_STEP_SUMMARY=" + summary,
		"GH_TOKEN=stub",
		"SAVING_JOBS=" + strings.Join(savingJobs, " "),
		"STUB_DIR=" + data,
		"STUB_LOG=" + log,
	}
	if c.runsFail {
		cmd.Env = append(cmd.Env, "STUB_RUNS_FAIL=1")
	}
	if c.cachesFail {
		cmd.Env = append(cmd.Env, "STUB_CACHES_FAIL=1")
	}
	var console bytes.Buffer
	cmd.Stdout, cmd.Stderr = &console, &console
	exit := 0
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.True(t, ok, "%v", err)
		exit = exitErr.ExitCode()
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			require.ErrorIs(t, err, os.ErrNotExist)
		}
		return string(b)
	}
	var calls []string
	if text := strings.TrimSpace(read(log)); text != "" {
		calls = strings.Split(text, "\n")
	}
	require.NotContains(t, console.String(), "stub gh: unknown call")
	return reuseResult{exit: exit, output: read(out), summary: read(summary), calls: calls, console: console.String()}
}

// TestPushRunReusesOnlyAGreenManualRunOfTheSameCommit asserts the find step of
// the reuse job, run with bash and a stub gh, with the real timeout where it
// exists and a stub where it does not, and the real jq, date and cat: it
// answers reuse=true only for a push whose commit has a successful, completed
// manual run of ci.yaml created within 24 hours, and a cache entry of the day
// on main for every saving job; any other answer, a failed call and unparsable
// JSON included, gives reuse=false. Each case asserts exit 0 and exactly one
// output line. The step reads the stub's answers with no query applied; it
// has to filter the path, event, commit, status, conclusion and creation time
// of every run and the ref and key of every cache entry itself.
func TestPushRunReusesOnlyAGreenManualRunOfTheSameCommit(t *testing.T) {
	t.Parallel()
	run, env := reuseFindStep(t)
	savingJobs := strings.Fields(env["SAVING_JOBS"])
	require.NotEmpty(t, savingJobs)
	dir, bash := reuseTools(t)
	for _, c := range reuseCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var got reuseResult
			for attempt := range 3 {
				now := time.Now().UTC()
				today := now.Format("2006-01-02")
				yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
				got = runReuseBody(t, dir, bash, run, savingJobs, c, today, yesterday, now)
				if time.Now().UTC().Format("2006-01-02") == today {
					break
				}
				require.Less(t, attempt, 2, "the UTC day changed during every attempt")
			}
			require.Zero(t, got.exit, "the step must always exit 0")
			want := "reuse=false\n"
			if c.want {
				want = "reuse=true\n"
			}
			assert.Equal(t, want, got.output, "the step printed:\n%s", got.console)
			if c.wantConsole != "" {
				assert.Contains(t, got.console, c.wantConsole)
			}
			if c.wantSummaryAndRequests {
				assert.Contains(t, got.summary, "this commit passed in manual run 4242 of ci.yaml")
				assert.Contains(t, got.summary, "4242")
				assert.Contains(t, got.summary, "https://github.com/"+reuseRepo+"/actions/runs/4242")
				assert.Equal(t, []string{reuseRunsRequest, reuseCachesRequest}, got.calls)
			}
		})
	}
}
