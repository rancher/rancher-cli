package conformance

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// e2eNoCgoBuild is the run body of the step that builds the module without
// cgo in the e2e job of ci.yaml.
const e2eNoCgoBuild = "go build ./..."

// e2eNoCgoBuildProblems returns what is wrong with the e2e job of ci.yaml
// when a job in readers restores the entry of e2e: the job must have exactly
// one step that runs go build, and that step must be the step whose body is
// e2eNoCgoBuild with the env CGO_ENABLED "0" and no other key besides name,
// placed after the step that runs make e2e and before the first
// actions/cache/save step. The builds of make e2e use cgo, so without the step
// the entry holds none of the builds the readers make without it.
func e2eNoCgoBuildProblems(files []cacheFile, readers map[string]goCacheReader) []string {
	var readerIDs []string
	for _, id := range slices.Sorted(maps.Keys(readers)) {
		if readers[id].source == "e2e" {
			readerIDs = append(readerIDs, id)
		}
	}
	if len(readerIDs) == 0 {
		return nil
	}
	label := ciWorkflowPath + " job e2e"
	prefix := fmt.Sprintf("%s: the jobs %s restore its cache entry, so it", label, strings.Join(readerIDs, ", "))
	for _, f := range files {
		if f.path != ciWorkflowPath {
			continue
		}
		job, ok := f.rawJobs["e2e"]
		if !ok {
			return []string{ciWorkflowPath + ": there is no job e2e"}
		}
		steps, _ := job["steps"].([]any)
		runOf := func(step any) string {
			m, _ := step.(map[string]any)
			run, _ := m["run"].(string)
			return strings.TrimSpace(run)
		}
		makeAt, saveAt := -1, len(steps)
		var builds []int
		for i, step := range steps {
			m, _ := step.(map[string]any)
			uses, _ := m["uses"].(string)
			switch run := runOf(step); {
			case run == "make e2e" && makeAt < 0:
				makeAt = i
			case strings.HasPrefix(run, "go build"):
				builds = append(builds, i)
			case usesName(uses) == "actions/cache/save" && saveAt == len(steps):
				saveAt = i
			}
		}
		if makeAt < 0 {
			return []string{prefix + " must have a step that runs make e2e"}
		}
		switch len(builds) {
		case 0:
			return []string{prefix + " must have a step that runs " + e2eNoCgoBuild + " with CGO_ENABLED \"0\""}
		case 1:
		default:
			return []string{prefix + " must have one step that runs go build, found more than one"}
		}
		want := map[string]any{"env": map[string]any{"CGO_ENABLED": "0"}, "run": e2eNoCgoBuild}
		got := maps.Clone(steps[builds[0]].(map[string]any))
		delete(got, "name")
		var out []string
		if !reflect.DeepEqual(got, want) {
			out = append(out, prefix+" must have a build step that is exactly run "+e2eNoCgoBuild+" with env CGO_ENABLED \"0\", with no if and no other key besides name")
		}
		if builds[0] < makeAt {
			out = append(out, prefix+" must run the build step after the make e2e step")
		}
		if builds[0] > saveAt {
			out = append(out, prefix+" must run the build step before the first actions/cache/save step")
		}
		return out
	}
	return []string{ciWorkflowPath + " is not among the files"}
}

// fxNoCgoStep is a step that runs body, with the lines of env under its env
// key (indented ten spaces, each ending in a newline) and extra lines
// (indented eight spaces) after the env.
func fxNoCgoStep(env, extra, body string) string {
	return "      - name: Build without cgo\n        env:\n" + env + extra + "        run: " + body + "\n"
}

// fxE2EWith is an e2e job that passes setup-go-key and runs the given steps
// after go-cache.
func fxE2EWith(steps ...string) string {
	all := []string{fxCheckout, fxSetupGo, fxGoCache("e2e", "          setup-go-key: \"true\"\n")}
	return fxJob("e2e", append(all, steps...)...)
}

// TestE2EJobBuildsWithoutCgoBeforeItSaves asserts that ci.yaml's e2e job has
// the step that builds the module without cgo, in the place that fills its
// cache entry for the jobs that restore it, and that the check accepts and
// refuses the cases of the table.
func TestE2EJobBuildsWithoutCgoBeforeItSaves(t *testing.T) {
	t.Parallel()
	assert.Empty(t, e2eNoCgoBuildProblems(readCacheFiles(t), goCacheReaders))

	const why = "a reason"
	readsE2E := map[string]goCacheReader{"fault-check": {"e2e", why}}
	readsOther := map[string]goCacheReader{"test-2": {"test-1", why}}
	ciOnly := func(jobs ...string) []cacheFile {
		return fxFiles(t, map[string]string{ciWorkflowPath: fxWorkflow("", jobs...)})
	}
	const cgoOff = "          CGO_ENABLED: \"0\"\n"
	step := fxNoCgoStep(cgoOff, "", e2eNoCgoBuild)
	makeE2E := fxRun("make e2e")
	save1, save2 := fxSave("e2e", false), fxSave("e2e", true)

	accept := []struct {
		name    string
		files   []cacheFile
		readers map[string]goCacheReader
	}{
		{"the step between make e2e and both saves", ciOnly(fxE2EWith(makeE2E, step, save1, save2)), readsE2E},
		{"no job reads the entry of e2e and the step is missing", ciOnly(fxE2EWith(makeE2E, save1, save2)), readsOther},
		{"no job reads the entry of e2e and there is no e2e job", ciOnly(fxStandardJob("test", "test")), readsOther},
	}
	for _, r := range accept {
		assert.Empty(t, e2eNoCgoBuildProblems(r.files, r.readers), "must accept: %s", r.name)
	}

	refuse := []struct {
		name  string
		files []cacheFile
		want  string
	}{
		{"the step is missing", ciOnly(fxE2EWith(makeE2E, save1, save2)), "must have a step that runs go build"},
		{"the step is after the first save", ciOnly(fxE2EWith(makeE2E, save1, step, save2)), "before the first actions/cache/save step"},
		{"the step is after both saves", ciOnly(fxE2EWith(makeE2E, save1, save2, step)), "before the first actions/cache/save step"},
		{"the step is before make e2e", ciOnly(fxE2EWith(step, makeE2E, save1, save2)), "after the make e2e step"},
		{"the step has an if", ciOnly(fxE2EWith(makeE2E, fxNoCgoStep(cgoOff, "        if: github.event_name == 'push'\n", e2eNoCgoBuild), save1, save2)), "must have a build step that is exactly"},
		{"the step has continue-on-error", ciOnly(fxE2EWith(makeE2E, fxNoCgoStep(cgoOff, "        continue-on-error: true\n", e2eNoCgoBuild), save1, save2)), "must have a build step that is exactly"},
		{"the step turns cgo on", ciOnly(fxE2EWith(makeE2E, fxNoCgoStep("          CGO_ENABLED: \"1\"\n", "", e2eNoCgoBuild), save1, save2)), "must have a build step that is exactly"},
		{"the step sets another variable", ciOnly(fxE2EWith(makeE2E, fxNoCgoStep(cgoOff+"          GOFLAGS: -mod=mod\n", "", e2eNoCgoBuild), save1, save2)), "must have a build step that is exactly"},
		{"the step builds one package", ciOnly(fxE2EWith(makeE2E, fxNoCgoStep(cgoOff, "", "go build ./cmd/rancher"), save1, save2)), "must have a build step that is exactly"},
		{"the step is there twice", ciOnly(fxE2EWith(makeE2E, step, step, save1, save2)), "found more than one"},
		{"there is no make e2e step", ciOnly(fxE2EWith(step, save1, save2)), "must have a step that runs make e2e"},
		{"there is no e2e job", ciOnly(fxStandardJob("test", "test")), "there is no job e2e"},
		{"ci.yaml is missing", fxFiles(t, map[string]string{".github/workflows/other.yaml": fxWorkflow("", fxStandardJob("test", "test"))}), "is not among the files"},
	}
	for _, r := range refuse {
		problems := e2eNoCgoBuildProblems(r.files, readsE2E)
		require.NotEmpty(t, problems, "must refuse: %s", r.name)
		assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, r.want) }),
			"must refuse: %s, with a problem naming %q, got %q", r.name, r.want, problems)
	}
}
