package conformance

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// eventDependenceRE matches a read of the event or the ref that started a
// run: the contexts github.event_name, github.event, github.ref,
// github.ref_name, github.head_ref and github.base_ref, the bracket form of
// the github context, toJSON(github), and the variables GITHUB_EVENT_NAME,
// GITHUB_EVENT_PATH, GITHUB_REF, GITHUB_REF_NAME, GITHUB_HEAD_REF and
// GITHUB_BASE_REF. Context names are not case sensitive.
var eventDependenceRE = regexp.MustCompile(`(?i)github\s*\.\s*(event|ref|head_ref|base_ref)|github\s*\[|tojson\(\s*github\s*\)|GITHUB_(EVENT|REF|HEAD_REF|BASE_REF)`)

// workflowInputsRE matches the inputs context (inputs.name or inputs['name']),
// which is empty on a push run and set on a manual run of a workflow that
// declares workflow_dispatch inputs. It is checked in ci.yaml only: in a
// composite action the same context names the action's own inputs, which
// ci.yaml passes and which are checked there.
var workflowInputsRE = regexp.MustCompile(`(?i)(^|[^a-z0-9_.])inputs(\.[a-z_]|\[)`)

// eventDependenceAllowedPaths lists the parts of ci.yaml that may read the
// event or the ref, by their path in the file, each with the reason.
var eventDependenceAllowedPaths = map[string]string{
	"concurrency": "a push run gets the group of its commit and every other run the group of its ref, so that a later push never cancels a push run that is waiting",
	"jobs.reuse":  "reads the event name to look for a manual run on a push only",
}

// cacheSaveIfReason is why the if of a cache save step may read the event and
// the ref: only a push to main saves a cache, and TestOnlyMainPushesSaveCaches
// matches the expression exactly.
const cacheSaveIfReason = "only a push to main saves a cache; TestOnlyMainPushesSaveCaches matches the expression exactly"

// eventDependenceAllowedFiles lists the files of scripts/ and .github/actions,
// and the Makefile and Go files outside the allowed directories, that may read
// the event or the ref, each with the reason. None does.
var eventDependenceAllowedFiles = map[string]string{}

// eventDependenceAllowedDirs lists the directories of Go files that are not
// scanned, each with the reason.
var eventDependenceAllowedDirs = map[string]string{
	"test/conformance": "the source scans read these names as text to check the workflow files, and its tests run on the runner with their own environment",
}

// eventDependenceSkippedDirs are directories no scan reads: version control,
// build output, documentation, vendored code and caches.
var eventDependenceSkippedDirs = []string{".git", "bin", "docs", "vendor", "graphify-out", "node_modules", ".claude"}

// eventDependentScalars appends a problem for every value in a workflow node,
// under any key, that reads the event or the ref, except the allowed paths and
// the if of a cache save step.
func eventDependentScalars(n *yaml.Node, path string, out *[]string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			eventDependentScalars(c, path, out)
		}
	case yaml.MappingNode:
		saves := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "uses" && isCacheSaveUses(n.Content[i+1].Value) {
				saves = true
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			child := key
			if path != "" {
				child = path + "." + key
			}
			if _, ok := eventDependenceAllowedPaths[child]; ok {
				continue
			}
			if saves && key == "if" {
				continue
			}
			eventDependentScalars(value, child, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			eventDependentScalars(c, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case yaml.ScalarNode:
		if eventDependenceRE.MatchString(n.Value) || workflowInputsRE.MatchString(n.Value) {
			*out = append(*out, fmt.Sprintf("%s: %q reads the event or the ref; a manual run and a push run of one commit must run the same steps", path, n.Value))
		}
	}
}

// eventDependenceWorkflowProblems returns what in a workflow text reads the
// event or the ref outside the allowed places.
func eventDependenceWorkflowProblems(t *testing.T, text string) []string {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(text), &doc))
	var out []string
	eventDependentScalars(&doc, "", &out)
	return out
}

// eventDependenceLines returns the numbers of the lines of a script or an
// action file that read the event or the ref.
func eventDependenceLines(text string) []int {
	var lines []int
	for i, line := range strings.Split(text, "\n") {
		if eventDependenceRE.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// eventDependenceFileProblems returns every line of every file under dirs that
// reads the event or the ref, except the files in allowed.
func eventDependenceFileProblems(t *testing.T, dirs []string, allowed map[string]string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if _, ok := allowed[filepath.ToSlash(path)]; ok {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range eventDependenceLines(string(data)) {
				out = append(out, fmt.Sprintf("%s:%d reads the event or the ref; a manual run and a push run of one commit must run the same code", filepath.ToSlash(path), line))
			}
			return nil
		}))
	}
	return out
}

// eventDependenceGoProblems returns every line of every Go file and every
// Makefile under root that reads the event or the ref, skipping the allowed
// directories and files and the skipped directories: the Go sources and
// tests the gates build and run, and the Makefile that dispatches them.
func eventDependenceGoProblems(t *testing.T, root string, allowedDirs, allowedFiles map[string]string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if _, ok := allowedDirs[rel]; ok || slices.Contains(eventDependenceSkippedDirs, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, ok := allowedFiles[rel]; ok || (d.Name() != "Makefile" && filepath.Ext(d.Name()) != ".go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range eventDependenceLines(string(data)) {
			out = append(out, fmt.Sprintf("%s:%d reads the event or the ref; a manual run and a push run of one commit must run the same code", rel, line))
		}
		return nil
	}))
	return out
}

// TestEveryJobAndStepOfCIIsEventIndependent asserts that nothing a job of
// ci.yaml runs reads the event or the ref that started the run, outside the
// reviewed places: the concurrency group, the reuse job and the if of the
// cache save steps. A push run reuses the result of a manual run of the same
// commit on the claim that the two differ only in the concurrency group and
// the cache saves; a step, an env value, an input or a script that branched on
// the event would make that claim false. The files of scripts/ and
// .github/actions, the Makefile and the Go files outside test/conformance (the
// sources and tests the gates build and run) are scanned for the same names.
func TestEveryJobAndStepOfCIIsEventIndependent(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(ciWorkflowPath)
	require.NoError(t, err)
	assert.Empty(t, eventDependenceWorkflowProblems(t, string(data)))
	assert.Empty(t, eventDependenceFileProblems(t, []string{"scripts", ".github/actions"}, eventDependenceAllowedFiles))
	assert.Empty(t, eventDependenceGoProblems(t, ".", eventDependenceAllowedDirs, eventDependenceAllowedFiles))

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(data, &doc))
	for _, path := range slices.Sorted(maps.Keys(eventDependenceAllowedPaths)) {
		assert.NotEmpty(t, strings.TrimSpace(eventDependenceAllowedPaths[path]), "the allowed path %s has no reason", path)
		top, rest, nested := strings.Cut(path, ".")
		_, found := doc[top]
		if nested {
			_, found = mapOf(doc[top])[rest]
		}
		assert.True(t, found, "the allowed path %s is not in ci.yaml; remove the entry", path)
	}
	assert.NotEmpty(t, cacheSaveIfReason)
	for dir, reason := range eventDependenceAllowedDirs {
		assert.NotEmpty(t, strings.TrimSpace(reason), "the allowed directory %s has no reason", dir)
		_, err := os.Stat(dir)
		require.NoError(t, err, "the allowed directory %s does not exist; remove the entry", dir)
	}
	for file, reason := range eventDependenceAllowedFiles {
		assert.NotEmpty(t, strings.TrimSpace(reason), "the allowed file %s has no reason", file)
		_, err := os.Stat(file)
		require.NoError(t, err, "the allowed file %s does not exist; remove the entry", file)
	}
}

const eventFixture = `
name: ci
concurrency:
  group: ${{ github.workflow }}-${{ github.event_name == 'push' && github.sha || github.ref }}
jobs:
  reuse:
    if: github.event_name == 'push'
    runs-on: ubuntu-latest
    steps:
      - run: test "$GITHUB_EVENT_NAME" = push
  e2e:
    runs-on: ubuntu-latest
    steps:
      - run: make e2e
      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9
        if: github.event_name == 'push' && github.ref == 'refs/heads/main'
        with:
          path: p
          key: ${{ github.sha }}
`

// TestEventDependenceRules asserts what the event independence rule accepts
// and refuses, over small workflow texts, scripts and action files.
func TestEventDependenceRules(t *testing.T) {
	t.Parallel()
	assert.Empty(t, eventDependenceWorkflowProblems(t, eventFixture), "the fixture before any edit")
	const e2eRun = "      - run: make e2e\n"
	for _, c := range []struct{ name, old, with string }{
		{"a push-only step", e2eRun, "      - if: github.event_name == 'push'\n        run: make e2e\n"},
		{"a step that skips a pull request", e2eRun, "      - if: ${{ github.event_name != 'pull_request' }}\n        run: make e2e\n"},
		{"a step env that reads the event", e2eRun, "      - env:\n          EVENT: ${{ github.event_name }}\n        run: make e2e\n"},
		{"a run body that reads GITHUB_EVENT_NAME", e2eRun, "      - run: test \"$GITHUB_EVENT_NAME\" = push || make e2e\n"},
		{"a run body that reads GITHUB_REF in braces", e2eRun, "      - run: echo \"${GITHUB_REF#refs/heads/}\"\n"},
		{"a run body that reads GITHUB_HEAD_REF", e2eRun, "      - run: echo \"$GITHUB_HEAD_REF\"\n"},
		{"a run body that reads the event payload", e2eRun, "      - run: echo ${{ github.event.pull_request.number }}\n"},
		{"an input that reads the head ref", e2eRun, "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1\n        with:\n          ref: ${{ github.head_ref }}\n"},
		{"a step that reads a workflow input", e2eRun, "      - if: ${{ inputs.skip == 'true' }}\n        run: make e2e\n"},
		{"a step that reads the event inputs", e2eRun, "      - if: github.event.inputs.skip != 'true'\n        run: make e2e\n"},
		{"a run body that reads an input in brackets", e2eRun, "      - run: echo ${{ inputs['skip'] }}\n"},
		{"a job if", "  e2e:\n    runs-on", "  e2e:\n    if: github.ref == 'refs/heads/main'\n    runs-on"},
		{"a job env", "  e2e:\n    runs-on: ubuntu-latest\n", "  e2e:\n    runs-on: ubuntu-latest\n    env:\n      BRANCH: ${{ github.ref_name }}\n"},
		{"a workflow env", "jobs:\n", "env:\n  EVENT: ${{ github.event_name }}\njobs:\n"},
		{"the whole github context", e2eRun, "      - run: echo '${{ toJSON(github) }}'\n"},
		{"the bracket form", e2eRun, "      - run: echo ${{ github['event_name'] }}\n"},
		{"another letter case", e2eRun, "      - if: GitHub.Event_Name == 'push'\n        run: make e2e\n"},
		{"a cache save step that reads the ref in its key", "          key: ${{ github.sha }}\n", "          key: ${{ github.ref }}\n"},
		{"a cache save step that reads the event in its path", "          path: p\n", "          path: ${{ github.event_name }}\n"},
		{"the reuse step in another job", e2eRun, "      - run: test \"$GITHUB_EVENT_NAME\" = push\n"},
	} {
		require.Contains(t, eventFixture, c.old, c.name)
		assert.NotEmpty(t, eventDependenceWorkflowProblems(t, strings.Replace(eventFixture, c.old, c.with, 1)), "must refuse: %s", c.name)
	}

	for _, text := range []string{
		"echo \"$GITHUB_REF\"\n", "case \"${GITHUB_EVENT_NAME:-}\" in\n", "x=${GITHUB_HEAD_REF}\n",
		"description: ${{ github.event_name }}\n", "value: ${{ github.ref }}\n", "# GITHUB_BASE_REF\n",
	} {
		assert.Equal(t, []int{1}, eventDependenceLines(text), "must refuse: %q", text)
	}
	for _, text := range []string{
		"echo \"$GITHUB_OUTPUT\" \"$GITHUB_SHA\" \"$GITHUB_REPOSITORY\" \"$GITHUB_WORKSPACE\"\n",
		"key: ${{ github.token }}-${{ github.sha }}-${{ runner.os }}\n", "echo ok\n",
		"the inputs. Then more\n", "cfg.inputs.x = 1\n", "if: inputs.lint-tools == 'true'\n",
	} {
		assert.Empty(t, eventDependenceLines(text), "must accept: %q", text)
	}

	dir := t.TempDir()
	scripts, actions := filepath.Join(dir, "scripts"), filepath.Join(dir, "actions", "x")
	require.NoError(t, os.MkdirAll(scripts, 0o755))
	require.NoError(t, os.MkdirAll(actions, 0o755))
	clean := filepath.Join(scripts, "clean")
	reads := filepath.Join(scripts, "reads")
	action := filepath.Join(actions, "action.yml")
	require.NoError(t, os.WriteFile(clean, []byte("echo ok\n"), 0o644))
	assert.Empty(t, eventDependenceFileProblems(t, []string{scripts}, nil), "a directory of clean files")
	require.NoError(t, os.WriteFile(reads, []byte("echo ok\necho \"$GITHUB_REF\"\n"), 0o644))
	require.NoError(t, os.WriteFile(action, []byte("runs:\n  steps:\n    - run: echo ${{ github.event_name }}\n"), 0o644))
	problems := eventDependenceFileProblems(t, []string{scripts, filepath.Join(dir, "actions")}, nil)
	assert.Len(t, problems, 2, "a script and an action file that read the event or the ref")
	assert.Empty(t, eventDependenceFileProblems(t, []string{scripts}, map[string]string{filepath.ToSlash(reads): "a reason"}), "an allowed file")

	tree := t.TempDir()
	write := func(rel, text string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(tree, rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tree, rel), []byte(text), 0o644))
	}
	write("internal/x/x.go", "package x\n\nvar a = 1\n")
	write("Makefile", "all:\n\t@echo ok\n")
	assert.Empty(t, eventDependenceGoProblems(t, tree, nil, nil), "a tree with clean Go files and Makefile")
	write("internal/x/ref.go", "package x\n\nvar b = os.Getenv(\"GITHUB_REF\")\n")
	write("cmd/y/y_test.go", "package y\n\nvar c = \"github.event_name\"\n")
	write("Makefile", "all:\n\t@echo $(GITHUB_EVENT_NAME)\n")
	write("test/conformance/z_test.go", "package z\n\nvar d = \"GITHUB_REF\"\n")
	write("docs/notes.go", "package docs\n\nvar e = \"GITHUB_REF\"\n")
	write("internal/x/notes.md", "GITHUB_REF\n")
	write("internal/x/testdata/t.go", "package t\n\nvar f = \"GITHUB_REF\"\n")
	got := eventDependenceGoProblems(t, tree, map[string]string{"test/conformance": "a reason"}, nil)
	assert.Len(t, got, 4, "a Go file, a test file, a testdata Go file and the Makefile read the event; an allowed directory, docs and a markdown file are not scanned: %v", got)
	got = eventDependenceGoProblems(t, tree, map[string]string{"test/conformance": "a reason"}, map[string]string{"Makefile": "a reason", "internal/x/ref.go": "a reason"})
	assert.Len(t, got, 2, "allowed files are not scanned: %v", got)
	assert.Len(t, eventDependenceGoProblems(t, tree, nil, nil), 5, "with no allowed directory the conformance file is scanned too")
}
