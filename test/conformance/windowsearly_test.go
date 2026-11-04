package conformance

import (
	"fmt"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	earlyWorkflowPath = ".github/workflows/ci-windows.yaml"
	testfsStepCommand = "go test -race -count=1 -v ./internal/testfs/"
)

// earlyJobs maps each job of ci-windows.yaml to the job of ci.yaml it
// copies.
var earlyJobs = map[string]string{
	"install-windows-install-early": "install-windows-install",
	"install-windows-config-early":  "install-windows-config",
	"install-windows-cli-early":     "install-windows-cli",
	"e2e-windows-early":             "e2e-windows",
}

// ciCheckNames are the check names ci.yaml reports for the jobs a merge
// waits on. A job that is renamed or removed stops reporting its check, so
// the change has to be a deliberate edit of this list. Other jobs may be
// added.
var ciCheckNames = []string{
	"cross-compile (darwin/amd64)",
	"cross-compile (darwin/arm64)",
	"cross-compile (linux/amd64)",
	"cross-compile (linux/arm64)",
	"cross-compile (linux/s390x)",
	"cross-compile (windows/amd64)",
	"e2e",
	"e2e-windows",
	"install-windows-cli",
	"install-windows-config",
	"install-windows-install",
	"lint",
	"test-1",
	"test-2",
	"test-3",
}

var shaPinnedUseRE = regexp.MustCompile(`@[0-9a-f]{40}$`)

var matrixReferenceRE = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)

func readWorkflowMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(data, &doc), path)
	return doc
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// workflowCheckNames returns the check name of every job of a workflow: the
// name when the job sets one and its id otherwise, with a name that refers to
// matrix values expanded over the matrix include list.
func workflowCheckNames(wf map[string]any) []string {
	var names []string
	for id, raw := range mapOf(wf["jobs"]) {
		job := mapOf(raw)
		name, _ := job["name"].(string)
		if name == "" {
			names = append(names, id)
			continue
		}
		include, _ := mapOf(mapOf(job["strategy"])["matrix"])["include"].([]any)
		if !matrixReferenceRE.MatchString(name) || len(include) == 0 {
			names = append(names, name)
			continue
		}
		for _, entry := range include {
			values := mapOf(entry)
			names = append(names, matrixReferenceRE.ReplaceAllStringFunc(name, func(ref string) string {
				key := matrixReferenceRE.FindStringSubmatch(ref)[1]
				value, _ := values[key].(string)
				return value
			}))
		}
	}
	slices.Sort(names)
	return names
}

// onTriggersProblem reports what is wrong with an `on` section that must be
// exactly workflow_dispatch with no inputs, or "" when it is right.
func onTriggersProblem(on any) string {
	triggers, ok := on.(map[string]any)
	if !ok {
		return "on is not a mapping with the one key workflow_dispatch"
	}
	if keys := sortedKeys(triggers); !slices.Equal(keys, []string{"workflow_dispatch"}) {
		return "on has the keys " + strings.Join(keys, ", ") + " and must have only workflow_dispatch"
	}
	switch dispatch := triggers["workflow_dispatch"].(type) {
	case nil:
		return ""
	case map[string]any:
		if _, has := dispatch["inputs"]; has {
			return "workflow_dispatch has inputs"
		}
		if len(dispatch) != 0 {
			return "workflow_dispatch has keys " + strings.Join(sortedKeys(dispatch), ", ")
		}
		return ""
	}
	return "workflow_dispatch is not empty"
}

// windowsEarlyProblems compares the two workflow files and returns what
// breaks the separation of the Windows-only run from the full run. Each
// problem starts with the letter of its assertion.
func windowsEarlyProblems(ci, early map[string]any) []string {
	var problems []string
	add := func(letter, format string, args ...any) {
		problems = append(problems, "("+letter+") "+strings.TrimSpace(fmt.Sprintf(format, args...)))
	}

	if p := onTriggersProblem(early["on"]); p != "" {
		add("a", "%s: ci-windows.yaml runs on dispatch only and takes no inputs", p)
	}

	earlyJobMap := mapOf(early["jobs"])
	wantEarly := []string{"e2e-windows-early", "install-windows-cli-early", "install-windows-config-early", "install-windows-install-early"}
	if got := sortedKeys(earlyJobMap); !slices.Equal(got, wantEarly) {
		add("b", "ci-windows.yaml holds the jobs %v and must hold exactly %v", got, wantEarly)
	}

	reported := workflowCheckNames(ci)
	for _, id := range sortedKeys(earlyJobMap) {
		for _, name := range []string{id, stringOf(mapOf(earlyJobMap[id])["name"])} {
			if name != "" && slices.Contains(reported, name) {
				add("c", "the job %s of ci-windows.yaml reports the check name %q that ci.yaml reports; a Windows-only run must never stand in for a check of the full run", id, name)
			}
		}
	}

	ciJobs := mapOf(ci["jobs"])
	for _, id := range sortedKeys(earlyJobMap) {
		original, ok := earlyJobs[id]
		if !ok {
			continue
		}
		copied, ciJob := mapOf(earlyJobMap[id]), mapOf(ciJobs[original])
		if ciJob == nil {
			add("d", "ci.yaml has no job %s for %s to copy", original, id)
			continue
		}
		for _, key := range []string{"runs-on", "timeout-minutes", "steps"} {
			if !reflect.DeepEqual(copied[key], ciJob[key]) {
				add("d", "the key %s of %s differs from %s in ci.yaml; a change to a Windows job is made in both ci.yaml and ci-windows.yaml", key, id, original)
			}
		}
		if got := sortedKeys(ciJob); !slices.Equal(got, []string{"runs-on", "steps", "timeout-minutes"}) {
			add("d", "%s in ci.yaml has the keys %v and must have exactly runs-on, timeout-minutes and steps; any other key changes the job in a way its copy does not carry (to add one, add it to the copy in ci-windows.yaml and to the key set this check expects)", original, got)
		}
	}
	for _, key := range []string{"env", "defaults"} {
		if !reflect.DeepEqual(ci[key], early[key]) {
			add("d", "ci.yaml and ci-windows.yaml differ in the workflow-level %s, which changes every job of one file and not the other", key)
		}
	}

	if got := sortedKeys(early); !slices.Equal(got, []string{"concurrency", "jobs", "name", "on", "permissions"}) {
		add("e", "ci-windows.yaml has the top-level keys %v and must have exactly name, on, permissions, concurrency and jobs", got)
	}
	if name, _ := early["name"].(string); name != "ci-windows" || name == stringOf(ci["name"]) {
		add("e", "the workflow name is %q and must be ci-windows, different from the name of ci.yaml", name)
	}
	if !reflect.DeepEqual(early["permissions"], map[string]any{"contents": "read"}) {
		add("e", "permissions must be exactly contents: read")
	}
	if !reflect.DeepEqual(early["concurrency"], map[string]any{
		"group":              "${{ github.workflow }}-${{ github.ref }}",
		"cancel-in-progress": false,
	}) {
		add("e", "concurrency must be the group ${{ github.workflow }}-${{ github.ref }} with cancel-in-progress false")
	}
	for _, id := range sortedKeys(earlyJobMap) {
		if got := sortedKeys(mapOf(earlyJobMap[id])); !slices.Equal(got, []string{"runs-on", "steps", "timeout-minutes"}) {
			add("e", "the job %s has the keys %v and must have exactly runs-on, timeout-minutes and steps", id, got)
		}
	}

	for _, id := range sortedKeys(earlyJobMap) {
		steps, _ := mapOf(earlyJobMap[id])["steps"].([]any)
		for _, raw := range steps {
			use := stringOf(mapOf(raw)["uses"])
			// A local path is not pinned: "./" is the relative form and "$/"
			// the form of this repository's own actions and workflows that
			// scripts/validate tells actionlint to accept.
			if use == "" || strings.HasPrefix(use, "./") || strings.HasPrefix(use, "$/") {
				continue
			}
			if !shaPinnedUseRE.MatchString(use) {
				add("f", "the step uses %q in %s and must be pinned to a full commit SHA", use, id)
			}
		}
	}

	if p := onTriggersProblem(map[string]any{"workflow_dispatch": mapOf(ci["on"])["workflow_dispatch"]}); p != "" {
		add("g", "ci.yaml must keep workflow_dispatch without inputs: %s", p)
	}
	if _, has := mapOf(ci["on"])["workflow_dispatch"]; !has {
		add("g", "ci.yaml has no workflow_dispatch")
	}
	got := workflowCheckNames(ci)
	for _, name := range ciCheckNames {
		if !slices.Contains(got, name) {
			add("g", "ci.yaml does not report the check name %q that the repository ruleset for main requires; reported: %v", name, got)
		}
	}
	return problems
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// testfsStepProblems returns what is wrong with a job's step that runs the
// tests of internal/testfs: the step must run exactly testfsStepCommand, with
// no key that could change what runs, and the job must carry no setting that
// changes every step.
func testfsStepProblems(id string, job map[string]any) []string {
	var problems []string
	for _, key := range []string{"env", "defaults", "continue-on-error", "if"} {
		if _, has := job[key]; has {
			problems = append(problems, id+" has the job key "+key+", which can change what its steps run")
		}
	}
	steps, _ := job["steps"].([]any)
	found := false
	for _, raw := range steps {
		step := mapOf(raw)
		if strings.TrimSpace(stringOf(step["run"])) != testfsStepCommand {
			continue
		}
		valid := true
		for key := range step {
			if key != "name" && key != "run" {
				valid = false
				problems = append(problems, id+" runs the helper tests in a step with the key "+key+", which can change what runs")
			}
		}
		found = found || valid
	}
	if !found {
		problems = append(problems, id+" has no step that runs exactly: "+testfsStepCommand)
	}
	return problems
}

// TestWindowsEarlyWorkflowStaysSeparate asserts the Windows-only workflow
// stays a separate file and that a run of it cannot be mistaken for a full run:
// dispatch only, jobs with names no check lists, copies of the four Windows
// jobs of ci.yaml, pinned actions and the narrowest permissions.
func TestWindowsEarlyWorkflowStaysSeparate(t *testing.T) {
	t.Parallel()
	ci := readWorkflowMap(t, ciWorkflowPath)
	early := readWorkflowMap(t, earlyWorkflowPath)
	assert.Empty(t, windowsEarlyProblems(ci, early))
}

const earlyFixtureCI = `
name: ci
on:
  push:
    branches: [main]
  workflow_dispatch:
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
  test-1:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  test-2:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  test-3:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  e2e:
    runs-on: ubuntu-latest
    steps:
      - run: make e2e
  cross-compile:
    name: cross-compile (${{ matrix.goos }}/${{ matrix.goarch }})
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - { goos: linux, goarch: amd64 }
          - { goos: linux, goarch: arm64 }
          - { goos: linux, goarch: s390x }
          - { goos: darwin, goarch: amd64 }
          - { goos: darwin, goarch: arm64 }
          - { goos: windows, goarch: amd64 }
    steps:
      - run: go build ./...
  install-windows-install:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 -run TestWindows ./install/
  install-windows-config:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 -v ./internal/testfs/
  install-windows-cli:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 ./internal/token/
  e2e-windows:
    runs-on: windows-latest
    timeout-minutes: 30
    steps:
      - run: bash scripts/e2e
        timeout-minutes: 25
`

const earlyFixtureEarly = `
name: ci-windows
on:
  workflow_dispatch:
permissions:
  contents: read
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: false
jobs:
  install-windows-install-early:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 -run TestWindows ./install/
  install-windows-config-early:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 -v ./internal/testfs/
  install-windows-cli-early:
    runs-on: windows-latest
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1
      - run: go test -race -count=1 ./internal/token/
  e2e-windows-early:
    runs-on: windows-latest
    timeout-minutes: 30
    steps:
      - run: bash scripts/e2e
        timeout-minutes: 25
`

func earlyFixture(t *testing.T, edit func(ci, early map[string]any)) (ci, early map[string]any) {
	t.Helper()
	require.NoError(t, yaml.Unmarshal([]byte(earlyFixtureCI), &ci))
	require.NoError(t, yaml.Unmarshal([]byte(earlyFixtureEarly), &early))
	if edit != nil {
		edit(ci, early)
	}
	return ci, early
}

func earlyJob(early map[string]any, id string) map[string]any {
	return mapOf(mapOf(early["jobs"])[id])
}

func earlySteps(early map[string]any, id string) []any {
	steps, _ := earlyJob(early, id)["steps"].([]any)
	return steps
}

// TestWindowsEarlyProblemsTable asserts the checker accepts the matching pair
// of files and returns the letter of the assertion that each kind of change
// breaks.
func TestWindowsEarlyProblemsTable(t *testing.T) {
	t.Parallel()
	tagged := func(early map[string]any) {
		for _, id := range []string{"install-windows-install-early"} {
			earlySteps(early, id)[0].(map[string]any)["uses"] = "actions/checkout@v7"
		}
	}
	rows := []struct {
		name string
		edit func(ci, early map[string]any)
		want []string
	}{
		{name: "the matching pair"},
		{name: "a null workflow_dispatch", edit: func(_, early map[string]any) {
			mapOf(early["on"])["workflow_dispatch"] = nil
		}},
		{name: "an empty workflow_dispatch mapping", edit: func(_, early map[string]any) {
			mapOf(early["on"])["workflow_dispatch"] = map[string]any{}
		}},
		{name: "a push trigger", edit: func(_, early map[string]any) {
			mapOf(early["on"])["push"] = nil
		}, want: []string{"a"}},
		{name: "a dispatch input", edit: func(_, early map[string]any) {
			mapOf(early["on"])["workflow_dispatch"] = map[string]any{"inputs": map[string]any{"only": nil}}
		}, want: []string{"a"}},
		{name: "a third job", edit: func(_, early map[string]any) {
			mapOf(early["jobs"])["extra"] = map[string]any{"runs-on": "windows-latest", "timeout-minutes": 1, "steps": []any{}}
		}, want: []string{"b"}},
		{name: "a missing job", edit: func(_, early map[string]any) {
			delete(mapOf(early["jobs"]), "e2e-windows-early")
		}, want: []string{"b"}},
		{name: "a missing install job", edit: func(_, early map[string]any) {
			delete(mapOf(early["jobs"]), "install-windows-cli-early")
		}, want: []string{"b"}},
		{name: "a job named like a ci check", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["name"] = "e2e-windows"
		}, want: []string{"c", "e"}},
		{name: "a job named like another ci check", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["name"] = "e2e"
		}, want: []string{"c", "e"}},
		{name: "a job named like an install check", edit: func(_, early map[string]any) {
			earlyJob(early, "install-windows-cli-early")["name"] = "install-windows-cli"
		}, want: []string{"c", "e"}},
		{name: "a changed run line in a copy", edit: func(_, early map[string]any) {
			earlySteps(early, "e2e-windows-early")[0].(map[string]any)["run"] = "bash scripts/e2e --other"
		}, want: []string{"d"}},
		{name: "a deleted step in a copy", edit: func(_, early map[string]any) {
			job := earlyJob(early, "install-windows-install-early")
			job["steps"] = job["steps"].([]any)[:1]
		}, want: []string{"d"}},
		{name: "a deleted step in the copy of another install job", edit: func(_, early map[string]any) {
			job := earlyJob(early, "install-windows-config-early")
			job["steps"] = job["steps"].([]any)[:1]
		}, want: []string{"d"}},
		{name: "a changed step timeout in a copy", edit: func(_, early map[string]any) {
			earlySteps(early, "e2e-windows-early")[0].(map[string]any)["timeout-minutes"] = 5
		}, want: []string{"d"}},
		{name: "a changed runner in a copy", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["runs-on"] = "windows-2022"
		}, want: []string{"d"}},
		{name: "a changed pin in one copy", edit: func(_, early map[string]any) {
			earlySteps(early, "install-windows-install-early")[0].(map[string]any)["uses"] = "actions/checkout@" + strings.Repeat("a", 40)
		}, want: []string{"d"}},
		{name: "a step key no typed struct lists", edit: func(_, early map[string]any) {
			earlySteps(early, "e2e-windows-early")[0].(map[string]any)["continue-on-error"] = true
		}, want: []string{"d"}},
		{name: "a step working directory", edit: func(_, early map[string]any) {
			earlySteps(early, "e2e-windows-early")[0].(map[string]any)["working-directory"] = "."
		}, want: []string{"d"}},
		{name: "a workflow env in ci.yaml", edit: func(ci, _ map[string]any) {
			ci["env"] = map[string]any{"GOFLAGS": "-run=x"}
		}, want: []string{"d"}},
		{name: "workflow defaults in ci.yaml", edit: func(ci, _ map[string]any) {
			ci["defaults"] = map[string]any{"run": map[string]any{"shell": "bash"}}
		}, want: []string{"d"}},
		{name: "the same workflow env in both files", edit: func(ci, early map[string]any) {
			ci["env"] = map[string]any{"A": "b"}
			early["env"] = map[string]any{"A": "b"}
		}, want: []string{"e"}},
		{name: "a job env on a ci Windows job only", edit: func(ci, _ map[string]any) {
			mapOf(mapOf(ci["jobs"])["install-windows-install"])["env"] = map[string]any{"GOFLAGS": "-run=x"}
		}, want: []string{"d"}},
		{name: "a job continue-on-error on a ci Windows job only", edit: func(ci, _ map[string]any) {
			mapOf(mapOf(ci["jobs"])["e2e-windows"])["continue-on-error"] = true
		}, want: []string{"d"}},
		{name: "a needs on a job", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["needs"] = "lint"
		}, want: []string{"e"}},
		{name: "an if on a job", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["if"] = "always()"
		}, want: []string{"e"}},
		{name: "wider permissions", edit: func(_, early map[string]any) {
			early["permissions"] = map[string]any{"contents": "write"}
		}, want: []string{"e"}},
		{name: "a job env", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["env"] = map[string]any{"A": "b"}
		}, want: []string{"e"}},
		{name: "a job continue-on-error", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["continue-on-error"] = true
		}, want: []string{"e"}},
		{name: "job defaults", edit: func(_, early map[string]any) {
			earlyJob(early, "e2e-windows-early")["defaults"] = map[string]any{"run": map[string]any{"shell": "bash"}}
		}, want: []string{"e"}},
		{name: "a workflow env", edit: func(_, early map[string]any) {
			early["env"] = map[string]any{"A": "b"}
		}, want: []string{"d", "e"}},
		{name: "the name of ci.yaml", edit: func(_, early map[string]any) {
			early["name"] = "ci"
		}, want: []string{"e"}},
		{name: "a concurrency group that cancels", edit: func(_, early map[string]any) {
			mapOf(early["concurrency"])["cancel-in-progress"] = true
		}, want: []string{"e"}},
		{name: "version tags in both files", edit: func(ci, early map[string]any) {
			for _, steps := range [][]any{earlySteps(early, "install-windows-install-early"), mapOf(mapOf(ci["jobs"])["install-windows-install"])["steps"].([]any)} {
				steps[0].(map[string]any)["uses"] = "actions/checkout@v7"
			}
		}, want: []string{"f"}},
		{name: "a version tag in the copy only", edit: func(_, early map[string]any) { tagged(early) }, want: []string{"d", "f"}},
		{name: "a ci dispatch input", edit: func(ci, _ map[string]any) {
			mapOf(ci["on"])["workflow_dispatch"] = map[string]any{"inputs": map[string]any{"only": nil}}
		}, want: []string{"g"}},
		{name: "a ci without dispatch", edit: func(ci, _ map[string]any) {
			delete(mapOf(ci["on"]), "workflow_dispatch")
		}, want: []string{"g"}},
		{name: "a renamed ci job", edit: func(ci, _ map[string]any) {
			jobs := mapOf(ci["jobs"])
			jobs["lint2"] = jobs["lint"]
			delete(jobs, "lint")
		}, want: []string{"g"}},
		{name: "a renamed ci Windows job", edit: func(ci, _ map[string]any) {
			jobs := mapOf(ci["jobs"])
			jobs["install-windows-cli2"] = jobs["install-windows-cli"]
			delete(jobs, "install-windows-cli")
		}, want: []string{"d", "g"}},
		{name: "an added ci job", edit: func(ci, _ map[string]any) {
			mapOf(ci["jobs"])["fault-check"] = map[string]any{"runs-on": "ubuntu-latest", "steps": []any{}}
		}},
		{name: "a ci job with the name of another check ci.yaml reports", edit: func(ci, _ map[string]any) {
			mapOf(mapOf(ci["jobs"])["test-1"])["name"] = "lint"
		}, want: []string{"g"}},
		{name: "a ci matrix without windows", edit: func(ci, _ map[string]any) {
			matrix := mapOf(mapOf(mapOf(mapOf(ci["jobs"])["cross-compile"])["strategy"])["matrix"])
			include := matrix["include"].([]any)
			matrix["include"] = include[:len(include)-1]
		}, want: []string{"g"}},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			ci, early := earlyFixture(t, c.edit)
			problems := windowsEarlyProblems(ci, early)
			var letters []string
			for _, p := range problems {
				letter := p[1:2]
				if !slices.Contains(letters, letter) {
					letters = append(letters, letter)
				}
			}
			slices.Sort(letters)
			assert.Equal(t, c.want, letters, "%v", problems)
		})
	}
}

// TestWindowsLegsRunTheHelperTests asserts the install-windows-config job of
// ci.yaml and its copy in ci-windows.yaml run the tests of internal/testfs
// with -v: the Windows branches of the helpers run nowhere else.
func TestWindowsLegsRunTheHelperTests(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ path, id string }{
		{ciWorkflowPath, "install-windows-config"},
		{earlyWorkflowPath, "install-windows-config-early"},
	} {
		job := mapOf(mapOf(readWorkflowMap(t, c.path)["jobs"])[c.id])
		require.NotNil(t, job, "%s has no job %s", c.path, c.id)
		assert.Empty(t, testfsStepProblems(c.id, job), c.path)
	}
}

// TestHelperTestStepTable asserts the check accepts the exact command and
// refuses every variation that could change which tests run or whether they
// print.
func TestHelperTestStepTable(t *testing.T) {
	t.Parallel()
	step := func(run string, extra map[string]any) map[string]any {
		job := map[string]any{"steps": []any{map[string]any{"run": run}}}
		maps.Copy(mapOf(job["steps"].([]any)[0]), extra)
		return job
	}
	rows := []struct {
		name  string
		job   map[string]any
		valid bool
	}{
		{name: "the exact command", job: step(testfsStepCommand, nil), valid: true},
		{name: "a name on the step", job: step(testfsStepCommand, map[string]any{"name": "helpers"}), valid: true},
		{name: "surrounding whitespace", job: step(" "+testfsStepCommand+"\n", nil), valid: true},
		{name: "run only some tests", job: step("go test -race -count=1 -v -run '^$' ./internal/testfs/", nil)},
		{name: "-v=false", job: step("go test -race -count=1 -v=false ./internal/testfs/", nil)},
		{name: "-count=0", job: step("go test -race -count=0 -v ./internal/testfs/", nil)},
		{name: "list only", job: step("go test -race -count=1 -v -list . ./internal/testfs/", nil)},
		{name: "short", job: step("go test -race -count=1 -v -short ./internal/testfs/", nil)},
		{name: "no -v", job: step("go test -race -count=1 ./internal/testfs/", nil)},
		{name: "no -race", job: step("go test -count=1 -v ./internal/testfs/", nil)},
		{name: "another package", job: step("go test -race -count=1 -v ./internal/telemetry/", nil)},
		{name: "an if on the step", job: step(testfsStepCommand, map[string]any{"if": false})},
		{name: "an env on the step", job: step(testfsStepCommand, map[string]any{"env": map[string]any{"GOFLAGS": "-run=x"}})},
		{name: "continue-on-error on the step", job: step(testfsStepCommand, map[string]any{"continue-on-error": true})},
		{name: "a working directory on the step", job: step(testfsStepCommand, map[string]any{"working-directory": "install"})},
		{name: "an env on the job", job: func() map[string]any {
			job := step(testfsStepCommand, nil)
			job["env"] = map[string]any{"GOFLAGS": "-run=x"}
			return job
		}()},
		{name: "no step", job: map[string]any{"steps": []any{}}},
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			problems := testfsStepProblems("job", c.job)
			if c.valid {
				assert.Empty(t, problems)
			} else {
				assert.NotEmpty(t, problems)
			}
		})
	}
}
