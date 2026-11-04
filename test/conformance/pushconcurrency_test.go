package conformance

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ciConcurrencyGroup is the concurrency group ci.yaml must use: a push run
// gets a group of its own commit, every other run shares the group of its
// ref.
const ciConcurrencyGroup = "${{ github.workflow }}-${{ github.event_name == 'push' && github.sha || github.ref }}"

// pushesToMain reports whether a workflow's on: block may start a run for
// a push to main. It errs toward yes: only a push filter that names tags
// alone, or branches that are all plain names other than main, says no. A
// branch glob or a negation may match main, so it counts as a push to main.
func pushesToMain(on any) bool {
	switch v := on.(type) {
	case string:
		return v == "push"
	case []any:
		return slices.Contains(v, any("push"))
	case map[string]any:
		push, ok := v["push"]
		if !ok {
			return false
		}
		filter, _ := push.(map[string]any)
		if filter == nil {
			return true
		}
		branches, hasBranches := filter["branches"]
		_, hasIgnore := filter["branches-ignore"]
		if !hasBranches {
			_, hasTags := filter["tags"]
			_, hasTagsIgnore := filter["tags-ignore"]
			return hasIgnore || (!hasTags && !hasTagsIgnore)
		}
		var names []any
		switch b := branches.(type) {
		case string:
			names = []any{b}
		case []any:
			names = b
		default:
			return true
		}
		for _, n := range names {
			name, _ := n.(string)
			if name == "main" || name == "" || strings.ContainsAny(name, "*?[]!+") {
				return true
			}
		}
		return false
	}
	return true
}

// shaGroupForms are the two ways a concurrency group may carry the commit
// of a push run: the SHA for every event, or the SHA when the event is a
// push.
var shaGroupForms = []string{"${{ github.sha }}", "github.event_name == 'push' && github.sha"}

// concurrencyProblems returns what is wrong with one concurrency block of a
// workflow that runs on a push to main. Every push run on main must finish
// and save its caches, so no push run may wait in a group that a later push
// can take: the group must carry the commit SHA of a push run, or the block
// must not exist.
func concurrencyProblems(where string, block any) []string {
	if block == nil {
		return nil
	}
	var group string
	switch v := block.(type) {
	case string:
		group = v
	case map[string]any:
		group, _ = v["group"].(string)
	}
	for _, form := range shaGroupForms {
		if strings.Contains(group, form) {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: the concurrency group %q does not carry the commit SHA of a push run, so a third push to main cancels the waiting push run", where, group)}
}

// pushConcurrencyProblems checks one workflow, given as a decoded map.
// ci.yaml must run on a push to main and use ciConcurrencyGroup exactly;
// every workflow that runs on a push to main follows concurrencyProblems,
// for its workflow block and each job's block.
func pushConcurrencyProblems(name string, wf map[string]any) []string {
	var out []string
	if name == filepath.Base(ciWorkflowPath) {
		if !pushesToMain(wf["on"]) {
			out = append(out, name+": must run on a push to main")
		}
		block, _ := wf["concurrency"].(map[string]any)
		if block == nil || block["group"] != ciConcurrencyGroup {
			out = append(out, fmt.Sprintf("%s: the concurrency group must be %s", name, ciConcurrencyGroup))
		}
	}
	if !pushesToMain(wf["on"]) {
		return out
	}
	out = append(out, concurrencyProblems(name, wf["concurrency"])...)
	jobs, _ := wf["jobs"].(map[string]any)
	for _, id := range slices.Sorted(maps.Keys(jobs)) {
		job, _ := jobs[id].(map[string]any)
		out = append(out, concurrencyProblems(name+" job "+id, job["concurrency"])...)
	}
	return out
}

func decodeWorkflowMap(t *testing.T, data []byte, name string) map[string]any {
	t.Helper()
	var wf map[string]any
	require.NoError(t, yaml.Unmarshal(data, &wf), name)
	return wf
}

// TestPushRunsOnMainNeverCancelEachOther asserts that every workflow in
// .github/workflows that runs on a push to main gives each push run a
// concurrency group of its own commit, or uses no group.
func TestPushRunsOnMainNeverCancelEachOther(t *testing.T) {
	dir := filepath.Join(".github", "workflows")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	pushing := 0
	for _, e := range entries {
		if ext := filepath.Ext(e.Name()); e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		wf := decodeWorkflowMap(t, data, e.Name())
		if pushesToMain(wf["on"]) {
			pushing++
		}
		assert.Empty(t, pushConcurrencyProblems(e.Name(), wf))
	}
	assert.GreaterOrEqual(t, pushing, 2, "ci.yaml and zizmor.yaml run on a push to main")
}

// TestPushConcurrencyProblemsFindsEveryWeakenedGroup asserts that the check
// fails on each change that would let push runs on main cancel each other.
func TestPushConcurrencyProblemsFindsEveryWeakenedGroup(t *testing.T) {
	ci := `on:
  push:
    branches: [main]
  pull_request:
concurrency:
  group: ` + ciConcurrencyGroup + `
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
jobs:
  a:
    runs-on: ubuntu-latest
`
	cases := []struct {
		name string
		file string
		src  string
		bad  bool
	}{
		{"ci.yaml as it is", "ci.yaml", ci, false},
		{"ci.yaml with the ref-only group", "ci.yaml", strings.Replace(ci, ciConcurrencyGroup, "${{ github.workflow }}-${{ github.ref }}", 1), true},
		{"ci.yaml with another group that carries the SHA", "ci.yaml", strings.Replace(ci, ciConcurrencyGroup, "ci-${{ github.sha }}", 1), true},
		{"ci.yaml with no concurrency block", "ci.yaml", strings.Replace(ci, "concurrency:\n  group: "+ciConcurrencyGroup+"\n  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n", "", 1), true},
		{"a push workflow with no concurrency block", "z.yaml", "on:\n  push:\n    branches: [main]\njobs:\n  a:\n    runs-on: ubuntu-latest\n", false},
		{"a push workflow with a ref-only group", "z.yaml", "on:\n  push:\n    branches: [main]\nconcurrency:\n  group: ${{ github.workflow }}-${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with a string group", "z.yaml", "on: push\nconcurrency: zizmor\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with a job-level ref-only group", "z.yaml", "on: [push, pull_request]\njobs:\n  a:\n    runs-on: ubuntu-latest\n    concurrency:\n      group: ${{ github.ref }}\n", true},
		{"a push workflow with a SHA group", "z.yaml", "on:\n  push:\nconcurrency:\n  group: z-${{ github.sha }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", false},
		{"a tag push workflow with a ref group", "r.yaml", "on:\n  push:\n    tags: [\"v*\"]\nconcurrency:\n  group: release-${{ github.ref_name }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", false},
		{"a push workflow for another branch", "o.yaml", "on:\n  push:\n    branches: [release]\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", false},
		{"ci.yaml without a push trigger", "ci.yaml", strings.Replace(ci, "  push:\n    branches: [main]\n", "", 1), true},
		{"a push workflow whose group has the SHA only for pull requests", "z.yaml", "on:\n  push:\nconcurrency:\n  group: ${{ github.event_name == 'pull_request' && github.sha || github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow naming main as a scalar", "z.yaml", "on:\n  push:\n    branches: main\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with a branch glob", "z.yaml", "on:\n  push:\n    branches: [\"**\"]\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with a branch glob and a negation", "z.yaml", "on:\n  push:\n    branches: [\"release*\", \"!release-old\"]\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with branches-ignore", "z.yaml", "on:\n  push:\n    branches-ignore: [release]\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a push workflow with a list trigger", "z.yaml", "on: [push]\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", true},
		{"a dispatch workflow with a ref group", "w.yaml", "on:\n  workflow_dispatch:\nconcurrency:\n  group: ${{ github.ref }}\njobs:\n  a:\n    runs-on: ubuntu-latest\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pushConcurrencyProblems(c.file, decodeWorkflowMap(t, []byte(c.src), c.name))
			if c.bad {
				assert.NotEmpty(t, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}
