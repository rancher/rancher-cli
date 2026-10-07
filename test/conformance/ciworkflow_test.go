package conformance

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	ciWorkflowPath    = ".github/workflows/ci.yaml"
	setupToolsAction  = "./.github/actions/setup-tools"
	setupToolsFile    = ".github/actions/setup-tools/action.yml"
	validateScript    = "scripts/validate"
	versionScript     = "scripts/golangci-lint-version"
	lintScript        = "scripts/lint"
	posixOnlyReason   = "runs bash scripts and relies on a POSIX PATH and exec bits"
	installPathPrefix = "go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"
)

// workflowFile holds the parts of a workflow file the checks below read.
type workflowFile struct {
	Jobs map[string]workflowJob `yaml:"jobs"`
}

// The continue-on-error fields are nodes so that an absent field (a zero
// node) differs from any value, true or an expression included.
type workflowJob struct {
	Needs           yaml.Node      `yaml:"needs"`
	If              string         `yaml:"if"`
	ContinueOnError yaml.Node      `yaml:"continue-on-error"`
	TimeoutMinutes  int            `yaml:"timeout-minutes"`
	Steps           []workflowStep `yaml:"steps"`
}

type workflowStep struct {
	Name             string            `yaml:"name"`
	Uses             string            `yaml:"uses"`
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

// The rules for the run bodies of ci.yaml are an allowlist: the two part
// steps are exact, every other body must not mention validate or
// scripts/ci, and a make word may only be followed by the e2e target.
// A list of forbidden spellings would always miss one.
const (
	staticStepBody = "./scripts/validate static"
	testsStepBody  = "./scripts/validate tests"
)

var (
	validateWordRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])(validate|scripts/(\./)*ci)([^A-Za-z0-9_]|$)`)
	makeWordRE     = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(g?make|\$MAKE|\$\{MAKE\}|\$\(MAKE\))([^A-Za-z0-9_-]|$)`)
	// e2e must be the only target: every script is a make target, so
	// "make e2e ci" would also run the whole gate.
	makeTargetRE = regexp.MustCompile(`^ +e2e *($|\n|;|&|\|)`)
)

// ciRunBodyProblems returns what is wrong with a run body of ci.yaml that
// is not one of the two part steps. The allowed make targets are listed
// here: e2e only.
func ciRunBodyProblems(body string) []string {
	var problems []string
	if validateWordRE.MatchString(body) {
		problems = append(problems, "the word validate or scripts/ci appears; only the two part steps may run the gate")
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

// TestCIRunsTheTwoValidateParts asserts that ci.yaml runs the static and
// tests parts of scripts/validate, each as the whole run body of one
// step, in two separate jobs that do not name each other in needs; that
// no other run body mentions validate or scripts/ci or runs make with a
// target but e2e (TestCIRunBodyRules); that neither job nor any of its
// steps can be turned off; and that each job installs only the tools its
// part uses.
func TestCIRunsTheTwoValidateParts(t *testing.T) {
	t.Parallel()
	wf := readWorkflow(t, ciWorkflowPath)

	partJobs := map[string][]string{}
	for name, job := range wf.Jobs {
		for i, step := range job.Steps {
			switch strings.TrimSpace(step.Run) {
			case staticStepBody:
				partJobs["static"] = append(partJobs["static"], name)
				assert.Empty(t, step.Shell, "job %s step %d must not change the shell", name, i+1)
				assert.Empty(t, step.WorkingDirectory, "job %s step %d must not change the directory", name, i+1)
			case testsStepBody:
				partJobs["tests"] = append(partJobs["tests"], name)
				assert.Empty(t, step.Shell, "job %s step %d must not change the shell", name, i+1)
				assert.Empty(t, step.WorkingDirectory, "job %s step %d must not change the directory", name, i+1)
			default:
				assert.Empty(t, ciRunBodyProblems(step.Run), "job %s step %d", name, i+1)
			}
		}
	}
	require.Len(t, partJobs["static"], 1, "%q must be the run body of exactly one step", staticStepBody)
	require.Len(t, partJobs["tests"], 1, "%q must be the run body of exactly one step", testsStepBody)
	staticJob, testsJob := partJobs["static"][0], partJobs["tests"][0]
	require.NotEqual(t, staticJob, testsJob, "the two parts must run in different jobs")

	assert.NotContains(t, wf.Jobs[staticJob].needsNames(), testsJob, "job %s must not wait for job %s", staticJob, testsJob)
	assert.NotContains(t, wf.Jobs[testsJob].needsNames(), staticJob, "job %s must not wait for job %s", testsJob, staticJob)

	for _, c := range []struct {
		job, part, needs, drops string
	}{
		{staticJob, "static", "lint-tools", "shells"},
		{testsJob, "tests", "shells", "lint-tools"},
	} {
		job := wf.Jobs[c.job]
		assert.Empty(t, job.If, "job %s must not be conditional", c.job)
		assert.Zero(t, job.ContinueOnError.Kind, "job %s must not set continue-on-error", c.job)
		setups := 0
		for i, step := range job.Steps {
			assert.Empty(t, step.If, "job %s step %d must not be conditional", c.job, i+1)
			assert.Zero(t, step.ContinueOnError.Kind, "job %s step %d must not set continue-on-error", c.job, i+1)
			if step.Uses != setupToolsAction {
				continue
			}
			setups++
			if v, ok := step.With[c.needs]; ok {
				assert.Equal(t, "true", v, "job %s runs the %s part and needs %s installed", c.job, c.part, c.needs)
			}
			assert.Equal(t, "false", step.With[c.drops], "job %s does not use %s and must not install it", c.job, c.drops)
		}
		require.Equal(t, 1, setups, "job %s must use %s once", c.job, setupToolsAction)
	}

	assert.Equal(t, 30, wf.Jobs[staticJob].TimeoutMinutes, "job %s timeout-minutes", staticJob)
	assert.Equal(t, 30, wf.Jobs[testsJob].TimeoutMinutes, "job %s timeout-minutes", testsJob)
	require.Contains(t, wf.Jobs, "cross-compile")
	assert.Equal(t, 20, wf.Jobs["cross-compile"].TimeoutMinutes, "job cross-compile timeout-minutes")
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
// argument runs the static part (vet, the lint version check, lint) and
// then the tests part (the unit tests, then the build), that each part
// alone runs only its own commands, that a failing command ends the run
// with a non-zero exit before any later command, and that any other
// arguments exit 2 with a usage line naming both parts.
func TestValidateRunsThePartsInOrder(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip(posixOnlyReason)
	}
	root, env, logPath := dispatchLayout(t)
	run := func(extraEnv []string, args ...string) (int, string, []string) {
		_ = os.Remove(logPath)
		code, stderr := runScript(t, root, filepath.Join(root, validateScript), append(slices.Clone(env), extraEnv...), args...)
		return code, stderr, readLog(t, logPath)
	}
	index := func(log []string, line string) int {
		t.Helper()
		i := slices.Index(log, line)
		require.GreaterOrEqual(t, i, 0, "%q is not in the log %q", line, log)
		return i
	}

	code, stderr, log := run(nil)
	require.Zero(t, code, stderr)
	vet := index(log, "go vet ./...")
	version := index(log, "golangci-lint version")
	lint := index(log, "golangci-lint run")
	unit := index(log, "scripts/test -vet=off")
	build := index(log, "scripts/build")
	assert.Less(t, vet, lint)
	assert.Less(t, version, lint)
	assert.Less(t, lint, unit)
	assert.Less(t, unit, build)

	code, stderr, log = run(nil, "static")
	require.Zero(t, code, stderr)
	assert.Contains(t, log, "go vet ./...")
	assert.Contains(t, log, "golangci-lint run")
	assert.NotContains(t, log, "scripts/test -vet=off")
	assert.NotContains(t, log, "scripts/build")

	code, stderr, log = run(nil, "tests")
	require.Zero(t, code, stderr)
	assert.Contains(t, log, "scripts/test -vet=off")
	assert.Contains(t, log, "scripts/build")
	assert.NotContains(t, log, "go vet ./...")
	assert.NotContains(t, log, "golangci-lint run")

	code, _, log = run([]string{"STUB_FAIL=go vet ./..."})
	assert.NotZero(t, code, "a failing go vet must fail the run")
	assert.Contains(t, log, "go vet ./...")
	assert.NotContains(t, log, "scripts/test -vet=off", "no test runs after a failing static part")

	code, _, log = run([]string{"STUB_FAIL=scripts/test -vet=off"})
	assert.NotZero(t, code, "a failing unit test run must fail the run")
	assert.Contains(t, log, "scripts/test -vet=off")
	assert.NotContains(t, log, "scripts/build", "no build after failing tests")

	for _, args := range [][]string{{"bogus"}, {"static", "tests"}} {
		code, stderr, log = run(nil, args...)
		assert.Equal(t, 2, code, "arguments %q", args)
		assert.Contains(t, stderr, "static", "arguments %q", args)
		assert.Contains(t, stderr, "tests", "arguments %q", args)
		assert.Empty(t, log, "arguments %q must run nothing", args)
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
