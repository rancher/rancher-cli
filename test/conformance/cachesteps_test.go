package conformance

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
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
	goCacheAction      = "$/.github/actions/go-cache"
	goCacheActionFile  = ".github/actions/go-cache/action.yml"
	envtestPathLinux   = "~/.local/share/kubebuilder-envtest"
	envtestPathWindows = `~\AppData\Local\kubebuilder-envtest`
	setupGoKeyMessage  = "setup-go is used at a commit other than the one the setup-go line of .github/actions/go-cache/action.yml records. " +
		"Before changing that line, read src/cache-restore.ts of the new setup-go commit and confirm its primaryKey line matches the key go-cache builds; " +
		"then dispatch ci.yaml on the branch and confirm the e2e job's go-cache lookup finds the setup-go key"
)

// goCacheOutputs are the outputs of the go-cache action, the only ones a
// workflow may read.
var goCacheOutputs = []string{"key", "path", "cache-hit", "setup-go-key", "setup-go-cache-hit"}

// goCacheInputs are the inputs of the go-cache action, with their defaults.
var goCacheInputs = map[string]string{"name": "", "restore-from": "", "setup-go-key": "false"}

// goCacheReader is a job that restores the entry of another job: source is
// the go-cache name of that job.
type goCacheReader struct {
	source string
	reason string
}

// goCacheReaders lists the jobs of ci.yaml that restore the entry of another
// job through restore-from and never save, by ci.yaml job id (cross-compile
// for the matrix job; a copy in another workflow is listed under the id of
// the ci.yaml job it copies). Such a job is a reader. Every other job saves
// its own entry and passes no restore-from. The source of a reader is a job of
// the same workflow that saves, is no reader, runs on the same runner and has
// the same steps before checkout, so the entry holds the same cache and
// go.sum reads the same bytes.
var goCacheReaders = map[string]goCacheReader{
	"test-2":                  {"test-1", "the race shards share the dependency builds of internal/cli; each shard builds its own test variants"},
	"test-3":                  {"test-1", "the race shards share the dependency builds of internal/cli; each shard builds its own test variants"},
	"cachemodel":              {"e2e", "the package imports the standard library, slices and testify only, which the entry of e2e holds"},
	"cross-compile":           {"e2e", "builds one platform without cgo; the entry of e2e holds the modules and the standard library"},
	"fault-check":             {"e2e", "runs tests without the race detector on the build of e2e, and builds its own test variants"},
	"install-windows-install": {"install-windows-cli", "both run on the Windows runner with the default checkout, so go.sum hashes the same and the entry fits"},
}

// saveKeyKinds pair each key output a save step writes with the output that
// says whether an entry with that key exists.
var saveKeyKinds = []struct{ keyOutput, hitOutput string }{
	{"key", "cache-hit"},
	{"setup-go-key", "setup-go-cache-hit"},
}

// saveBranchCondition is the part of a save step's if that limits the save
// to the event and the ref that may write a cache. It takes the go-cache
// name so that one edit covers a job and its copy in another workflow.
func saveBranchCondition(_ string) string {
	return "github.event_name == 'push' && github.ref == 'refs/heads/main'"
}

// expectedSaveIf is the if of a save step of the job whose go-cache step has
// the given id and name, for the hit output paired with the saved key.
func expectedSaveIf(name, stepID, hitOutput string) string {
	return saveBranchCondition(name) + " && steps." + stepID + ".outputs." + hitOutput + " != 'true'"
}

// goCacheOutputExpr is the expression that reads one output of a go-cache
// step.
func goCacheOutputExpr(stepID, output string) string {
	return "${{ steps." + stepID + ".outputs." + output + " }}"
}

// cacheUsesAllowlist lists every action and reusable workflow the files may
// use, with the reason it holds no cache the rules below do not cover. A
// use that is not listed fails until it is added with a reason; a version
// change of a listed third-party action is a new review.
var cacheUsesAllowlist = map[string]string{
	"actions/checkout":                "checks out the repository; holds no cache",
	"actions/setup-go":                "its cache input must be false (setupGoCacheExceptions lists the two workflows that keep it)",
	"actions/cache/restore":           "only the two restore steps of go-cache, or a step on an envtest directory",
	"actions/cache/save":              "only a save step that passes the save rules",
	"actions/cache":                   "only on an envtest directory: envtest assets are not a Go build cache",
	"actions/upload-artifact":         "uploads files; holds no cache",
	"actions/attest-build-provenance": "builds an attestation; holds no cache step",
	"zizmorcore/zizmor-action":        "runs zizmor; holds no cache step",
	"rancher/renovate-config/.github/workflows/renovate-vault.yml": "a reusable workflow outside this repository that restores and saves its own renovate-cache entries, on schedule and dispatch",
	"$/.github/actions/setup-tools":                                "installs tools; holds no cache step",
	goCacheAction:                                                  "the action that restores the Go caches",
	"$/.github/workflows/zizmor.yaml":                              "runs zizmor; holds no cache step",
}

// cacheUsesPins records the commit of each third-party action the files use,
// as read when the allowlist was reviewed for any cache step. A change of a
// commit fails until a person reads the new version of the action for a
// cache step and updates the pin here.
var cacheUsesPins = map[string]string{
	"actions/checkout":                "3d3c42e5aac5ba805825da76410c181273ba90b1",
	"actions/setup-go":                "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
	"actions/cache":                   "55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
	"actions/cache/restore":           "55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
	"actions/cache/save":              "55cc8345863c7cc4c66a329aec7e433d2d1c52a9",
	"actions/upload-artifact":         "cf430e030ddbb5b0abf93d22962f4752f3646cd9",
	"actions/attest-build-provenance": "4d101475d8b20a2381f78447822ac1eab6504dd8",
	"zizmorcore/zizmor-action":        "53290badfa369cb0bf7ad16342cb62c2fa47ac7b",
	"rancher/renovate-config/.github/workflows/renovate-vault.yml": "25cb0e4e38b4931d4a551db492bf2cf47dac03f1",
}

// allowlistPinProblems returns the entries of the allowlist that are not a
// local use and have no recorded commit in pins, and the pins that name a
// use the allowlist does not list.
func allowlistPinProblems(allow, pins map[string]string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(allow)) {
		if _, ok := pins[name]; !ok && !strings.HasPrefix(name, "$/") {
			out = append(out, name+": a third-party use in the allowlist needs its commit in cacheUsesPins")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		if _, ok := allow[name]; !ok {
			out = append(out, name+": cacheUsesPins records a commit for a use the allowlist does not list")
		}
	}
	return out
}

// setupGoCacheExceptions lists the workflows whose setup-go keeps its own
// cache.
var setupGoCacheExceptions = map[string]string{
	".github/workflows/e2e-fidelity.yaml": "keeps the setup-go cache for the fidelity runs; the e2e job of ci.yaml saves the entry it reads",
	".github/workflows/fuzz.yaml":         "keeps the setup-go cache for the weekly fuzz run; the e2e job of ci.yaml saves the entry it reads",
}

// setupGoCacheExceptionEvents are the triggers the two exception workflows may
// have. Their setup-go post step saves the cache of the ref the run is for;
// a trigger that runs on the default branch for outside code
// (pull_request_target, workflow_run) would let that code write main's scope.
var setupGoCacheExceptionEvents = []string{"schedule", "workflow_dispatch"}

// cacheServiceWords are the spellings of a direct call to the cache service
// that no run body may hold.
var cacheServiceWords = []string{"gh cache", "/actions/caches", "ACTIONS_CACHE_URL", "ACTIONS_RESULTS_URL", "ACTIONS_RUNTIME_TOKEN"}

// runsGoRE matches a run body that names go or make as a word, or a path
// that starts with scripts/.
var runsGoRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])(?:go|make)(?:[^A-Za-z0-9_-]|$)|(?:^|[^A-Za-z0-9_.-])scripts/`)

// runsGo reports whether a run body runs the Go toolchain by the names
// above.
func runsGo(body string) bool {
	return runsGoRE.MatchString(body)
}

var (
	matrixExprRE    = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)
	stepOutputRE    = regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)`)
	stepIDRE        = regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.`)
	setupGoRecordRE = regexp.MustCompile(`actions/setup-go@([0-9a-f]{40})`)
)

// cacheFile is a workflow or a composite action read for the cache rules.
type cacheFile struct {
	path      string
	isAction  bool
	text      string
	workflow  cacheWorkflow
	rawJobs   map[string]map[string]any
	outputs   map[string]string
	inputs    map[string]string
	actionRun []workflowStep
}

type cacheWorkflow struct {
	On       yaml.Node              `yaml:"on"`
	Env      any                    `yaml:"env"`
	Defaults any                    `yaml:"defaults"`
	Jobs     map[string]workflowJob `yaml:"jobs"`
}

// stepUnit is the list of steps one rule pass reads: a job, or the steps of
// a composite action.
type stepUnit struct {
	label string
	job   *workflowJob
	steps []workflowStep
}

func parseCacheFile(filePath, text string) (cacheFile, error) {
	f := cacheFile{path: filePath, text: text}
	base := path.Base(filePath)
	f.isAction = strings.HasPrefix(filePath, ".github/actions/") && (base == "action.yml" || base == "action.yaml")
	if f.isAction {
		var action struct {
			Inputs map[string]struct {
				Default string `yaml:"default"`
			} `yaml:"inputs"`
			Outputs map[string]struct {
				Value string `yaml:"value"`
			} `yaml:"outputs"`
			Runs struct {
				Steps []workflowStep `yaml:"steps"`
			} `yaml:"runs"`
		}
		if err := yaml.Unmarshal([]byte(text), &action); err != nil {
			return f, fmt.Errorf("%s: %w", filePath, err)
		}
		f.inputs = map[string]string{}
		for name, in := range action.Inputs {
			f.inputs[name] = in.Default
		}
		f.outputs = map[string]string{}
		for name, o := range action.Outputs {
			f.outputs[name] = o.Value
		}
		f.actionRun = action.Runs.Steps
		return f, nil
	}
	if err := yaml.Unmarshal([]byte(text), &f.workflow); err != nil {
		return f, fmt.Errorf("%s: %w", filePath, err)
	}
	var raw struct {
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(text), &raw); err != nil {
		return f, fmt.Errorf("%s: %w", filePath, err)
	}
	f.rawJobs = raw.Jobs
	return f, nil
}

// units lists the step lists of the file, jobs in name order.
func (f cacheFile) units() []stepUnit {
	if f.isAction {
		return []stepUnit{{label: f.path, steps: f.actionRun}}
	}
	var out []stepUnit
	for _, id := range slices.Sorted(maps.Keys(f.workflow.Jobs)) {
		job := f.workflow.Jobs[id]
		out = append(out, stepUnit{label: f.path + " job " + id, job: &job, steps: job.Steps})
	}
	return out
}

// readCacheFiles reads every workflow and every composite action of the
// repository: the files directly in .github/workflows with the extension
// yml or yaml, and the files named action.yml or action.yaml at any depth
// below .github/actions.
func readCacheFiles(t *testing.T) []cacheFile {
	t.Helper()
	var paths []string
	entries, err := os.ReadDir(".github/workflows")
	require.NoError(t, err)
	for _, e := range entries {
		if ext := path.Ext(e.Name()); !e.IsDir() && (ext == ".yml" || ext == ".yaml") {
			paths = append(paths, ".github/workflows/"+e.Name())
		}
	}
	require.NoError(t, filepath.WalkDir(".github/actions", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (d.Name() == "action.yml" || d.Name() == "action.yaml") {
			paths = append(paths, filepath.ToSlash(p))
		}
		return nil
	}))
	var files []cacheFile
	for _, p := range paths {
		data, err := os.ReadFile(filepath.FromSlash(p))
		require.NoError(t, err)
		f, err := parseCacheFile(p, string(data))
		require.NoError(t, err)
		files = append(files, f)
	}
	return files
}

// usesName is a uses value in lower case without its ref.
func usesName(uses string) string {
	name, _, _ := strings.Cut(strings.ToLower(uses), "@")
	return name
}

func isEnvtestPath(p string) bool {
	return p == envtestPathLinux || p == envtestPathWindows
}

// releaseReachedFiles lists release.yaml and every file it reaches through
// a uses of the self-repository form, followed transitively: a local
// action or a reusable workflow.
func releaseReachedFiles(files []cacheFile) []cacheFile {
	byPath := map[string]cacheFile{}
	for _, f := range files {
		byPath[f.path] = f
	}
	var out []cacheFile
	seen := map[string]bool{}
	queue := []string{releaseWorkflowPath}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		f, ok := byPath[p]
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, f)
		for _, u := range f.units() {
			uses := []string{}
			if u.job != nil && u.job.Uses != "" {
				uses = append(uses, u.job.Uses)
			}
			for _, s := range u.steps {
				uses = append(uses, s.Uses)
			}
			for _, use := range uses {
				rel, ok := strings.CutPrefix(use, "$/")
				if !ok {
					continue
				}
				rel, _, _ = strings.Cut(rel, "@")
				if strings.HasPrefix(rel, ".github/actions/") {
					queue = append(queue, rel+"/action.yml", rel+"/action.yaml")
				} else {
					queue = append(queue, rel)
				}
			}
		}
	}
	return out
}

func isSaveStep(s workflowStep) bool { return usesName(s.Uses) == "actions/cache/save" }

// goCacheStep returns the go-cache step of a unit and its index, or -1.
func goCacheStep(u stepUnit) (workflowStep, int) {
	for i, s := range u.steps {
		if usesName(s.Uses) == goCacheAction {
			return s, i
		}
	}
	return workflowStep{}, -1
}

// onlyMainPushesSaveProblems returns what breaks the rules that keep every
// Go cache save to a push to main: the list of permitted uses, setup-go
// without its own cache, restores only where allowed, save steps with the
// expected if, path and key, saves after every other step, no direct call
// to the cache service in a run body, and no cache use at all in the files
// release.yaml reaches.
func onlyMainPushesSaveProblems(files []cacheFile) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	checkUse := func(f cacheFile, u stepUnit, where, uses string, with map[string]string) {
		name := usesName(uses)
		if _, ok := cacheUsesAllowlist[name]; !ok {
			add("%s %s: %q is not in the allowlist of uses; add it with a reason", u.label, where, uses)
			return
		}
		if pin, ok := cacheUsesPins[name]; ok {
			if _, ref, _ := strings.Cut(uses, "@"); ref != pin {
				add("%s %s: %q is at %q and cacheUsesPins records %q; read the new version of the action for a cache step, then update the pin", u.label, where, uses, ref, pin)
			}
		}
		switch name {
		case "actions/setup-go":
			if _, exempt := setupGoCacheExceptions[f.path]; !exempt && with["cache"] != "false" {
				add("%s %s: setup-go with.cache must be \"false\" outside the two exceptions, got %q", u.label, where, with["cache"])
			}
		case "actions/cache/restore":
			if f.path != goCacheActionFile && !isEnvtestPath(with["path"]) {
				add("%s %s: actions/cache/restore outside the go-cache action needs with.path equal to an envtest directory, got %q", u.label, where, with["path"])
			}
		case "actions/cache":
			if !isEnvtestPath(with["path"]) {
				add("%s %s: actions/cache is allowed only with with.path equal to an envtest directory, got %q", u.label, where, with["path"])
			}
		}
	}
	for _, f := range files {
		if _, exempt := setupGoCacheExceptions[f.path]; exempt {
			for _, event := range workflowEvents(f.workflow.On) {
				if !slices.Contains(setupGoCacheExceptionEvents, event) {
					add("%s: setup-go keeps its own cache here, so the workflow may run only on %q, and it runs on %q", f.path, setupGoCacheExceptionEvents, event)
				}
			}
		}
		for _, u := range f.units() {
			if u.job != nil && u.job.Uses != "" {
				checkUse(f, u, "uses", u.job.Uses, nil)
			}
			sawSave := false
			for i, s := range u.steps {
				where := fmt.Sprintf("step %d", i+1)
				if s.Uses != "" {
					checkUse(f, u, where, s.Uses, s.With)
				}
				for _, word := range cacheServiceWords {
					if strings.Contains(s.Run, word) {
						add("%s %s: a run body must not hold %q", u.label, where, word)
					}
				}
				if isSaveStep(s) {
					sawSave = true
					out = append(out, saveStepProblems(f, u, i)...)
				} else if sawSave {
					add("%s %s: no step may come after the first cache save step except another save step", u.label, where)
				}
			}
		}
	}
	for _, f := range releaseReachedFiles(files) {
		for _, u := range f.units() {
			uses := []string{}
			if u.job != nil && u.job.Uses != "" {
				uses = append(uses, u.job.Uses)
			}
			for _, s := range u.steps {
				uses = append(uses, s.Uses)
				if usesName(s.Uses) == "actions/setup-go" && s.With["cache"] != "false" {
					add("%s: setup-go in a file release.yaml reaches needs with.cache \"false\", got %q", u.label, s.With["cache"])
				}
			}
			for _, use := range uses {
				switch usesName(use) {
				case "actions/cache", "actions/cache/restore", "actions/cache/save", goCacheAction:
					add("%s: a file release.yaml reaches must not use %s: the release restores no cache", u.label, use)
				}
			}
		}
	}
	return out
}

// workflowEvents lists the events of the on key of a workflow, whichever
// of the three forms it is written in.
func workflowEvents(on yaml.Node) []string {
	var out []string
	switch on.Kind {
	case yaml.ScalarNode:
		out = append(out, on.Value)
	case yaml.SequenceNode:
		for _, n := range on.Content {
			out = append(out, n.Value)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			out = append(out, on.Content[i].Value)
		}
	}
	return out
}

// saveStepProblems checks one save step of a unit.
func saveStepProblems(f cacheFile, u stepUnit, i int) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	s := u.steps[i]
	where := fmt.Sprintf("%s step %d", u.label, i+1)
	if f.isAction {
		return []string{where + ": actions/cache/save is refused inside a composite action"}
	}
	if s.ContinueOnError.Kind != 0 {
		add("%s: a save step has no continue-on-error", where)
	}
	cacheStep, idx := goCacheStep(u)
	if idx < 0 {
		return append(out, where+": a save step needs a go-cache step in its job")
	}
	name := cacheStep.With["name"]
	matched := false
	for _, kind := range saveKeyKinds {
		if s.With["key"] != goCacheOutputExpr(cacheStep.ID, kind.keyOutput) {
			continue
		}
		matched = true
		if want := expectedSaveIf(name, cacheStep.ID, kind.hitOutput); strings.TrimSpace(s.If) != want {
			add("%s: the save if must equal an allowed expression, the one for key output %s is %q, got %q", where, kind.keyOutput, want, s.If)
		}
	}
	if !matched {
		add("%s: with.key must be a key output of the go-cache step (%s or %s), got %q", where,
			goCacheOutputExpr(cacheStep.ID, "key"), goCacheOutputExpr(cacheStep.ID, "setup-go-key"), s.With["key"])
	}
	if want := goCacheOutputExpr(cacheStep.ID, "path"); s.With["path"] != want {
		add("%s: with.path must be %q, got %q", where, want, s.With["path"])
	}
	return out
}

// matrixEntry is one include entry of a job's matrix, with its keys in file
// order.
type matrixEntry struct {
	keys   []string
	values map[string]string
}

// includeMatrix reads the matrix of a job. A job with no matrix returns no
// entries and no problem; a matrix that is not made of include entries
// returns a problem.
func includeMatrix(job workflowJob) (entries []matrixEntry, problem string) {
	if job.Strategy.Kind != yaml.MappingNode {
		return nil, ""
	}
	var matrix *yaml.Node
	for i := 0; i+1 < len(job.Strategy.Content); i += 2 {
		if job.Strategy.Content[i].Value == "matrix" {
			matrix = job.Strategy.Content[i+1]
		}
	}
	if matrix == nil {
		return nil, ""
	}
	const unsupported = "only a matrix made of include entries is supported"
	if matrix.Kind != yaml.MappingNode {
		return nil, unsupported
	}
	for i := 0; i+1 < len(matrix.Content); i += 2 {
		if matrix.Content[i].Value != "include" || matrix.Content[i+1].Kind != yaml.SequenceNode {
			return nil, unsupported
		}
		for _, item := range matrix.Content[i+1].Content {
			if item.Kind != yaml.MappingNode {
				return nil, unsupported
			}
			e := matrixEntry{values: map[string]string{}}
			for j := 0; j+1 < len(item.Content); j += 2 {
				e.keys = append(e.keys, item.Content[j].Value)
				e.values[item.Content[j].Value] = item.Content[j+1].Value
			}
			entries = append(entries, e)
		}
	}
	return entries, ""
}

// expectedGoCacheNames returns the names a job's go-cache step passes after
// each matrix entry is filled in, and the names ci.yaml requires: the job
// id, with each matrix value appended and joined with a hyphen.
func expectedGoCacheNames(id string, job workflowJob, passed string) (got, want []string, problem string) {
	entries, problem := includeMatrix(job)
	if problem != "" {
		return nil, nil, problem
	}
	if len(entries) == 0 {
		return []string{passed}, []string{id}, ""
	}
	for _, e := range entries {
		got = append(got, matrixExprRE.ReplaceAllStringFunc(passed, func(m string) string {
			return e.values[matrixExprRE.FindStringSubmatch(m)[1]]
		}))
		parts := []string{id}
		for _, k := range e.keys {
			parts = append(parts, e.values[k])
		}
		want = append(want, strings.Join(parts, "-"))
	}
	return got, want, ""
}

func hasSetupGo(steps []workflowStep) bool {
	return slices.ContainsFunc(steps, func(s workflowStep) bool { return usesName(s.Uses) == "actions/setup-go" })
}

// goCacheRefreshProblems returns what breaks the rules that make every Go
// cache refresh. In every job with a setup-go step, except the two
// workflows that keep setup-go's own cache and the files release.yaml
// reaches: one go-cache step after checkout and setup-go and before any step
// that runs go, save steps at the end that include the save of the
// go-cache key, the setup-go key input exactly when the job saves that
// key, and a go-cache name that matches the job (in ci.yaml) or a ci.yaml
// job it equals (in another workflow). A job listed in readers passes
// restore-from with its source and saves nothing, and its source is a job of
// the same workflow that saves; any other job passes no restore-from. Every
// key of readers is a job of ci.yaml and every source a go-cache name of one.
func goCacheRefreshProblems(files []cacheFile, readers map[string]goCacheReader) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	skip := map[string]bool{}
	for _, f := range releaseReachedFiles(files) {
		skip[f.path] = true
	}
	for p := range setupGoCacheExceptions {
		skip[p] = true
	}
	var ci cacheFile
	haveCI := false
	for _, f := range files {
		if f.path == ciWorkflowPath {
			ci, haveCI = f, true
		}
	}
	ciNames := map[string]string{}
	if haveCI {
		for id, job := range ci.workflow.Jobs {
			if _, want, problem := expectedGoCacheNames(id, job, ""); problem == "" {
				for _, name := range want {
					ciNames[name] = id
				}
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(readers)) {
		r := readers[id]
		if _, ok := ci.workflow.Jobs[id]; !haveCI || !ok {
			add("goCacheReaders lists job %q, which %s does not have", id, ciWorkflowPath)
		}
		if _, ok := ciNames[r.source]; !ok {
			add("goCacheReaders: the source %q of job %q is no go-cache name of a %s job", r.source, id, ciWorkflowPath)
		}
		if r.reason == "" {
			add("goCacheReaders: job %q needs a reason", id)
		}
	}
	readerSeen := map[string]bool{}
	for _, f := range files {
		if f.isAction || skip[f.path] {
			continue
		}
		usedBy := map[string]string{}
		var jobs []cacheJob
		for _, u := range f.units() {
			if !hasSetupGo(u.steps) {
				continue
			}
			id := strings.TrimPrefix(u.label, f.path+" job ")
			cacheStep, idx := goCacheStep(u)
			count := 0
			for _, s := range u.steps {
				if usesName(s.Uses) == goCacheAction {
					count++
				}
			}
			if count != 1 {
				add("%s: the job must use go-cache exactly once, found %d", u.label, count)
				continue
			}
			setupIdx, checkoutIdx := -1, -1
			for i, s := range u.steps {
				switch usesName(s.Uses) {
				case "actions/setup-go":
					if setupIdx < 0 {
						setupIdx = i
					}
				case "actions/checkout":
					if checkoutIdx < 0 {
						checkoutIdx = i
					}
				}
			}
			if checkoutIdx < 0 || idx < checkoutIdx || idx < setupIdx {
				add("%s: go-cache must come after checkout and setup-go", u.label)
			}
			for i := range idx {
				if usesName(u.steps[i].Uses) == "$/.github/actions/setup-tools" || runsGo(u.steps[i].Run) {
					add("%s: go-cache must come before any step that runs go, but step %d does", u.label, i+1)
				}
			}
			tableID := id
			names, _, problem := expectedGoCacheNames(id, *u.job, cacheStep.With["name"])
			if f.path != ciWorkflowPath {
				tableID = ""
				if problem == "" && len(names) > 0 {
					tableID = ciNames[names[0]]
				}
			}
			reader, listed := readers[tableID]
			if listed && f.path == ciWorkflowPath {
				readerSeen[tableID] = true
			}
			out = append(out, jobSaveProblems(u, cacheStep, idx, reader, listed)...)
			out = append(out, goCacheNameProblems(f, ci, haveCI, ciNames, id, *u.job, cacheStep, usedBy)...)
			if problem != "" {
				continue
			}
			jobs = append(jobs, cacheJob{
				label:          u.label,
				names:          names,
				restoreFrom:    cacheStep.With["restore-from"],
				saves:          slices.ContainsFunc(u.steps, isSaveStep),
				runsOn:         f.rawJobs[id]["runs-on"],
				jobSettings:    rawJobSettings(f.rawJobs[id]),
				beforeCheckout: rawStepsBefore(f.rawJobs[id], checkoutIdx),
			})
		}
		out = append(out, readerSourceProblems(jobs)...)
	}
	for _, id := range slices.Sorted(maps.Keys(readers)) {
		if _, ok := ci.workflow.Jobs[id]; haveCI && ok && !readerSeen[id] {
			add("goCacheReaders lists job %q, which uses no single go-cache step in %s", id, ciWorkflowPath)
		}
	}
	return out
}

// cacheJob is what the reader rules read of one job that uses go-cache.
type cacheJob struct {
	label          string
	names          []string
	restoreFrom    string
	saves          bool
	runsOn         any
	jobSettings    map[string]any
	beforeCheckout []any
}

// readerJobSettingKeys are the job keys besides runs-on that change the
// shell or the file system a job builds in: they must be equal in a reader
// and its source. A job env is left out: Go's build cache is keyed by the
// content of each build, so a reader with other env (fault-check runs its
// tests through an exec wrapper) restores a less useful entry, never a wrong
// one.
var readerJobSettingKeys = []string{"defaults", "container"}

// rawJobSettings returns the readerJobSettingKeys values of a raw job.
func rawJobSettings(job map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range readerJobSettingKeys {
		if v, ok := job[k]; ok {
			out[k] = v
		}
	}
	return out
}

// rawStepsBefore returns the steps of a raw job before the step at index
// end, as the plain values the file holds.
func rawStepsBefore(job map[string]any, end int) []any {
	steps, _ := job["steps"].([]any)
	if end < 0 || end > len(steps) {
		return nil
	}
	return steps[:end]
}

// readerSourceProblems checks the source of every job of one workflow that
// passes restore-from: a job of the workflow uses that go-cache name, saves,
// passes no restore-from itself, has the same runs-on value, the same
// defaults and container, and the same steps before checkout.
func readerSourceProblems(jobs []cacheJob) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	byName := map[string]cacheJob{}
	for _, j := range jobs {
		for _, name := range j.names {
			byName[name] = j
		}
	}
	for _, j := range jobs {
		if j.restoreFrom == "" {
			continue
		}
		src, ok := byName[j.restoreFrom]
		switch {
		case !ok:
			add("%s: restore-from %q is the go-cache name of no job of this workflow", j.label, j.restoreFrom)
			continue
		case src.restoreFrom != "":
			add("%s: restore-from %q names %s, which is itself a reader: the source of a reader is a job that saves", j.label, j.restoreFrom, src.label)
		case !src.saves:
			add("%s: restore-from %q names %s, which does not save", j.label, j.restoreFrom, src.label)
		}
		if !reflect.DeepEqual(j.runsOn, src.runsOn) {
			add("%s: runs-on %v differs from %v of %s, the job restore-from %q names", j.label, j.runsOn, src.runsOn, src.label, j.restoreFrom)
		}
		if !reflect.DeepEqual(j.jobSettings, src.jobSettings) {
			add("%s: the job defaults or container differ from those of %s, the job restore-from %q names", j.label, src.label, j.restoreFrom)
		}
		if !reflect.DeepEqual(j.beforeCheckout, src.beforeCheckout) {
			add("%s: the steps before checkout differ from those of %s, the job restore-from %q names: the checkout line endings, and so the go.sum hash, could differ", j.label, src.label, j.restoreFrom)
		}
	}
	return out
}

// jobSaveProblems checks the end of one job that uses go-cache: save steps
// after every other step that include the save of the go-cache key, and the
// setup-go key input paired with the save of that key. A job listed as a
// reader instead passes restore-from with its source and has no save step; a
// job not listed passes no restore-from.
func jobSaveProblems(u stepUnit, cacheStep workflowStep, idx int, reader goCacheReader, isReader bool) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	restoreFrom := cacheStep.With["restore-from"]
	switch {
	case isReader && restoreFrom != reader.source:
		add("%s: a job listed in goCacheReaders must pass restore-from %q to go-cache, got %q", u.label, reader.source, restoreFrom)
	case !isReader && restoreFrom != "":
		add("%s: the job is not in goCacheReaders and must not pass restore-from, got %q", u.label, restoreFrom)
	}
	if isReader && slices.ContainsFunc(u.steps, isSaveStep) {
		add("%s: a job listed in goCacheReaders must have no actions/cache/save step", u.label)
	}
	first := -1
	for i := idx + 1; i < len(u.steps); i++ {
		if isSaveStep(u.steps[i]) {
			if first < 0 {
				first = i
			}
		} else if first >= 0 {
			add("%s: step %d comes after a save step; the saves must be the last steps", u.label, i+1)
		}
	}
	saves := map[string]bool{}
	for _, s := range u.steps {
		if isSaveStep(s) {
			for _, kind := range saveKeyKinds {
				if s.With["key"] == goCacheOutputExpr(cacheStep.ID, kind.keyOutput) {
					saves[kind.keyOutput] = true
				}
			}
		}
	}
	if !isReader && !saves["key"] {
		add("%s: the saves at the end of the job must include the save whose key is outputs.key of the go-cache step", u.label)
	}
	if cacheStep.With["setup-go-key"] == "true" && (u.job == nil || u.job.RunsOn.Kind != yaml.ScalarNode || !strings.HasPrefix(u.job.RunsOn.Value, "ubuntu-")) {
		add("%s: setup-go-key \"true\" is for a job that runs on ubuntu: only a Linux job keeps the setup-go entry that fuzz.yaml and e2e-fidelity.yaml read, and a key saved from another system is one no workflow looks up", u.label)
	}
	if passes := cacheStep.With["setup-go-key"] == "true"; passes != saves["setup-go-key"] {
		add("%s: a job passes setup-go-key \"true\" to go-cache exactly when it has the save whose key is outputs.setup-go-key (input %v, save %v)", u.label, passes, saves["setup-go-key"])
	}
	return out
}

// goCacheNameProblems checks the name one job passes to go-cache.
func goCacheNameProblems(f, ci cacheFile, haveCI bool, ciNames map[string]string, id string, job workflowJob, cacheStep workflowStep, usedBy map[string]string) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	label := f.path + " job " + id
	passed := cacheStep.With["name"]
	got, want, problem := expectedGoCacheNames(id, job, passed)
	if problem != "" {
		return []string{label + ": " + problem}
	}
	for _, name := range got {
		if prev, ok := usedBy[name]; ok {
			add("%s: the go-cache name %q is also used by job %s of the same workflow", label, name, prev)
		}
		usedBy[name] = id
	}
	if f.path == ciWorkflowPath {
		if !slices.Equal(got, want) {
			add("%s: in ci.yaml the go-cache name must equal the job id with each matrix value appended, want %q, got %q", label, want, got)
		}
		return out
	}
	if !haveCI {
		return append(out, label+": the go-cache name must be the name of a ci.yaml job, and there is no ci.yaml")
	}
	ciIDs := map[string]bool{}
	for _, name := range got {
		ciID, ok := ciNames[name]
		if !ok {
			add("%s: in another workflow the go-cache name must be the name of a ci.yaml job, %q is none", label, name)
			continue
		}
		ciIDs[ciID] = true
	}
	for ciID := range ciIDs {
		if !reflect.DeepEqual(comparableJob(f.rawJobs[id]), comparableJob(ci.rawJobs[ciID])) {
			add("%s: a job in another workflow must equal the ci.yaml job %s its go-cache name names (compared without needs, if, timeout-minutes and permissions)", label, ciID)
		}
		if !reflect.DeepEqual(blockOrNil(f.workflow.Env), blockOrNil(ci.workflow.Env)) || !reflect.DeepEqual(blockOrNil(f.workflow.Defaults), blockOrNil(ci.workflow.Defaults)) {
			add("%s: the top-level env and defaults of %s must equal those of ci.yaml", label, f.path)
		}
	}
	return out
}

func comparableJob(raw map[string]any) map[string]any {
	c := maps.Clone(raw)
	for _, key := range []string{"needs", "if", "timeout-minutes", "permissions"} {
		delete(c, key)
	}
	return c
}

// blockOrNil treats an empty mapping as an absent one.
func blockOrNil(v any) any {
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil
	}
	return v
}

// ciE2EKeyProblems returns what is wrong with the e2e job of ci.yaml: it
// passes setup-go-key "true" and has both saves.
func ciE2EKeyProblems(files []cacheFile) []string {
	for _, f := range files {
		if f.path != ciWorkflowPath {
			continue
		}
		job, ok := f.workflow.Jobs["e2e"]
		if !ok {
			return []string{ciWorkflowPath + ": there is no job e2e"}
		}
		u := stepUnit{label: ciWorkflowPath + " job e2e", job: &job, steps: job.Steps}
		cacheStep, _ := goCacheStep(u)
		var out []string
		if cacheStep.With["setup-go-key"] != "true" {
			out = append(out, u.label+": ci.yaml's job e2e must pass setup-go-key \"true\" to go-cache")
		}
		saved := slices.ContainsFunc(u.steps, func(s workflowStep) bool {
			return isSaveStep(s) && s.With["key"] == goCacheOutputExpr(cacheStep.ID, "setup-go-key")
		})
		if !saved {
			out = append(out, u.label+": ci.yaml's job e2e must have the save whose key is outputs.setup-go-key")
		}
		return out
	}
	return []string{ciWorkflowPath + " is not among the files"}
}

// goCacheActionProblems returns what is wrong with the go-cache action
// file, when it is among the files: its inputs and their defaults, its two
// restore steps and their keys (the key and the restore keys name the job
// through restore-from or name, and no other line names it),
// the step that clears the test cache after both, run bodies with no
// expression and no write to the environment or the path, the five
// outputs with their expressions, and the step ids the expressions name.
// It also checks that every output a workflow reads from a go-cache step
// is one of the five.
func goCacheActionProblems(files []cacheFile) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	for _, f := range files {
		if f.isAction || f.path == goCacheActionFile {
			continue
		}
		for _, u := range f.units() {
			for _, s := range u.steps {
				if usesName(s.Uses) != goCacheAction || s.ID == "" {
					continue
				}
				for _, m := range stepOutputRE.FindAllStringSubmatch(f.text, -1) {
					if m[1] == s.ID && !slices.Contains(goCacheOutputs, m[2]) {
						add("%s reads steps.%s.outputs.%s, which go-cache does not declare", f.path, m[1], m[2])
					}
				}
			}
		}
	}
	var action cacheFile
	found := false
	for _, f := range files {
		if f.path == goCacheActionFile {
			action, found = f, true
		}
	}
	if !found {
		return out
	}
	steps := action.actionRun
	restoreIdx := map[string]int{}
	restores := 0
	clean := -1
	for i, s := range steps {
		switch {
		case usesName(s.Uses) == "actions/cache/restore":
			restores++
			restoreIdx[s.ID] = i
		case strings.TrimSpace(s.Run) == "go clean -testcache":
			clean = i
		}
		for _, bad := range []string{"${{", "GITHUB_ENV", "GITHUB_PATH"} {
			if strings.Contains(s.Run, bad) {
				add("%s step %d: no run body of go-cache's action file contains %s (an expression reaches a body only through env)", goCacheActionFile, i+1, bad)
			}
		}
	}
	if !reflect.DeepEqual(action.inputs, goCacheInputs) {
		add("%s: go-cache declares exactly the inputs and defaults %q, found %q", goCacheActionFile, goCacheInputs, action.inputs)
	}
	restoreStep, haveRestore := restoreIdx["restore"]
	lookupStep, haveLookup := restoreIdx["setup-go-lookup"]
	if restores != 2 || !haveRestore || !haveLookup {
		add("%s: the file must have exactly two actions/cache/restore steps, with ids restore and setup-go-lookup, found %d", goCacheActionFile, restores)
	}
	if haveLookup && steps[lookupStep].With["lookup-only"] != "true" {
		add("%s: the setup-go-lookup step must set lookup-only to true", goCacheActionFile)
	}
	const keyPrefix = "go-${{ inputs.restore-from || inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-"
	if haveRestore {
		r := steps[restoreIdx["restore"]]
		if !strings.HasSuffix(r.With["key"], "-${{ steps.paths.outputs.day }}") {
			add("%s: the restore key must end with the day output, got %q", goCacheActionFile, r.With["key"])
		}
		if want := keyPrefix + "${{ hashFiles('go.sum') }}-${{ steps.paths.outputs.day }}"; r.With["key"] != want {
			add("%s: the restore key must be %q, got %q", goCacheActionFile, want, r.With["key"])
		}
		wantKeys := []string{keyPrefix + "${{ hashFiles('go.sum') }}-", keyPrefix, "setup-go-${{ runner.os }}-"}
		var gotKeys []string
		for line := range strings.SplitSeq(strings.TrimSpace(r.With["restore-keys"]), "\n") {
			gotKeys = append(gotKeys, strings.TrimSpace(line))
		}
		if !slices.Equal(gotKeys, wantKeys) {
			add("%s: the restore step must have the three restore-keys in order, want %q, got %q", goCacheActionFile, wantKeys, gotKeys)
		}
	}
	switch {
	case clean < 0:
		add("%s: a step after both restore steps must run exactly go clean -testcache", goCacheActionFile)
	default:
		if clean < restoreStep || clean < lookupStep {
			add("%s: the step that runs go clean -testcache must come after both restore steps", goCacheActionFile)
		}
		c := steps[clean]
		if c.Shell != "bash" {
			add("%s: the clean step must set shell bash, got %q", goCacheActionFile, c.Shell)
		}
		if c.Env.Kind != 0 || c.WorkingDirectory != "" || c.If != "" || c.ContinueOnError.Kind != 0 {
			add("%s: the clean step has no env, working-directory, if or continue-on-error", goCacheActionFile)
		}
	}
	wantOutputs := map[string]string{
		"key":                "${{ steps.restore.outputs.cache-primary-key }}",
		"cache-hit":          "${{ steps.restore.outputs.cache-hit }}",
		"setup-go-key":       "${{ steps.setup-go-key.outputs.key }}",
		"setup-go-cache-hit": "${{ steps.setup-go-lookup.outputs.cache-hit }}",
	}
	if haveRestore {
		wantOutputs["path"] = strings.TrimSpace(steps[restoreStep].With["path"])
	}
	if got := slices.Sorted(maps.Keys(action.outputs)); !slices.Equal(got, slices.Sorted(slices.Values(goCacheOutputs))) {
		add("%s: go-cache declares exactly the outputs %q, found %q", goCacheActionFile, goCacheOutputs, got)
	}
	for name, want := range wantOutputs {
		if got := strings.TrimSpace(action.outputs[name]); got != want {
			add("%s: output %s must be %q, got %q", goCacheActionFile, name, want, got)
		}
	}
	ids := map[string]bool{}
	for _, s := range steps {
		if s.ID != "" {
			ids[s.ID] = true
		}
	}
	for _, m := range stepIDRE.FindAllStringSubmatch(action.text, -1) {
		if !ids[m[1]] {
			add("%s: an expression names step %q, which the file does not have", goCacheActionFile, m[1])
		}
	}
	return out
}

// setupGoKeyProblems returns what is wrong with the commit of setup-go
// that the go-cache action records: exactly one recorded commit, and every
// use of setup-go in every walked file naming it.
func setupGoKeyProblems(files []cacheFile) []string {
	var action cacheFile
	found := false
	for _, f := range files {
		if f.path == goCacheActionFile {
			action, found = f, true
		}
	}
	if !found {
		return []string{goCacheActionFile + " is missing"}
	}
	var recorded []string
	for line := range strings.SplitSeq(action.text, "\n") {
		if m := setupGoRecordRE.FindStringSubmatch(line); m != nil {
			recorded = append(recorded, m[1])
		}
	}
	if len(recorded) != 1 {
		return []string{fmt.Sprintf("%s must record the setup-go commit on exactly one line, found %d", goCacheActionFile, len(recorded))}
	}
	var out []string
	for _, f := range files {
		for _, u := range f.units() {
			for i, s := range u.steps {
				if usesName(s.Uses) != "actions/setup-go" {
					continue
				}
				if _, ref, _ := strings.Cut(s.Uses, "@"); ref != recorded[0] {
					out = append(out, fmt.Sprintf("%s step %d uses setup-go at %q, the recorded commit is %q: %s", u.label, i+1, ref, recorded[0], setupGoKeyMessage))
				}
			}
		}
	}
	return out
}

// TestOnlyMainPushesSaveCaches asserts that every use in every workflow and
// composite action is on the allowlist; that setup-go keeps no cache of its
// own outside the two listed workflows; that a restore or an actions/cache
// step appears only as go-cache's own restore steps or on an envtest
// directory; that a save step has the if of a push to main for the key it
// writes, the path and key outputs of the job's go-cache step and no
// continue-on-error, and is refused in a composite action; that save steps
// come last in every job; that no run body calls the cache service
// directly; and that the files release.yaml reaches use no cache and set
// the setup-go cache input to false.
func TestOnlyMainPushesSaveCaches(t *testing.T) {
	t.Parallel()
	files := readCacheFiles(t)
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.path] = true
	}
	for _, p := range []string{ciWorkflowPath, releaseWorkflowPath, goCacheActionFile, setupToolsFile} {
		require.True(t, paths[p], "%s is not among the files the cache rules read", p)
	}
	assert.Empty(t, onlyMainPushesSaveProblems(files))
	assert.Empty(t, allowlistPinProblems(cacheUsesAllowlist, cacheUsesPins))
}

// TestCIGoCachesRefresh asserts that every job with a setup-go step outside
// the two exception workflows and the files release.yaml reaches uses
// go-cache once, at the right place, with the saves and the name the rules
// give; that ci.yaml's e2e job also saves the setup-go key; and that the
// go-cache action file clears the test cache after both restores, holds no
// expression in a run body and declares the five outputs.
func TestCIGoCachesRefresh(t *testing.T) {
	t.Parallel()
	files := readCacheFiles(t)
	require.True(t, slices.ContainsFunc(files, func(f cacheFile) bool { return f.path == goCacheActionFile }), "%s is missing", goCacheActionFile)
	assert.Empty(t, goCacheRefreshProblems(files, goCacheReaders))
	assert.Empty(t, goCacheActionProblems(files))
	assert.Empty(t, ciE2EKeyProblems(files))
}

// TestSetupGoKeyFormatMatchesTheSetupGoVersion asserts that every use of
// setup-go in the workflows and actions, e2e-fidelity.yaml and fuzz.yaml
// included, names the commit that the go-cache action records as the one
// whose cache key format it copies.
func TestSetupGoKeyFormatMatchesTheSetupGoVersion(t *testing.T) {
	t.Parallel()
	assert.Empty(t, setupGoKeyProblems(readCacheFiles(t)))
}

const (
	fxCheckoutSHA = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	fxSetupGoSHA  = "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e"
	fxCacheSHA    = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"

	fxCheckout = "      - uses: actions/checkout@" + fxCheckoutSHA + "\n        with:\n          persist-credentials: false\n"
	fxSetupGo  = "      - uses: actions/setup-go@" + fxSetupGoSHA + "\n        with:\n          go-version-file: go.mod\n          cache: false\n"

	fxPlainSetupGo = "      - uses: actions/setup-go@" + fxSetupGoSHA + "\n        with:\n          go-version-file: go.mod\n"
	fxSetupTools   = "      - uses: $/.github/actions/setup-tools\n"

	// fxGoCacheAction is a go-cache action file that passes every rule.
	fxGoCacheAction = `name: go-cache
inputs:
  name:
    required: true
  restore-from:
    default: ""
  setup-go-key:
    default: "false"
outputs:
  key:
    value: ${{ steps.restore.outputs.cache-primary-key }}
  path:
    value: |
      ${{ steps.paths.outputs.gomodcache }}
      ${{ steps.paths.outputs.gocache }}
  cache-hit:
    value: ${{ steps.restore.outputs.cache-hit }}
  setup-go-key:
    value: ${{ steps.setup-go-key.outputs.key }}
  setup-go-cache-hit:
    value: ${{ steps.setup-go-lookup.outputs.cache-hit }}
runs:
  using: composite
  steps:
    - id: paths
      shell: bash
      run: |
        echo "gomodcache=$(go env GOMODCACHE)" >> "$GITHUB_OUTPUT"
    - id: restore
      uses: actions/cache/restore@` + fxCacheSHA + `
      with:
        path: |
          ${{ steps.paths.outputs.gomodcache }}
          ${{ steps.paths.outputs.gocache }}
        key: go-${{ inputs.restore-from || inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-${{ hashFiles('go.sum') }}-${{ steps.paths.outputs.day }}
        restore-keys: |
          go-${{ inputs.restore-from || inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-${{ hashFiles('go.sum') }}-
          go-${{ inputs.restore-from || inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-
          setup-go-${{ runner.os }}-
    # The key format of actions/setup-go@` + fxSetupGoSHA + `
    - id: setup-go-key
      if: inputs.setup-go-key == 'true'
      shell: bash
      env:
        GO_VERSION: ${{ steps.paths.outputs.goversion }}
      run: echo "key=setup-go-$GO_VERSION" >> "$GITHUB_OUTPUT"
    - id: setup-go-lookup
      if: inputs.setup-go-key == 'true'
      uses: actions/cache/restore@` + fxCacheSHA + `
      with:
        lookup-only: true
        path: |
          ${{ steps.paths.outputs.gomodcache }}
          ${{ steps.paths.outputs.gocache }}
        key: ${{ steps.setup-go-key.outputs.key }}
    - shell: bash
      run: go clean -testcache
`
)

func fxGoCache(name, extra string) string {
	return "      - id: go-cache\n        uses: $/.github/actions/go-cache\n        with:\n          name: " + name + "\n" + extra
}

// fxReader is a job that restores the entry of the go-cache name source and
// saves nothing, with extra steps after go-cache.
func fxReader(id, name, source string, extra ...string) string {
	steps := []string{fxCheckout, fxSetupGo, fxGoCache(name, "          restore-from: "+source+"\n")}
	steps = append(steps, extra...)
	steps = append(steps, fxRun("go test ./..."))
	return fxJob(id, steps...)
}

func fxRun(body string) string { return "      - run: " + body + "\n" }

// fxSaveIf is a save step with the given if, key and extra lines; an empty
// ifExpr leaves the if out.
func fxSaveIf(uses, ifExpr, key, extra string) string {
	out := "      - uses: " + uses + "@" + fxCacheSHA + "\n"
	if ifExpr != "" {
		out += "        if: " + ifExpr + "\n"
	}
	out += extra + "        with:\n          path: ${{ steps.go-cache.outputs.path }}\n          key: " + key + "\n"
	return out
}

// fxSave is the save step of the job whose go-cache name is name: the one
// for outputs.key, or with second the one for outputs.setup-go-key.
func fxSave(name string, second bool) string {
	kind := saveKeyKinds[0]
	if second {
		kind = saveKeyKinds[1]
	}
	return fxSaveIf("actions/cache/save", expectedSaveIf(name, "go-cache", kind.hitOutput), goCacheOutputExpr("go-cache", kind.keyOutput), "")
}

func fxJob(id string, steps ...string) string {
	return fxJobWith(id, "", steps...)
}

// fxJobWith is a job with extra lines (indented four spaces) before its
// steps.
func fxJobWith(id, header string, steps ...string) string {
	return "  " + id + ":\n    runs-on: ubuntu-latest\n" + header + "    steps:\n" + strings.Join(steps, "")
}

func fxWorkflow(top string, jobs ...string) string {
	return "name: x\non:\n  workflow_dispatch:\n" + top + "jobs:\n" + strings.Join(jobs, "")
}

// fxStandardJob is a job that passes every rule, with go-cache named name.
func fxStandardJob(id, name string, extra ...string) string {
	steps := []string{fxCheckout, fxSetupGo, fxGoCache(name, "")}
	steps = append(steps, extra...)
	steps = append(steps, fxRun("go test ./..."), fxSave(name, false))
	return fxJob(id, steps...)
}

const fxMatrix = "    strategy:\n      matrix:\n        include:\n          - { goos: linux, goarch: amd64 }\n          - { goos: darwin, goarch: arm64 }\n"

func fxE2E(withInput bool, saves ...string) string {
	extra := ""
	if withInput {
		extra = "          setup-go-key: \"true\"\n"
	}
	steps := []string{fxCheckout, fxSetupGo, fxGoCache("e2e", extra), fxRun("make e2e")}
	return fxJob("e2e", append(steps, saves...)...)
}

// fxFiles maps paths to texts, adding the go-cache action file that passes
// every rule unless the map holds one.
func fxFiles(t *testing.T, files map[string]string) []cacheFile {
	t.Helper()
	if _, ok := files[goCacheActionFile]; !ok {
		files[goCacheActionFile] = fxGoCacheAction
	}
	var out []cacheFile
	for _, p := range slices.Sorted(maps.Keys(files)) {
		f, err := parseCacheFile(p, files[p])
		require.NoError(t, err, p)
		out = append(out, f)
	}
	return out
}

// fxReplace replaces the one occurrence of old in text.
func fxReplace(t *testing.T, text, old, replacement string) string {
	t.Helper()
	require.Equal(t, 1, strings.Count(text, old), "%q must occur once in the fixture", old)
	return strings.Replace(text, old, replacement, 1)
}

func allCacheProblems(files []cacheFile, readers map[string]goCacheReader) []string {
	var out []string
	out = append(out, onlyMainPushesSaveProblems(files)...)
	out = append(out, goCacheRefreshProblems(files, readers)...)
	out = append(out, goCacheActionProblems(files)...)
	out = append(out, setupGoKeyProblems(files)...)
	return out
}

// TestCacheStepRules asserts what the cache rules accept and refuse, on
// inline workflows and actions that run the same checking functions as the
// real tree.
func TestCacheStepRules(t *testing.T) {
	t.Parallel()
	type row struct {
		name    string
		files   map[string]string
		readers map[string]goCacheReader
		wants   []string
	}
	const (
		ciCopy  = ".github/workflows/ci-windows.yaml"
		tempWF  = ".github/workflows/zz-mutation.yaml"
		fuzzWF  = ".github/workflows/fuzz.yaml"
		relWF   = releaseWorkflowPath
		nested  = ".github/actions/x/y/action.yaml"
		toolsAF = setupToolsFile
	)
	ciOnly := func(jobs ...string) map[string]string {
		return map[string]string{ciWorkflowPath: fxWorkflow("", jobs...)}
	}
	installWindows := fxStandardJob("install-windows", "install-windows")
	copyOf := func(name string, extraSteps ...string) string {
		steps := []string{fxCheckout, fxSetupGo, fxGoCache(name, "")}
		steps = append(steps, extraSteps...)
		steps = append(steps, fxRun("go test ./..."), fxSave("install-windows", false))
		return fxJob("install-windows-early", steps...)
	}
	withCopy := func(copy string, top string) map[string]string {
		return map[string]string{
			ciWorkflowPath: fxWorkflow("", installWindows),
			ciCopy:         fxWorkflow(top, copy),
		}
	}
	envtestLinux := "      - uses: actions/cache@" + fxCacheSHA + "\n        with:\n          path: " + envtestPathLinux + "\n          key: envtest\n"
	envtestWindows := "      - uses: actions/cache@" + fxCacheSHA + "\n        with:\n          path: " + envtestPathWindows + "\n          key: envtest\n"
	restoreAt := func(p string) string {
		return "      - uses: actions/cache/restore@" + fxCacheSHA + "\n        with:\n          path: " + p + "\n          key: x\n"
	}
	cacheActionWith := func(old, replacement string) map[string]string {
		return map[string]string{goCacheActionFile: fxReplace(t, fxGoCacheAction, old, replacement)}
	}
	savesBoth := []string{fxSave("e2e", false), fxSave("e2e", true)}
	const why = "a reason"
	readsTest1 := map[string]goCacheReader{"test-2": {"test-1", why}}
	windowsJob := func(job string) string { return strings.Replace(job, "ubuntu-latest", "windows-latest", 1) }
	cliJob := windowsJob(fxStandardJob("install-windows-cli", "install-windows-cli"))
	installReader := windowsJob(fxReader("install-windows-install", "install-windows-install", "install-windows-cli"))
	installReaders := map[string]goCacheReader{"install-windows-install": {"install-windows-cli", why}}
	earlyCopies := func(installRestoreFrom string) string {
		return fxWorkflow("",
			windowsJob(fxStandardJob("install-windows-cli-early", "install-windows-cli")),
			windowsJob(fxReader("install-windows-install-early", "install-windows-install", installRestoreFrom)))
	}

	accept := []row{
		{name: "two saves at the end of a job", files: ciOnly(fxE2E(true, savesBoth...))},
		{name: "a job whose git config step comes before checkout, with the Windows envtest step", files: ciOnly(fxJob("e2e-windows",
			fxRun("git config --global core.autocrlf false"), fxCheckout, fxSetupGo, fxGoCache("e2e-windows", ""), envtestWindows,
			fxRun("bash scripts/e2e"), fxSave("e2e-windows", false)))},
		{name: "a copy of a ci.yaml job in another workflow under another job id", files: withCopy(copyOf("install-windows"), "")},
		{name: "a Linux job with its own env and the envtest step", files: ciOnly(fxJobWith("fault-check", "    env:\n      GOFLAGS: -mod=mod\n",
			fxCheckout, fxSetupGo, fxGoCache("fault-check", ""), envtestLinux, fxRun("make e2e"), fxSave("fault-check", false)))},
		{name: "an envtest restore step on the Windows path", files: ciOnly(fxStandardJob("test", "test", restoreAt(envtestPathWindows)))},
		{name: "steps that do not run go before go-cache", files: ciOnly(fxJob("test",
			fxRun("echo going"),
			"      - name: CPU model\n        shell: pwsh\n        run: Get-CimInstance Win32_Processor | Select-Object -ExpandProperty Name\n",
			fxCheckout, fxSetupGo, fxGoCache("test", ""), fxRun("go test ./..."), fxSave("test", false)))},
		{name: "a matrix job named from its include entries", files: ciOnly(fxJobWith("cross-compile", fxMatrix,
			fxCheckout, fxSetupGo, fxGoCache("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", ""), fxRun("go build ./..."), fxSave("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", false)))},
		{name: "fuzz.yaml keeps setup-go's cache", files: map[string]string{
			fuzzWF: fxWorkflow("", fxJob("fuzz", fxCheckout, fxPlainSetupGo, fxRun("go test ./..."))),
		}},
		{name: "a reader of a job that saves", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1")), readers: readsTest1},
		{name: "readers of one job, one of them a matrix job", files: ciOnly(fxE2E(true, savesBoth...),
			fxReader("cachemodel", "cachemodel", "e2e"),
			fxJobWith("cross-compile", fxMatrix, fxCheckout, fxSetupGo,
				fxGoCache("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", "          restore-from: e2e\n"), fxRun("go build ./..."))),
			readers: map[string]goCacheReader{"cachemodel": {"e2e", why}, "cross-compile": {"e2e", why}}},
		{name: "a reader and its copy in another workflow", files: map[string]string{
			ciWorkflowPath: fxWorkflow("", cliJob, installReader),
			ciCopy:         earlyCopies("install-windows-cli"),
		}, readers: installReaders},
		{name: "a fuzz workflow on a schedule", files: map[string]string{
			fuzzWF: fxWorkflow("  schedule:\n    - cron: \"23 6 * * 1\"\n", fxJob("fuzz", fxCheckout, fxPlainSetupGo, fxRun("go test ./..."))),
		}},
	}

	refuse := []row{
		{name: "save with no if", files: ciOnly(fxJob("install-windows", fxCheckout, fxSetupGo, fxGoCache("install-windows", ""), fxRun("go test ./..."),
			fxSaveIf("actions/cache/save", "", "${{ steps.go-cache.outputs.key }}", ""))),
			wants: []string{"the save if must equal an allowed expression"}},
		{name: "save on every push", files: ciOnly(fxJob("install-windows", fxCheckout, fxSetupGo, fxGoCache("install-windows", ""), fxRun("go test ./..."),
			fxSaveIf("actions/cache/save", "github.event_name == 'push'", "${{ steps.go-cache.outputs.key }}", ""))),
			wants: []string{"the save if must equal an allowed expression"}},
		{name: "actions/cache on a Go build directory", files: ciOnly(fxStandardJob("test", "test",
			"      - uses: actions/cache@"+fxCacheSHA+"\n        with:\n          path: ~/.cache/go-build\n          key: x\n")),
			wants: []string{"actions/cache is allowed only with with.path equal to an envtest directory"}},
		{name: "an envtest step on another Windows path", files: ciOnly(fxStandardJob("test", "test",
			"      - uses: actions/cache@"+fxCacheSHA+"\n        with:\n          path: "+`~\AppData\Local\other`+"\n          key: x\n")),
			wants: []string{"actions/cache is allowed only with with.path equal to an envtest directory"}},
		{name: "release setup-go without cache false", files: map[string]string{
			relWF: fxWorkflow("", fxJob("validate", fxCheckout, fxPlainSetupGo, fxRun("go test ./..."))),
		}, wants: []string{"setup-go with.cache must be \"false\" outside the two exceptions", "setup-go in a file release.yaml reaches"}},
		{name: "job without the key save", files: ciOnly(fxJob("test", fxCheckout, fxSetupGo, fxGoCache("test", ""), fxRun("go test ./..."))),
			wants: []string{"the saves at the end of the job must include the save whose key is outputs.key"}},
		{name: "setup-go without cache false in a new workflow", files: map[string]string{
			ciWorkflowPath: fxWorkflow("", installWindows),
			tempWF:         fxWorkflow("", fxJob("x", fxCheckout, fxPlainSetupGo, fxGoCache("x", ""), fxRun("go test ./..."), fxSave("x", false))),
		}, wants: []string{"setup-go with.cache must be \"false\" outside the two exceptions"}},
		{name: "a cache restore in an action release.yaml reaches", files: map[string]string{
			relWF:   fxWorkflow("", fxJob("validate", fxCheckout, fxSetupGo, fxSetupTools, fxRun("go test ./..."))),
			toolsAF: "name: setup-tools\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache/restore@" + fxCacheSHA + "\n      with:\n        path: x\n        key: y\n",
		}, wants: []string{"a file release.yaml reaches must not use"}},
		{name: "no clean step", files: cacheActionWith("    - shell: bash\n      run: go clean -testcache\n", ""),
			wants: []string{"a step after both restore steps must run exactly go clean -testcache"}},
		{name: "a name that is another job's id", files: ciOnly(fxStandardJob("e2e", "test"), fxStandardJob("test", "test")),
			wants: []string{"in ci.yaml the go-cache name must equal the job id"}},
		{name: "a composite action that uses actions/cache on another path", files: map[string]string{
			nested: "name: x\nruns:\n  using: composite\n  steps:\n    - uses: actions/cache@" + fxCacheSHA + "\n      with:\n        path: x\n        key: y\n",
		}, wants: []string{"actions/cache is allowed only with with.path equal to an envtest directory"}},
		{name: "an action that is not on the allowlist", files: ciOnly(fxStandardJob("lint", "lint",
			"      - uses: golangci/golangci-lint-action@0000000000000000000000000000000000000000\n")),
			wants: []string{"is not in the allowlist of uses"}},
		{name: "a save with mixed-case uses and no if", files: ciOnly(fxJob("install-windows", fxCheckout, fxSetupGo, fxGoCache("install-windows", ""), fxRun("go test ./..."),
			fxSaveIf("Actions/Cache/Save", "", "${{ steps.go-cache.outputs.key }}", ""))),
			wants: []string{"the save if must equal an allowed expression"}},
		{name: "a run body that calls gh cache", files: ciOnly(fxStandardJob("lint", "lint", fxRun("gh cache delete --all"))),
			wants: []string{`a run body must not hold "gh cache"`}},
		{name: "go-cache after the tools install", files: ciOnly(fxJob("lint", fxCheckout, fxSetupGo, fxSetupTools, fxGoCache("lint", ""), fxRun("go vet ./..."), fxSave("lint", false))),
			wants: []string{"go-cache must come before any step that runs go"}},
		{name: "a save with a constant key", files: ciOnly(fxJob("test", fxCheckout, fxSetupGo, fxGoCache("test", ""), fxRun("go test ./..."),
			fxSaveIf("actions/cache/save", expectedSaveIf("test", "go-cache", "cache-hit"), "go-test-fixed", ""))),
			wants: []string{"with.key must be a key output of the go-cache step"}},
		{name: "a step between two saves", files: ciOnly(fxE2E(true, fxSave("e2e", false), fxRun("echo done"), fxSave("e2e", true))),
			wants: []string{"no step may come after the first cache save step"}},
		{name: "setup-go at another commit", files: map[string]string{
			fuzzWF: fxWorkflow("", fxJob("fuzz", fxCheckout, strings.Replace(fxPlainSetupGo, "b7ad1dad", "b7ad1dae", 1), fxRun("go test ./..."))),
		}, wants: []string{"uses setup-go at"}},
		{name: "the second save tests the first output", files: ciOnly(fxE2E(true, fxSave("e2e", false),
			fxSaveIf("actions/cache/save", expectedSaveIf("e2e", "go-cache", "cache-hit"), goCacheOutputExpr("go-cache", "setup-go-key"), ""))),
			wants: []string{"the save if must equal an allowed expression"}},
		{name: "a restore key without the day", files: cacheActionWith("-${{ hashFiles('go.sum') }}-${{ steps.paths.outputs.day }}\n", "-${{ hashFiles('go.sum') }}\n"),
			wants: []string{"the restore key must end with the day output"}},
		{name: "a job named test in another workflow with other steps", files: map[string]string{
			ciWorkflowPath: fxWorkflow("", fxStandardJob("test", "test")),
			tempWF:         fxWorkflow("", fxStandardJob("test", "test", fxRun("echo extra"))),
		}, wants: []string{"a job in another workflow must equal the ci.yaml job"}},
		{name: "an env on the clean step", files: cacheActionWith("    - shell: bash\n      run: go clean -testcache\n", "    - shell: bash\n      env:\n        GOCACHE: other\n      run: go clean -testcache\n"),
			wants: []string{"the clean step has no env"}},
		{name: "go-cache after the compile step of a matrix job", files: ciOnly(fxJobWith("cross-compile", fxMatrix,
			fxCheckout, fxSetupGo, fxRun("go build ./..."), fxGoCache("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", ""), fxSave("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", false))),
			wants: []string{"go-cache must come before any step that runs go"}},
		{name: "a copy with no go-cache step", files: withCopy(fxJob("install-windows-early", fxCheckout, fxSetupGo, fxRun("go test ./...")), ""),
			wants: []string{"the job must use go-cache exactly once"}},
		{name: "a copy with one more step", files: withCopy(copyOf("install-windows", fxRun("echo extra")), ""),
			wants: []string{"a job in another workflow must equal the ci.yaml job"}},
		{name: "a job with the default setup-go cache and no go-cache", files: ciOnly(fxJob("fault-check", fxCheckout, fxPlainSetupGo, fxRun("go test ./..."))),
			wants: []string{"setup-go with.cache must be \"false\" outside the two exceptions", "the job must use go-cache exactly once"}},
		{name: "a job that shares the e2e name", files: ciOnly(fxE2E(true, savesBoth...), fxStandardJob("fault-check", "e2e")),
			wants: []string{"in ci.yaml the go-cache name must equal the job id", "is also used by job"}},
		{name: "a copy with a job env", files: withCopy(strings.Replace(copyOf("install-windows"), "    steps:\n", "    env:\n      GOFLAGS: -race\n    steps:\n", 1), ""),
			wants: []string{"a job in another workflow must equal the ci.yaml job"}},
		{name: "a copy under a workflow with a top-level env", files: withCopy(copyOf("install-windows"), "env:\n  GOFLAGS: -race\n"),
			wants: []string{"the top-level env and defaults"}},
		{name: "a cartesian matrix", files: ciOnly(fxJobWith("cross-compile", "    strategy:\n      matrix:\n        goos: [linux, darwin]\n        goarch: [amd64, arm64]\n",
			fxCheckout, fxSetupGo, fxGoCache("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", ""), fxRun("go build ./..."), fxSave("cross-compile-${{ matrix.goos }}-${{ matrix.goarch }}", false))),
			wants: []string{"only a matrix made of include entries is supported"}},
		{name: "a copy that passes its own job id", files: withCopy(strings.Replace(copyOf("install-windows"), "name: install-windows\n", "name: install-windows-early\n", 1), ""),
			wants: []string{"must be the name of a ci.yaml job"}},
		{name: "a restore after go-cache on a Go build directory", files: ciOnly(fxStandardJob("test", "test", restoreAt("~/.cache/go-build"))),
			wants: []string{"actions/cache/restore outside the go-cache action needs with.path equal to an envtest directory"}},
		{name: "an expression in a run body", files: cacheActionWith("echo \"gomodcache=$(go env GOMODCACHE)\"", "echo \"${{ inputs.name }}\"; echo \"gomodcache=$(go env GOMODCACHE)\""),
			wants: []string{"contains ${{"}},
		{name: "an expression with no spaces in a run body", files: cacheActionWith("echo \"gomodcache=$(go env GOMODCACHE)\"", "echo \"${{inputs.name}}\"; echo \"gomodcache=$(go env GOMODCACHE)\""),
			wants: []string{"contains ${{"}},
		{name: "another context in a run body", files: cacheActionWith("echo \"gomodcache=$(go env GOMODCACHE)\"", "echo \"${{ github.event.head_commit.message }}\"; echo \"gomodcache=$(go env GOMODCACHE)\""),
			wants: []string{"contains ${{"}},
		{name: "a write to the environment file", files: cacheActionWith("echo \"gomodcache=$(go env GOMODCACHE)\"", "echo X=1 >> \"$GITHUB_ENV\"; echo \"gomodcache=$(go env GOMODCACHE)\""),
			wants: []string{"contains GITHUB_ENV"}},
		{name: "the e2e job without the input but with both saves", files: ciOnly(fxE2E(false, savesBoth...)),
			wants: []string{"exactly when it has the save whose key is outputs.setup-go-key"}},
		{name: "the e2e job with the input and one save", files: ciOnly(fxE2E(true, fxSave("e2e", false))),
			wants: []string{"exactly when it has the save whose key is outputs.setup-go-key"}},
		{name: "go-cache above setup-go", files: ciOnly(fxJob("lint", fxCheckout, fxGoCache("lint", ""), fxSetupGo, fxRun("go vet ./..."), fxSave("lint", false))),
			wants: []string{"go-cache must come after checkout and setup-go"}},
		{name: "a save in a composite action", files: map[string]string{
			nested: "name: x\nruns:\n  using: composite\n  steps:\n" + fxSaveIf("actions/cache/save", expectedSaveIf("x", "go-cache", "cache-hit"), "${{ steps.go-cache.outputs.key }}", ""),
		}, wants: []string{"actions/cache/save is refused inside a composite action"}},
		{name: "a save with continue-on-error", files: ciOnly(fxJob("install-windows", fxCheckout, fxSetupGo, fxGoCache("install-windows", ""), fxRun("go test ./..."),
			fxSaveIf("actions/cache/save", expectedSaveIf("install-windows", "go-cache", "cache-hit"), "${{ steps.go-cache.outputs.key }}", "        continue-on-error: true\n"))),
			wants: []string{"a save step has no continue-on-error"}},
		{name: "an output renamed", files: map[string]string{
			goCacheActionFile: strings.Replace(fxGoCacheAction, "  cache-hit:\n", "  hit:\n", 1),
			ciWorkflowPath:    fxWorkflow("", fxStandardJob("test", "test")),
		}, wants: []string{"go-cache declares exactly the outputs"}},
		{name: "the key output reads another output", files: cacheActionWith("steps.restore.outputs.cache-primary-key", "steps.restore.outputs.cache-matched-key"),
			wants: []string{"output key must be"}},
		{name: "a third restore step", files: cacheActionWith("    - shell: bash\n      run: go clean -testcache\n",
			"    - shell: bash\n      run: go clean -testcache\n    - uses: actions/cache/restore@"+fxCacheSHA+"\n      with:\n        path: x\n        key: y\n"),
			wants: []string{"exactly two actions/cache/restore steps"}},
		{name: "the clean step before the lookup step", files: map[string]string{
			goCacheActionFile: strings.Replace(strings.Replace(fxGoCacheAction, "    - shell: bash\n      run: go clean -testcache\n", "", 1),
				"    # The key format", "    - shell: bash\n      run: go clean -testcache\n    # The key format", 1),
		}, wants: []string{"must come after both restore steps"}},
		{name: "a restore key with no name", files: cacheActionWith("key: go-${{ inputs.restore-from || inputs.name }}-", "key: go-"),
			wants: []string{"the restore key must be"}},
		{name: "a restore key with another prefix", files: cacheActionWith("key: go-${{ inputs.restore-from || inputs.name }}-", "key: gx-${{ inputs.restore-from || inputs.name }}-"),
			wants: []string{"the restore key must be"}},
		{name: "a fuzz workflow that runs on pull_request_target", files: map[string]string{
			fuzzWF: fxWorkflow("  pull_request_target:\n", fxJob("fuzz", fxCheckout, fxPlainSetupGo, fxRun("go test ./..."))),
		}, wants: []string{"setup-go keeps its own cache here"}},
		{name: "the setup-go key on a Windows job", files: ciOnly(strings.Replace(fxE2E(true, savesBoth...), "ubuntu-latest", "windows-latest", 1)),
			wants: []string{"is for a job that runs on ubuntu"}},
		{name: "a reader with a save step", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1", fxSave("test-2", false))),
			readers: readsTest1, wants: []string{"must have no actions/cache/save step"}},
		{name: "a listed reader that passes no restore-from", files: ciOnly(fxStandardJob("test-1", "test-1"), fxStandardJob("test-2", "test-2")),
			readers: readsTest1, wants: []string{"must pass restore-from \"test-1\" to go-cache, got \"\""}},
		{name: "a listed reader that passes another source", files: ciOnly(fxStandardJob("test-1", "test-1"), fxStandardJob("lint", "lint"), fxReader("test-2", "test-2", "lint")),
			readers: readsTest1, wants: []string{"must pass restore-from \"test-1\" to go-cache, got \"lint\""}},
		{name: "a job that is not listed passes restore-from", files: ciOnly(fxStandardJob("test-1", "test-1"),
			fxJob("lint", fxCheckout, fxSetupGo, fxGoCache("lint", "          restore-from: test-1\n"), fxRun("go vet ./..."), fxSave("lint", false))),
			wants: []string{"the job is not in goCacheReaders and must not pass restore-from"}},
		{name: "a reader whose source is a reader", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1"), fxReader("test-3", "test-3", "test-2")),
			readers: map[string]goCacheReader{"test-2": {"test-1", why}, "test-3": {"test-2", why}}, wants: []string{"which is itself a reader"}},
		{name: "a reader whose source runs on another runner", files: ciOnly(fxE2E(true, savesBoth...), cliJob, fxReader("cachemodel", "cachemodel", "install-windows-cli")),
			readers: map[string]goCacheReader{"cachemodel": {"install-windows-cli", why}}, wants: []string{"differs from windows-latest"}},
		{name: "a reader with a step before checkout that its source lacks", files: map[string]string{
			ciWorkflowPath: fxWorkflow("", cliJob, strings.Replace(installReader, "    steps:\n", "    steps:\n"+fxRun("git config --global core.autocrlf false"), 1)),
		}, readers: installReaders, wants: []string{"the steps before checkout differ"}},
		{name: "a reader whose source does not save", files: ciOnly(fxE2E(true), fxReader("cachemodel", "cachemodel", "e2e")),
			readers: map[string]goCacheReader{"cachemodel": {"e2e", why}}, wants: []string{"which does not save"}},
		{name: "a reader of a name no job of its workflow uses", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1"), fxReader("cachemodel", "cachemodel", "e2e")),
			readers: map[string]goCacheReader{"test-2": {"test-1", why}, "cachemodel": {"e2e", why}}, wants: []string{"is the go-cache name of no job of this workflow"}},
		{name: "a reader and a copy with another restore-from", files: map[string]string{
			ciWorkflowPath: fxWorkflow("", cliJob, installReader),
			ciCopy:         earlyCopies("install-windows-config"),
		}, readers: installReaders, wants: []string{"a job in another workflow must equal the ci.yaml job"}},
		{name: "a readers table key that is no job", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1")),
			readers: map[string]goCacheReader{"test-2": {"test-1", why}, "test-9": {"test-1", why}}, wants: []string{`lists job "test-9", which`}},
		{name: "a readers table source that no job uses", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-8")),
			readers: map[string]goCacheReader{"test-2": {"test-8", why}}, wants: []string{`the source "test-8" of job "test-2" is no go-cache name`}},
		{name: "a readers table key whose job has no go-cache step", files: ciOnly(fxStandardJob("test-1", "test-1"), fxJob("test-2", fxCheckout, fxRun("echo test"))),
			readers: readsTest1, wants: []string{`lists job "test-2", which uses no single go-cache step`}},
		{name: "a reader whose job defaults differ from its source", files: ciOnly(fxStandardJob("test-1", "test-1"),
			strings.Replace(fxReader("test-2", "test-2", "test-1"), "    steps:\n", "    defaults:\n      run:\n        shell: sh\n    steps:\n", 1)),
			readers: readsTest1, wants: []string{"the job defaults or container differ"}},
		{name: "a reader whose job container differs from its source", files: ciOnly(fxStandardJob("test-1", "test-1"),
			strings.Replace(fxReader("test-2", "test-2", "test-1"), "    steps:\n", "    container: golang:1\n    steps:\n", 1)),
			readers: readsTest1, wants: []string{"the job defaults or container differ"}},
		{name: "a readers table row with no reason", files: ciOnly(fxStandardJob("test-1", "test-1"), fxReader("test-2", "test-2", "test-1")),
			readers: map[string]goCacheReader{"test-2": {"test-1", ""}}, wants: []string{`job "test-2" needs a reason`}},
		{name: "a readers table with no ci.yaml", files: map[string]string{fuzzWF: fxWorkflow("", fxJob("fuzz", fxCheckout, fxPlainSetupGo, fxRun("go test ./...")))},
			readers: readsTest1, wants: []string{`lists job "test-2", which`}},
		{name: "own-name restore keys beside the restore-from ones", files: cacheActionWith("          setup-go-${{ runner.os }}-\n",
			"          go-${{ inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-${{ hashFiles('go.sum') }}-\n"+
				"          go-${{ inputs.name }}-${{ runner.os }}-${{ runner.arch }}-${{ steps.paths.outputs.goversion }}-\n"+
				"          setup-go-${{ runner.os }}-\n"),
			wants: []string{"the restore step must have the three restore-keys in order"}},
		{name: "no restore-from input", files: cacheActionWith("  restore-from:\n    default: \"\"\n", ""),
			wants: []string{"go-cache declares exactly the inputs and defaults"}},
		{name: "a third-party action at another commit", files: ciOnly(fxJob("test", strings.Replace(fxCheckout, "3d3c42e5", "3d3c42e6", 1), fxSetupGo, fxGoCache("test", ""), fxRun("go test ./..."), fxSave("test", false))),
			wants: []string{"cacheUsesPins records"}},
	}

	for _, r := range accept {
		assert.Empty(t, allCacheProblems(fxFiles(t, maps.Clone(r.files)), r.readers), "must accept: %s", r.name)
	}
	for _, r := range refuse {
		problems := allCacheProblems(fxFiles(t, maps.Clone(r.files)), r.readers)
		require.NotEmpty(t, r.wants, r.name)
		for _, want := range r.wants {
			assert.True(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }),
				"must refuse: %s, with a problem naming %q, got %q", r.name, want, problems)
		}
	}

	assert.Empty(t, allowlistPinProblems(
		map[string]string{"a/b": "reason", "$/.github/actions/x": "local"}, map[string]string{"a/b": fxCacheSHA}),
		"a listed use with a pin and a local use with none")
	unpinned := allowlistPinProblems(map[string]string{"a/b": "reason", "c/d": "reason"}, map[string]string{"a/b": fxCacheSHA})
	assert.Equal(t, []string{"c/d: a third-party use in the allowlist needs its commit in cacheUsesPins"}, unpinned)
	stale := allowlistPinProblems(map[string]string{"a/b": "reason"}, map[string]string{"a/b": fxCacheSHA, "e/f": fxCacheSHA})
	assert.Equal(t, []string{"e/f: cacheUsesPins records a commit for a use the allowlist does not list"}, stale)

	e2eAccept := ciOnly(fxE2E(true, savesBoth...))
	assert.Empty(t, ciE2EKeyProblems(fxFiles(t, e2eAccept)), "the e2e job with the input and both saves")
	e2eRefuse := ciOnly(fxE2E(false, fxSave("e2e", false)))
	assert.NotEmpty(t, ciE2EKeyProblems(fxFiles(t, e2eRefuse)), "an e2e job with neither the input nor the second save")
	assert.NotEmpty(t, ciE2EKeyProblems(fxFiles(t, ciOnly(fxStandardJob("test", "test")))), "ci.yaml with no e2e job")

	message := setupGoKeyProblems(fxFiles(t, map[string]string{
		fuzzWF: fxWorkflow("", fxJob("fuzz", fxCheckout, strings.Replace(fxPlainSetupGo, "b7ad1dad", "b7ad1dae", 1), fxRun("go test ./..."))),
	}))
	require.NotEmpty(t, message)
	assert.Contains(t, message[0], goCacheActionFile, "the message names the file to edit")
	assert.Contains(t, message[0], "src/cache-restore.ts", "the message names the check to repeat")
	assert.Contains(t, message[0], "primaryKey", "the message names the line to compare")
}
