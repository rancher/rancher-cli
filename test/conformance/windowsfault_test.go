package conformance

import (
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	faultAllowlistPath = ".github/fault-allowlist"
	faultJobName       = "fault-check"
	faultCheckRun      = "./scripts/fault-check"
	faultCheckIf       = "${{ !cancelled() }}"
	faultTraceFlags    = "-exec=${{ github.workspace }}/scripts/fault-trace"
	faultLogDir        = "/tmp/fault-logs"
	e2eScriptPath      = "scripts/e2e"
	e2eScriptCommand   = "bash scripts/e2e"
	autocrlfCommand    = "git config --global core.autocrlf false"
	faultCPUCommand    = "Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name"
	faultInstallRun    = "sudo apt-get update\nsudo apt-get install -y --no-install-recommends strace\nstrace -V | sed -n 1p\nmkdir -p \"$FAULT_LOG_DIR\""
)

// faultStep is one step of a job with every key it carries; a key
// no rule allows is visible.
type faultStep map[string]yaml.Node

func (s faultStep) text(key string) string {
	n := s[key]
	return n.Value
}

func (s faultStep) keys() []string {
	return slices.Sorted(maps.Keys(s))
}

func (s faultStep) with() map[string]string {
	var with map[string]string
	n := s["with"]
	if n.Kind == 0 || n.Decode(&with) != nil {
		return nil
	}
	return with
}

// faultJob is one job with every key it carries and its decoded steps.
type faultJob struct {
	keys  map[string]yaml.Node
	steps []faultStep
}

func (j faultJob) text(key string) string {
	n := j.keys[key]
	return n.Value
}

func (j faultJob) env() map[string]string {
	var env map[string]string
	n := j.keys["env"]
	if n.Kind == 0 || n.Decode(&env) != nil {
		return nil
	}
	return env
}

func (j faultJob) keyNames() []string {
	return slices.Sorted(maps.Keys(j.keys))
}

// decodeFaultWorkflow decodes every job of a workflow file.
func decodeFaultWorkflow(data []byte) (map[string]faultJob, error) {
	var doc struct {
		Jobs map[string]map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	jobs := map[string]faultJob{}
	for name, keys := range doc.Jobs {
		job := faultJob{keys: keys}
		if n, ok := keys["steps"]; ok {
			if err := n.Decode(&job.steps); err != nil {
				return nil, err
			}
		}
		jobs[name] = job
	}
	return jobs, nil
}

func readFaultWorkflow(t *testing.T, file string) map[string]faultJob {
	t.Helper()
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	jobs, err := decodeFaultWorkflow(data)
	require.NoError(t, err, file)
	return jobs
}

func isWindowsRunner(j faultJob) bool {
	n := j.keys["runs-on"]
	return n.Kind == yaml.ScalarNode && strings.HasPrefix(strings.ToLower(n.Value), "windows")
}

// isTracedCommand reports whether a line of a run body is a command the
// fault-check job traces: a go test command or the e2e script.
func isTracedCommand(line string) bool {
	return (strings.HasPrefix(line, "go test ") || line == e2eScriptCommand) && !hasShellChain(line)
}

var quotedRE = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// hasShellChain reports whether a command line joins commands outside its
// quotes: a pattern such as -run 'A|B' is one command, a && b is two.
func hasShellChain(line string) bool {
	bare := quotedRE.ReplaceAllString(line, "")
	for _, op := range []string{"&&", "||", ";", "|", "`", "$("} {
		if strings.Contains(bare, op) {
			return true
		}
	}
	return false
}

func runLines(step faultStep) []string {
	var lines []string
	for line := range strings.SplitSeq(step.text("run"), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// withoutRace is a Windows job's go test command as the fault-check job
// runs it: the same text without the race detector.
func withoutRace(command string) string {
	return strings.ReplaceAll(command, " -race ", " ")
}

// windowsTracedCommands collects the commands the Windows jobs run that the
// fault-check job must trace, and the problems with every other line of
// their steps.
func windowsTracedCommands(jobs map[string]faultJob) (commands, problems []string) {
	for _, name := range slices.Sorted(maps.Keys(jobs)) {
		job := jobs[name]
		if !isWindowsRunner(job) {
			continue
		}
		for i, step := range job.steps {
			if step.text("uses") != "" {
				continue
			}
			for _, line := range runLines(step) {
				switch {
				case isTracedCommand(line):
					commands = append(commands, withoutRace(line))
				case line == autocrlfCommand || line == faultCPUCommand:
				case strings.Contains(line, "go test"):
					problems = append(problems, name+" step "+strconv.Itoa(i+1)+": write each go test as its own line, or add the command to the job "+faultJobName+" or to the list in this test: "+line)
				default:
					problems = append(problems, name+" step "+strconv.Itoa(i+1)+": add it to the job "+faultJobName+" or to the list in this test: "+line)
				}
			}
		}
	}
	return commands, problems
}

var (
	faultSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	faultJobKeySet  = []string{"env", "runs-on", "steps", "timeout-minutes"}
	faultStepKeySet = []string{"name", "run", "timeout-minutes"}
)

// faultCheckJobProblems returns what is wrong with the job fault-check
// against the Windows jobs of the same workflow: its traced commands, its
// job and step keys, its environment and the actions it uses.
func faultCheckJobProblems(jobs map[string]faultJob) []string {
	job, ok := jobs[faultJobName]
	if !ok {
		return []string{"the workflow has no job " + faultJobName}
	}
	var problems []string
	add := func(parts ...string) { problems = append(problems, strings.Join(parts, " ")) }
	list := func(v []string) string { return "[" + strings.Join(v, " ") + "]" }

	if got := job.keyNames(); !slices.Equal(got, faultJobKeySet) {
		add("the job keys must be exactly", list(faultJobKeySet), "got", list(got))
	}
	if job.text("runs-on") != "ubuntu-latest" {
		add("runs-on must be ubuntu-latest, got", job.text("runs-on"))
	}
	if job.text("timeout-minutes") != "20" {
		add("timeout-minutes must be 20, got", job.text("timeout-minutes"))
	}
	if env := job.env(); len(env) != 2 || env["GOFLAGS"] != faultTraceFlags || env["FAULT_LOG_DIR"] != faultLogDir {
		add("env must be exactly GOFLAGS", faultTraceFlags, "and FAULT_LOG_DIR", faultLogDir)
	}

	want, lineProblems := windowsTracedCommands(jobs)
	problems = append(problems, lineProblems...)

	var got []string
	checkIndex, lastTraced := -1, -1
	for i, step := range job.steps {
		run := strings.TrimSpace(step.text("run"))
		switch {
		case run == faultCheckRun:
			if checkIndex >= 0 {
				add("the step", faultCheckRun, "appears twice")
			}
			checkIndex = i
			if !slices.Equal(step.keys(), []string{"if", "name", "run"}) {
				add("the step", faultCheckRun, "must have exactly the keys name, if and run, got", list(step.keys()))
			}
			if step.text("if") != faultCheckIf {
				add("the step", faultCheckRun, "must run when a test fails: if", faultCheckIf+", got", step.text("if"))
			}
		case isTracedCommand(run) && !strings.Contains(run, "\n"):
			got = append(got, run)
			lastTraced = i
			for _, key := range step.keys() {
				if !slices.Contains(faultStepKeySet, key) {
					add("the traced step", run, "must not carry the key", key+": a step with it could run untraced and stay green")
				}
			}
			if strings.HasPrefix(run, "go test ") && !strings.Contains(run, "-bench") && !slices.Contains(strings.Fields(run), "-count=1") {
				add("the traced command", run, "needs -count=1: a cached result runs no test binary and leaves no log")
			}
		case strings.Contains(run, "go test") || strings.Contains(run, e2eScriptPath):
			add("write each traced command as the whole run of one step:", run)
		case run != "" && run != faultInstallRun:
			add("a step may run only a traced command, the strace install or", faultCheckRun+", because any other command can change what the traced commands run under:", run)
		case run == faultInstallRun:
			for _, key := range step.keys() {
				if !slices.Contains(faultStepKeySet, key) {
					add("the strace install step must not carry the key", key+": a step with it could change what the traced commands run under")
				}
			}
		}
	}
	for _, c := range want {
		if i := slices.Index(got, c); i >= 0 {
			got = slices.Delete(got, i, i+1)
		} else {
			add("the job is missing the traced command", c)
		}
	}
	for _, c := range got {
		add("the job traces a command no Windows job runs:", c)
	}
	switch {
	case checkIndex < 0:
		add("the job has no step", faultCheckRun)
	case checkIndex < lastTraced:
		add("the step", faultCheckRun, "must come after every traced command")
	}

	allowed := map[string]map[string]bool{}
	for name, other := range jobs {
		if name == faultJobName {
			continue
		}
		for _, step := range other.steps {
			if action, ref, ok := strings.Cut(step.text("uses"), "@"); ok {
				ref, _, _ = strings.Cut(ref, " ")
				if allowed[action] == nil {
					allowed[action] = map[string]bool{}
				}
				allowed[action][ref] = true
			}
		}
	}
	for i, step := range job.steps {
		n := strconv.Itoa(i + 1)
		uses := step.text("uses")
		isSave := strings.HasPrefix(uses, "actions/cache/save@")
		if step.text("if") != "" && i != checkIndex && (!isSave || checkIndex < 0 || i < checkIndex) {
			add("step", n, "must not carry an if: a step that can be skipped leaves tests untraced")
		}
		if _, has := step["continue-on-error"]; has {
			add("step", n, "must not carry continue-on-error")
		}
		if checkIndex >= 0 && i > checkIndex && !isSave {
			add("only cache save steps may follow", faultCheckRun+", got step", n)
		}
		if uses == "" || uses == goCacheAction {
			continue
		}
		action, ref, ok := strings.Cut(uses, "@")
		ref, _, _ = strings.Cut(ref, " ")
		lookup := action
		if action == "actions/cache/save" {
			lookup = "actions/cache"
		}
		if !ok || !faultSHAPattern.MatchString(ref) || !allowed[lookup][ref] {
			add("the action", uses, "must be pinned to a commit SHA that another job of the workflow uses for the same action")
		}
		if action == "actions/setup-go" {
			if cache, has := step.with()["cache"]; has && !otherSetupGoSetsCache(jobs, cache) {
				add("setup-go sets cache", cache+", which no other job does")
			}
		}
	}
	return problems
}

// otherSetupGoSetsCache reports whether a job other than fault-check passes
// the cache input to setup-go with the same value.
func otherSetupGoSetsCache(jobs map[string]faultJob, value string) bool {
	for name, job := range jobs {
		if name == faultJobName {
			continue
		}
		for _, step := range job.steps {
			if strings.HasPrefix(step.text("uses"), "actions/setup-go@") {
				if v, ok := step.with()["cache"]; ok && v == value {
					return true
				}
			}
		}
	}
	return false
}

// TestFaultCheckJobTracesEveryWindowsTest asserts that the job fault-check
// of ci.yaml runs, under scripts/fault-trace, every go test command and the
// e2e script that the Windows jobs run, as the same text with -race
// removed, each as the whole run of a step that cannot be skipped or run
// without the trace, and then runs scripts/fault-check whether or not a
// test failed. The only other command a step may run is the install of
// strace: a step that changed the environment of the later steps would
// leave them untraced.
func TestFaultCheckJobTracesEveryWindowsTest(t *testing.T) {
	t.Parallel()
	assert.Empty(t, faultCheckJobProblems(readFaultWorkflow(t, ciWorkflowPath)))
}

// TestFaultCheckJobRules asserts what the checker of the job fault-check
// accepts and refuses, from edits of ci.yaml itself.
func TestFaultCheckJobRules(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(ciWorkflowPath)
	require.NoError(t, err)
	base := string(data)
	edit := func(from, to string) string {
		t.Helper()
		require.Contains(t, base, from)
		return strings.Replace(base, from, to, 1)
	}
	const (
		configStep   = "      - run: go test -count=1 ./internal/config/\n"
		e2eStep      = "      - run: bash scripts/e2e\n"
		checkStep    = "        run: ./scripts/fault-check\n"
		checkoutUses = "      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"
		cacheSHA     = "55cc8345863c7cc4c66a329aec7e433d2d1c52a9"
	)
	const checkBlock = "      - name: Fault check\n        if: ${{ !cancelled() }}\n        run: ./scripts/fault-check\n"
	saveStep := "      - uses: actions/cache/save@" + cacheSHA + " # v6.1.0\n        if: ${{ github.event_name == 'push' }}\n        with:\n          path: ~/go/pkg/mod\n          key: fault-check\n"
	goCacheStep := "      - uses: " + goCacheAction + "\n        with:\n          name: fault-check\n"
	windowsJobEnd := e2eStep + "        timeout-minutes: 25\n"

	for _, c := range []struct {
		name, text, want string
	}{
		{"the workflow as it stands", base, ""},
		{"the job with the go cache action and a save step", edit(checkStep, checkStep+saveStep), ""},
		{"a copied step is missing", edit(configStep, ""), "missing the traced command go test -count=1 ./internal/config/"},
		{"the e2e script step is missing", edit(e2eStep, ""), "missing the traced command bash scripts/e2e"},
		{"a Windows job runs a go test the job does not", edit(windowsJobEnd, windowsJobEnd+"      - run: go test -count=1 ./internal/fsacl/ ./internal/testfs/\n"), "missing the traced command go test -count=1 ./internal/fsacl/ ./internal/testfs/"},
		{"a run pattern differs in one copy", edit("TestRenameOverAnOpenTarget|", "TestRenameOverAnOpenTargets|"), "missing the traced command"},
		{"the job has no GOFLAGS", edit("      GOFLAGS: -exec=${{ github.workspace }}/scripts/fault-trace\n", ""), "env must be exactly"},
		{"the job continues on error", edit("    timeout-minutes: 20\n    env:\n      GOFLAGS", "    timeout-minutes: 20\n    continue-on-error: true\n    env:\n      GOFLAGS"), "the job keys must be exactly"},
		{"a step continues on error", edit(configStep, configStep+"        continue-on-error: true\n"), "must not carry the key continue-on-error"},
		{"the check step has no if", edit("        if: ${{ !cancelled() }}\n", ""), "must have exactly the keys"},
		{"the check step comes before the e2e step", moveCheckStep(edit(e2eStep, checkBlock+e2eStep), checkBlock), "must come after every traced command"},
		{"a copy has the race flag again", edit(configStep, "      - run: go test -race -count=1 ./internal/config/\n"), "the job traces a command no Windows job runs"},
		{"a copy has a build tag the Windows command lacks", edit(configStep, "      - run: go test -tags envtest -count=1 ./internal/config/\n"), "missing the traced command go test -count=1 ./internal/config/"},
		{"a traced step sets its environment", edit(configStep, configStep+"        env:\n          GOFLAGS: \"\"\n"), "must not carry the key env"},
		{"a traced step sets its working directory", edit(configStep, configStep+"        working-directory: internal\n"), "must not carry the key working-directory"},
		{"a traced command has no count flag", edit(configStep, "      - run: go test ./internal/config/\n"), "needs -count=1"},
		{"a Windows step runs go test through pwsh", edit(windowsJobEnd, windowsJobEnd+"      - run: pwsh -Command \"go test ./internal/fsacl/\"\n"), "to the job fault-check or to the list in this test"},
		{"a Windows step chains two go test commands", edit(windowsJobEnd, windowsJobEnd+"      - run: go test -count=1 ./a/ && go test -count=1 ./b/\n"), "write each go test as its own line"},
		{"a Windows step runs a test binary", edit(windowsJobEnd, windowsJobEnd+"      - run: ./fsacl.test\n"), "to the job fault-check or to the list in this test"},
		{"an action is pinned to another commit", edit("      - name: Install strace\n", "      - uses: actions/checkout@0000000000000000000000000000000000000000 # v7.0.1\n      - name: Install strace\n"), "must be pinned to a commit SHA"},
		{"a step after the check step is not a cache save", edit(checkStep, checkStep+"      - run: echo done\n"), "only cache save steps may follow"},
		{"a save step before the check step carries an if", edit("      - name: Install strace\n", saveStep+"      - name: Install strace\n"), "must not carry an if"},
		{"setup-go sets a cache input the other jobs do not", edit("      - name: Install strace\n", "      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0\n        with:\n          cache: true\n      - name: Install strace\n"), "setup-go sets cache true"},
		{"a step writes to the environment of the later steps", edit(e2eStep, "      - run: echo \"GOFLAGS=\" >> \"$GITHUB_ENV\"\n"+e2eStep), "a step may run only a traced command, the strace install or"},
		{"a step runs another command", edit(e2eStep, "      - run: echo done\n"+e2eStep), "a step may run only a traced command, the strace install or"},
		{"the strace install step has a shell template", edit("      - name: Install strace\n        run: |\n", "      - name: Install strace\n        shell: sh -c 'echo GOFLAGS= >> \"$GITHUB_ENV\"' {0}\n        run: |\n"), "the strace install step must not carry the key shell"},
		{"the strace install step sets its environment", edit("      - name: Install strace\n        run: |\n", "      - name: Install strace\n        env:\n          GOFLAGS: \"\"\n        run: |\n"), "the strace install step must not carry the key env"},
		{"the strace install step changes", edit("          mkdir -p \"$FAULT_LOG_DIR\"\n", "          mkdir -p \"$FAULT_LOG_DIR\"\n          export GOFLAGS=\n"), "a step may run only a traced command, the strace install or"},
		{"the go cache action is accepted before the first test", edit("      - name: Install strace\n", goCacheStep+"      - name: Install strace\n"), ""},
	} {
		problems := faultCheckJobProblems(decodeFaultText(t, c.text))
		if c.want == "" {
			assert.Empty(t, problems, c.name)
			continue
		}
		require.NotEmpty(t, problems, c.name)
		assert.Contains(t, strings.Join(problems, "\n"), c.want, c.name)
	}
}

// moveCheckStep removes the last copy of block from text.
func moveCheckStep(text, block string) string {
	i := strings.LastIndex(text, block)
	return text[:i] + text[i+len(block):]
}

func decodeFaultText(t *testing.T, text string) map[string]faultJob {
	t.Helper()
	jobs, err := decodeFaultWorkflow([]byte(text))
	require.NoError(t, err)
	return jobs
}

// faultRow is one row of the allowlist.
type faultRow struct {
	pkg, program, count, reason string
}

// parseFaultAllowlist reads the rows of the allowlist: package|program|
// count|reason, with blank lines and lines that start with # ignored.
func parseFaultAllowlist(text string) (rows []faultRow, problems []string) {
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		where := faultAllowlistPath + ":" + strconv.Itoa(i+1) + ": "
		if len(fields) != 4 {
			problems = append(problems, where+"want four fields separated by |: package|program|count|reason")
			continue
		}
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}
		rows = append(rows, faultRow{fields[0], fields[1], fields[2], fields[3]})
	}
	return rows, problems
}

// faultAllowlistProblems returns what is wrong with the rows. packages are
// the directories the traced commands cover.
func faultAllowlistProblems(rows []faultRow, packages []string) []string {
	var problems []string
	seen := map[[2]string]bool{}
	for _, r := range rows {
		who := r.pkg + " " + r.program + ": "
		switch {
		case !slices.Contains(packages, r.pkg):
			problems = append(problems, who+"no traced command covers the package")
		case r.program == "" || strings.Contains(r.program, "/"):
			problems = append(problems, who+"the program is a non-empty base name that holds no /")
		}
		if n, err := strconv.Atoi(r.count); r.count != "any" && (err != nil || n < 1 || r.count != strconv.Itoa(n)) {
			problems = append(problems, who+"the count is a positive integer or any")
		}
		if r.count == "any" && (strings.HasSuffix(r.program, ".test") || strings.HasPrefix(r.program, "rancher")) {
			problems = append(problems, who+"a program built from this repository takes an exact count, never any")
		}
		if r.reason == "" || strings.Contains(r.reason, "#") {
			problems = append(problems, who+"the reason is one non-empty line without #")
		}
		key := [2]string{r.pkg, r.program}
		if seen[key] {
			problems = append(problems, who+"a second row for the same package and program")
		}
		seen[key] = true
	}
	return problems
}

// faultCoveredPackages lists the directories the traced commands cover: the
// package arguments of the go test commands and of scripts/e2e, with a
// trailing /... expanded to every directory below.
func faultCoveredPackages(t *testing.T, jobs map[string]faultJob) []string {
	t.Helper()
	commands, _ := windowsTracedCommands(jobs)
	script, err := os.ReadFile(e2eScriptPath)
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(script), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "go test ") {
			commands = append(commands, line)
		}
	}
	var dirs []string
	for _, command := range commands {
		for arg := range strings.FieldsSeq(command) {
			rest, ok := strings.CutPrefix(arg, "./")
			if !ok {
				continue
			}
			if base, deep := strings.CutSuffix(rest, "/..."); deep {
				require.NoError(t, filepath.WalkDir(filepath.FromSlash(base), func(p string, d fs.DirEntry, err error) error {
					if err == nil && d.IsDir() {
						dirs = append(dirs, filepath.ToSlash(p))
					}
					return err
				}))
				continue
			}
			dir := path.Clean(rest)
			info, err := os.Stat(filepath.FromSlash(dir))
			require.NoError(t, err, "%s names a package that does not exist", arg)
			require.True(t, info.IsDir(), arg)
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// TestFaultAllowlistRowsAreComplete asserts that every row of
// .github/fault-allowlist has four fields, names a package a traced command
// covers, a program that is a base name, a positive count or any (never any
// for a program built from this repository) and a reason, and that no
// package and program has two rows.
func TestFaultAllowlistRowsAreComplete(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(faultAllowlistPath)
	require.NoError(t, err)
	rows, problems := parseFaultAllowlist(string(data))
	assert.Empty(t, problems)
	packages := faultCoveredPackages(t, readFaultWorkflow(t, ciWorkflowPath))
	require.NotEmpty(t, packages)
	assert.Empty(t, faultAllowlistProblems(rows, packages))
}

// TestFaultAllowlistRules asserts what the allowlist checker accepts and
// refuses.
func TestFaultAllowlistRules(t *testing.T) {
	t.Parallel()
	packages := []string{"internal/fsacl", "internal/commands/telemetrycmd", "test/e2e", "test/e2e/fidelity"}
	for _, c := range []struct {
		name string
		row  faultRow
		want string
	}{
		{"an exact count on a test binary", faultRow{"internal/fsacl", "fsacl.test", "2", "a reason"}, ""},
		{"any on a program this repository does not build", faultRow{"internal/fsacl", "pwsh", "any", "a reason"}, ""},
		{"a row with an empty reason", faultRow{"internal/fsacl", "pwsh", "any", ""}, "the reason"},
		{"a row for a package no traced command covers", faultRow{"internal/other", "pwsh", "any", "a reason"}, "no traced command covers"},
		{"any on a provider test binary", faultRow{"internal/fsacl", "provider.test", "any", "a reason"}, "never any"},
		{"any on a long test binary name", faultRow{"internal/commands/telemetrycmd", "telemetrycmd.test", "any", "a reason"}, "never any"},
		{"any on a test binary of a nested package", faultRow{"test/e2e", "fidelity.test", "any", "a reason"}, "never any"},
		{"any on the command line program", faultRow{"internal/fsacl", "rancher", "any", "a reason"}, "never any"},
		{"a program name that holds a slash", faultRow{"internal/fsacl", "bin/pwsh", "any", "a reason"}, "base name"},
		{"a count of zero", faultRow{"internal/fsacl", "pwsh", "0", "a reason"}, "positive integer"},
		{"a count that is not a number", faultRow{"internal/fsacl", "pwsh", "many", "a reason"}, "positive integer"},
		{"a reason with a hash", faultRow{"internal/fsacl", "pwsh", "any", "see #12"}, "the reason"},
	} {
		problems := faultAllowlistProblems([]faultRow{c.row}, packages)
		if c.want == "" {
			assert.Empty(t, problems, c.name)
			continue
		}
		require.NotEmpty(t, problems, c.name)
		assert.Contains(t, strings.Join(problems, "\n"), c.want, c.name)
	}

	twice := []faultRow{{"internal/fsacl", "pwsh", "any", "a"}, {"internal/fsacl", "pwsh", "3", "b"}}
	assert.Contains(t, strings.Join(faultAllowlistProblems(twice, packages), "\n"), "a second row", "two rows for one package and program")

	rows, problems := parseFaultAllowlist("# comment\n\ninternal/fsacl|pwsh|any|a reason\n")
	assert.Empty(t, problems)
	assert.Equal(t, []faultRow{{"internal/fsacl", "pwsh", "any", "a reason"}}, rows)
	_, problems = parseFaultAllowlist("internal/fsacl|pwsh|any\ninternal/fsacl|pwsh|any|a|b\n")
	assert.Len(t, problems, 2, "a row with three fields and a row with five")
}

const faultCPUShell = "pwsh"

// windowsCPUStepProblems returns what is wrong with the jobs of one workflow
// file against the rule that a job on a Windows runner starts by printing the
// CPU model. It also returns the number of Windows jobs it saw.
func windowsCPUStepProblems(file string, jobs map[string]faultJob) (problems []string, windowsJobs int) {
	for _, name := range slices.Sorted(maps.Keys(jobs)) {
		job := jobs[name]
		where := file + " job " + name + ": "
		runsOn, hasRunsOn := job.keys["runs-on"]
		if !hasRunsOn {
			if _, calls := job.keys["uses"]; !calls {
				problems = append(problems, where+"the job has neither runs-on nor uses; write runs-on as one plain runner name so the CPU step rule can see the job")
			}
			continue
		}
		if runsOn.Kind != yaml.ScalarNode || strings.Contains(runsOn.Value, "${{") {
			problems = append(problems, where+"write runs-on as one plain runner name so the CPU step rule can see the job")
			continue
		}
		if !isWindowsRunner(job) {
			continue
		}
		windowsJobs++
		if len(job.steps) == 0 {
			problems = append(problems, where+"the job has no steps; its first step must print the CPU model")
			continue
		}
		first := job.steps[0]
		if !slices.Equal(first.keys(), []string{"name", "run", "shell"}) {
			problems = append(problems, where+"the first step must print the CPU model with exactly the keys name, shell and run, got "+strings.Join(first.keys(), ", "))
			continue
		}
		if first.text("shell") != faultCPUShell {
			problems = append(problems, where+"the CPU step must use shell "+faultCPUShell)
		}
		if strings.TrimSpace(first.text("run")) != faultCPUCommand {
			problems = append(problems, where+"the CPU step must run "+faultCPUCommand)
		}
	}
	return problems, windowsJobs
}

// TestEveryWindowsJobStartsByPrintingTheCPU asserts that every job on a
// Windows runner, in every workflow file, has as its first step the step that
// prints the CPU model. A crash on a runner then names its host.
func TestEveryWindowsJobStartsByPrintingTheCPU(t *testing.T) {
	t.Parallel()
	var files []string
	for _, pattern := range []string{".github/workflows/*.yaml", ".github/workflows/*.yml"} {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		files = append(files, matches...)
	}
	require.NotEmpty(t, files)
	total := 0
	for _, file := range files {
		problems, windowsJobs := windowsCPUStepProblems(file, readFaultWorkflow(t, file))
		assert.Empty(t, problems)
		total += windowsJobs
	}
	assert.GreaterOrEqual(t, total, 2, "the rule found fewer Windows jobs than the repository has")
}

// TestWindowsCPUStepRules asserts what the checker of the CPU step accepts and
// refuses, over small workflow texts.
func TestWindowsCPUStepRules(t *testing.T) {
	t.Parallel()
	const cpuStep = "      - name: CPU model\n        shell: pwsh\n        run: " + faultCPUCommand + "\n"
	const otherStep = "      - run: go version\n"
	job := func(runsOn, steps string) string {
		return "jobs:\n  a:\n    runs-on: " + runsOn + "\n    steps:\n" + steps
	}
	for _, c := range []struct {
		name, text, want string
		windowsJobs      int
	}{
		{"a Windows job that starts with the step", job("windows-latest", cpuStep+otherStep), "", 1},
		{"a job on another runner with no step", job("ubuntu-latest", otherStep), "", 0},
		{"a reusable workflow call with no runner", "jobs:\n  call:\n    uses: ./.github/workflows/other.yaml\n", "", 0},
		{"a Windows job with no step", job("windows-latest", otherStep), "the first step must print the CPU model", 1},
		{"a Windows job whose query can fail the step", job("windows-latest", strings.Replace(cpuStep, " -ErrorAction SilentlyContinue", "", 1)+otherStep), "the CPU step must run", 1},
		{"a Windows job with the step second", job("windows-latest", otherStep+cpuStep), "the first step must print the CPU model", 1},
		{"a Windows job with another command", job("windows-latest", strings.Replace(cpuStep, "Win32_Processor", "Win32_BIOS", 1)+otherStep), "the CPU step must run", 1},
		{"a Windows job whose step has a condition", job("windows-latest", cpuStep+"        if: false\n"+otherStep), "exactly the keys name, shell and run", 1},
		{"a Windows job whose step continues on error", job("windows-latest", cpuStep+"        continue-on-error: true\n"+otherStep), "exactly the keys name, shell and run", 1},
		{"a Windows job whose step uses bash", job("windows-latest", strings.Replace(cpuStep, "shell: pwsh", "shell: bash", 1)+otherStep), "the CPU step must use shell pwsh", 1},
		{"a Windows job named with a capital and no step", job("Windows-latest", otherStep), "the first step must print the CPU model", 1},
		{"a Windows job with the step and a capital", job("Windows-2022", cpuStep+otherStep), "", 1},
		{"a Windows job with an empty step list", "jobs:\n  a:\n    runs-on: windows-latest\n", "the job has no steps", 1},
		{"a runner list", job("[self-hosted, windows]", cpuStep+otherStep), "write runs-on as one plain runner name", 0},
		{"a runner from an expression", job("${{ matrix.os }}", cpuStep+otherStep), "write runs-on as one plain runner name", 0},
		{"a runner group mapping", "jobs:\n  a:\n    runs-on:\n      group: big\n    steps:\n" + cpuStep, "write runs-on as one plain runner name", 0},
		{"a job with neither a runner nor a call", "jobs:\n  a:\n    steps:\n" + otherStep, "neither runs-on nor uses", 0},
	} {
		problems, windowsJobs := windowsCPUStepProblems("w.yaml", decodeFaultText(t, c.text))
		assert.Equal(t, c.windowsJobs, windowsJobs, c.name)
		if c.want == "" {
			assert.Empty(t, problems, c.name)
			continue
		}
		require.NotEmpty(t, problems, c.name)
		assert.Contains(t, strings.Join(problems, "\n"), c.want, c.name)
	}
}
