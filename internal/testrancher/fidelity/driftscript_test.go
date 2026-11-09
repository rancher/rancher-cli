package fidelity

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driftVariableNames are the six variables the drift run sets for the test
// binary; the scripts refuse them without E2E_FIDELITY_DRIFT=1.
var driftVariableNames = []string{
	"E2E_FIDELITY_DRIFT_REPORT", "E2E_FIDELITY_DRIFT_KNOWN", "E2E_FIDELITY_DRIFT_STARTED",
	"E2E_FIDELITY_GOLDEN_IMAGE", "E2E_FIDELITY_GOLDEN_K3S_IMAGE", "E2E_FIDELITY_DOWNSTREAM_IMAGE",
}

// driftRefusedNames are the seven variables the scripts refuse without
// E2E_FIDELITY_DRIFT=1: the six of driftVariableNames and
// E2E_FIDELITY_DRIFT_PROFILE, which the script sets for every drift run and
// the test binary reads separately from the six.
var driftRefusedNames = append(slices.Clone(driftVariableNames), "E2E_FIDELITY_DRIFT_PROFILE")

// scriptLiteral reads the value of a top-level variable assignment of
// scripts/e2e-fidelity, such as ORACLE_IMAGE="...".
func scriptLiteral(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(realScriptPath(t))
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^` + name + `="([^"]*)"$`).FindSubmatch(raw)
	require.NotNilf(t, m, "%s literal not found in the script", name)
	return string(m[1])
}

// TestDriftRefusesBeforeDocker asserts that every refusal of the drift
// variables exits 1 with its message before any docker call and before any
// state is created, and that E2E_FIDELITY_DRIFT=0 with no drift variable
// is not refused.
func TestDriftRefusesBeforeDocker(t *testing.T) {
	t.Parallel()
	type tc struct {
		name string
		env  map[string]string
		want string
	}
	cases := []tc{
		{"drift without a minor", map[string]string{"E2E_FIDELITY_DRIFT": "1"},
			"error: E2E_FIDELITY_DRIFT needs E2E_FIDELITY_MINOR=<major>.<minor>; the drift run compares an upcoming minor with the community goldens"},
		{"drift with a record flag", map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_UPDATE_FLAG": "-update"},
			"error: E2E_FIDELITY_DRIFT compares only; it never records goldens"},
		{"drift with prime", map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRIME": "1"},
			"E2E_FIDELITY_PRIME and E2E_FIDELITY_MINOR select different oracles; set one"},
		{"report in a missing directory", map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_DRIFT_REPORT": "/nonexistent-dir/report.json"},
			"error: E2E_FIDELITY_DRIFT_REPORT=/nonexistent-dir/report.json is not a file path in an existing writable directory"},
		{"report that is a directory", map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_DRIFT_REPORT": os.TempDir()},
			"error: E2E_FIDELITY_DRIFT_REPORT=" + os.TempDir() + " is not a file path in an existing writable directory"},
		{"report with a trailing slash", map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_DRIFT_REPORT": "/nonexistent-dir/"},
			"is not a file path in an existing writable directory"},
	}
	for _, name := range driftRefusedNames {
		want := "error: " + name + " is set without E2E_FIDELITY_DRIFT=1; unset it"
		cases = append(cases,
			tc{name + " alone", map[string]string{name: "x"}, want},
			tc{name + " with a minor", map[string]string{name: "x", "E2E_FIDELITY_MINOR": "2.16"}, want},
			tc{name + " with drift off", map[string]string{name: "x", "E2E_FIDELITY_DRIFT": "0", "E2E_FIDELITY_MINOR": "2.16"}, want},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newScenario(t)
			s.env = c.env
			res := s.run()
			s.requireRefused(res, c.want)
			assert.Empty(t, s.reg.recorded())
		})
	}
	for _, c := range []struct {
		name string
		mode os.FileMode
	}{{"a directory that is not writable", 0o500}, {"a directory that is not searchable", 0o200}} {
		t.Run("report in "+c.name, func(t *testing.T) {
			t.Parallel()
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			dir := filepath.Join(t.TempDir(), "reports")
			require.NoError(t, os.Mkdir(dir, 0o700))
			require.NoError(t, os.Chmod(dir, c.mode))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			path := filepath.Join(dir, "report.json")
			s := newScenario(t)
			s.env = map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_DRIFT_REPORT": path}
			res := s.run()
			s.requireRefused(res, "error: E2E_FIDELITY_DRIFT_REPORT="+path+" is not a file path in an existing writable directory")
		})
	}
	t.Run("E2E_FIDELITY_DRIFT=0 with no drift variable is not refused", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		s.env = map[string]string{"E2E_FIDELITY_DRIFT": "0"}
		res := s.run()
		assert.NotContains(t, res.stderr, "E2E_FIDELITY_DRIFT")
		assert.Equal(t, 97, res.code, "the run goes on to the community profile and stops at the docker stub")
	})
	t.Run("E2E_FIDELITY_DRIFT=0 with a minor is not refused", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		s.publishDefaults()
		s.env = map[string]string{"E2E_FIDELITY_DRIFT": "0", "E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1"}
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		assert.NotContains(t, decodePrinted(t, res.stdout), "driftKnown")
	})
}

// TestDriftPrintMode asserts what print mode shows about a drift run: the
// images the community goldens were recorded against (the two literals of
// the script, not the upcoming images), and with drift on the absolute
// report path (empty for the default) and the known file under the script's
// own repository root.
func TestDriftPrintMode(t *testing.T) {
	t.Parallel()
	printMode := func(t *testing.T, s *scenario, extra map[string]string) map[string]string {
		t.Helper()
		s.publishDefaults()
		s.env = map[string]string{"E2E_FIDELITY_MINOR": "2.16", "E2E_FIDELITY_PRINT_IMAGES": "1"}
		maps.Copy(s.env, extra)
		res := s.run()
		require.Equalf(t, 0, res.code, "stderr: %s", res.stderr)
		return decodePrinted(t, res.stdout)
	}
	resolved := func(t *testing.T, path string) string {
		t.Helper()
		out, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		return out
	}

	t.Run("the goldens' images, not the upcoming images", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		got := printMode(t, s, nil)
		assert.Equal(t, scriptLiteral(t, "ORACLE_IMAGE"), got["goldenOracleImage"])
		assert.Equal(t, scriptLiteral(t, "K3S_IMAGE"), got["goldenK3sImage"])
		assert.NotEqual(t, got["oracleImage"], got["goldenOracleImage"])
		assert.NotEqual(t, got["k3sImage"], got["goldenK3sImage"])
		assert.NotContains(t, got, "driftReport")
		assert.NotContains(t, got, "driftKnown")
	})

	t.Run("the goldens' images also with an upcoming override", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		got := printMode(t, s, map[string]string{"E2E_FIDELITY_K3S_IMAGE": "rancher/k3s:v1.99.0-k3s1@" + sampleDigest})
		assert.Equal(t, scriptLiteral(t, "K3S_IMAGE"), got["goldenK3sImage"])
		assert.Equal(t, "rancher/k3s:v1.99.0-k3s1@"+sampleDigest, got["k3sImage"])
	})

	t.Run("a relative report path is made absolute against the working directory", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		got := printMode(t, s, map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_DRIFT_REPORT": "report.json"})
		assert.True(t, filepath.IsAbs(got["driftReport"]), got["driftReport"])
		assert.Equal(t, filepath.Join(resolved(t, s.home), "report.json"), filepath.Join(resolved(t, filepath.Dir(got["driftReport"])), filepath.Base(got["driftReport"])))
		repoRoot := resolved(t, filepath.Join(filepath.Dir(realScriptPath(t)), ".."))
		assert.Equal(t, filepath.Join(repoRoot, "internal", "testrancher", "fidelity", "testdata", "upcoming", "v2.16.json"), got["driftKnown"])
	})

	t.Run("an absolute report path is kept", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		path := filepath.Join(t.TempDir(), "named.json")
		got := printMode(t, s, map[string]string{"E2E_FIDELITY_DRIFT": "1", "E2E_FIDELITY_DRIFT_REPORT": path})
		assert.Equal(t, path, got["driftReport"])
	})

	t.Run("no report path prints empty", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		got := printMode(t, s, map[string]string{"E2E_FIDELITY_DRIFT": "1"})
		assert.Contains(t, got, "driftReport")
		assert.Empty(t, got["driftReport"])
	})

	t.Run("a copy of the script in another directory uses its own repository root", func(t *testing.T) {
		t.Parallel()
		s := newScenario(t)
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "scripts"), 0o755))
		raw, err := os.ReadFile(realScriptPath(t))
		require.NoError(t, err)
		s.script = filepath.Join(root, "scripts", "e2e-fidelity")
		testfs.WriteExecutable(t, s.script, raw)
		got := printMode(t, s, map[string]string{"E2E_FIDELITY_DRIFT": "1"})
		assert.Equal(t, filepath.Join(resolved(t, root), "internal", "testrancher", "fidelity", "testdata", "upcoming", "v2.16.json"), got["driftKnown"])
	})
}

// TestDriftDownstreamScriptRefusesTheDriftVariables asserts that
// scripts/e2e-fidelity-downstream refuses each of the six drift variables
// with the same message, before it runs go, and still runs go without them.
func TestDriftDownstreamScriptRefusesTheDriftVariables(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/e2e-fidelity-downstream is a bash script")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "e2e-fidelity-downstream"))
	require.NoError(t, err)
	run := func(t *testing.T, extra map[string]string) (stderr string, code int, goCalls []string) {
		t.Helper()
		dir := t.TempDir()
		logPath := filepath.Join(dir, "go.log")
		require.NoError(t, os.WriteFile(logPath, nil, 0o600))
		testfs.WriteExecutable(t, filepath.Join(dir, "go"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$STUB_LOG\"\n"))
		env := []string{
			"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "STUB_LOG=" + logPath,
			"E2E_FIDELITY_URL=https://127.0.0.1:1", "E2E_FIDELITY_ADMIN_TOKEN=t", "E2E_FIDELITY_CACERT=pem", "E2E_FIDELITY_DOWNSTREAM=c-x",
		}
		for k, v := range extra {
			env = append(env, k+"="+v)
		}
		probe := exec.CommandContext(t.Context(), "bash", "-c", "command -v go")
		probe.Env = env
		found, err := probe.Output()
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, "go"), strings.TrimSpace(string(found)), "go must resolve to the stub before the script runs")
		cmd := exec.CommandContext(t.Context(), "bash", script)
		cmd.Env = env
		cmd.Dir = dir
		var errOut strings.Builder
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			exitErr, ok := errors.AsType[*exec.ExitError](err)
			require.Truef(t, ok, "running the script: %v", err)
			code = exitErr.ExitCode()
		}
		raw, err := os.ReadFile(logPath)
		require.NoError(t, err)
		for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				goCalls = append(goCalls, line)
			}
		}
		return errOut.String(), code, goCalls
	}

	_, code, calls := run(t, nil)
	require.Equal(t, 0, code)
	require.Len(t, calls, 1, "without a drift variable the script runs go test")
	assert.Contains(t, calls[0], "test -tags e2e_fidelity")

	for _, name := range driftRefusedNames {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stderr, code, calls := run(t, map[string]string{name: "x"})
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr, "error: "+name+" is set without E2E_FIDELITY_DRIFT=1; unset it")
			assert.Empty(t, calls, "no go call")
		})
	}
}

// shellEnv is an isolated shell for one program that uses functions cut out
// of scripts/e2e-fidelity: docker and df stubs first on PATH and no Docker
// daemon.
type shellEnv struct {
	t       *testing.T
	dir     string
	logPath string
}

func newShellEnv(t *testing.T) *shellEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("scripts/e2e-fidelity is a bash script")
	}
	e := &shellEnv{t: t, dir: t.TempDir()}
	e.logPath = filepath.Join(e.dir, "docker.log")
	require.NoError(t, os.WriteFile(e.logPath, nil, 0o600))
	testfs.WriteExecutable(t, filepath.Join(e.dir, "docker"), []byte(dockerStubScript))
	testfs.WriteExecutable(t, filepath.Join(e.dir, "df"), []byte("#!/bin/sh\necho df-stub\n"))
	testfs.WriteExecutable(t, filepath.Join(e.dir, "memory_pressure"), []byte(memoryPressureStubScript))
	return e
}

// run executes the program and returns what it printed, its exit code and
// the docker stub's calls.
func (e *shellEnv) run(program string) (stdout, stderr string, code int, dockerCalls []string) {
	e.t.Helper()
	env := []string{
		"PATH=" + e.dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + e.dir, "STUB_LOG=" + e.logPath, "DOCKER_HOST=unix:///nonexistent",
		"E2E_FIDELITY_KEEP=1",
	}
	probe := exec.CommandContext(e.t.Context(), "bash", "-c", "command -v docker")
	probe.Env = env
	found, err := probe.Output()
	require.NoError(e.t, err)
	require.Equal(e.t, filepath.Join(e.dir, "docker"), strings.TrimSpace(string(found)), "docker must resolve to the stub")
	cmd := exec.CommandContext(e.t.Context(), "bash", "-c", program)
	cmd.Env = env
	cmd.Dir = e.dir
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		require.Truef(e.t, ok, "running the program: %v", err)
		code = exitErr.ExitCode()
	}
	raw, err := os.ReadFile(e.logPath)
	require.NoError(e.t, err)
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			dockerCalls = append(dockerCalls, line)
		}
	}
	return out.String(), errOut.String(), code, dockerCalls
}

// cleanupDefinitions sets what cleanup reads and defines cleanup with the
// functions and variables it uses, under set -euo pipefail. DRIFT_REPORTED is
// set first because the script runs under set -u. The memory reading has no
// meminfo file and reads the memory_pressure stub of the environment.
func (e *shellEnv) cleanupDefinitions(drift int, reported int) string {
	e.t.Helper()
	state := filepath.Join(e.dir, "state.json")
	lock := filepath.Join(e.dir, "lock")
	require.NoError(e.t, os.WriteFile(state, []byte("{}"), 0o600))
	require.NoError(e.t, os.MkdirAll(lock, 0o700))
	return "set -euo pipefail\n" +
		"DRIFT=" + strconv.Itoa(drift) + "\nDRIFT_REPORTED=" + strconv.Itoa(reported) + "\n" +
		"STATE_FILE=" + state + " LOCK_DIR=" + lock + " CONTAINER=c K3S_CONTAINER=c-k3s NETWORK=c-net\n" +
		"MEMINFO_FILE=" + filepath.Join(e.dir, "no-meminfo") + " MEMORY_PRESSURE_CMD=memory_pressure MEMORY_READ_LIMIT_SECONDS=5\n" +
		shellFunction(e.t, "redact_log") +
		shellFunction(e.t, "host_memory_free") +
		shellFunction(e.t, "cleanup")
}

// cleanupProgram sets what cleanup reads, installs it as the exit trap behind
// a probe that prints DRIFT_REPORTED, and runs body.
func (e *shellEnv) cleanupProgram(drift int, reported int, body string) string {
	e.t.Helper()
	return e.cleanupDefinitions(drift, reported) +
		"report_flag() { echo \"DRIFT_REPORTED=${DRIFT_REPORTED}\" >&2; }\n" +
		"trap 'rc=$?; set +e; report_flag; (exit ${rc}); cleanup' EXIT\n" +
		body + "\n"
}

func dumped(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "stats") || strings.HasPrefix(c, "logs") {
			return true
		}
	}
	return false
}

// TestDriftCleanup asserts that in a drift run cleanup keeps exit status 3
// and skips the diagnostics dump only once drift_result set DRIFT_REPORTED,
// turns any other 3 into 1 with the dump, and that outside a drift run a 3
// stays 3 with the dump, as every other failure.
func TestDriftCleanup(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		drift, reported int
		body            string
		code            int
		dump            bool
	}{
		{"drift, 3, reported", 1, 1, "exit 3", 3, false},
		{"drift, 3, not reported", 1, 0, "exit 3", 1, true},
		{"drift, 1, reported", 1, 1, "exit 1", 1, true},
		{"drift, 0", 1, 0, "exit 0", 0, false},
		{"not drift, 3", 0, 0, "exit 3", 3, true},
		{"not drift, 1", 0, 0, "exit 1", 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newShellEnv(t)
			stdout, stderr, code, calls := e.run(e.cleanupProgram(c.drift, c.reported, c.body))
			assert.Equal(t, c.code, code, "stderr: %s", stderr)
			assert.Equal(t, c.dump, dumped(calls), "docker calls: %v", calls)
			assert.Equal(t, c.dump, strings.Contains(stdout, "==> host resources:"), "stdout: %s", stdout)
			assert.Contains(t, stderr, "DRIFT_REPORTED="+strconv.Itoa(c.reported))
			_, err := os.Stat(filepath.Join(e.dir, "lock"))
			assert.True(t, os.IsNotExist(err), "the lock directory is removed")
			for _, call := range calls {
				assert.NotContains(t, call, "rm ", "E2E_FIDELITY_KEEP=1 keeps the containers")
			}
		})
	}
}

// stubReport writes a drift report for drift_result to read. WriteReport sets
// complete; tweak changes the report first.
func stubReport(t *testing.T, tweak func(*Report)) string {
	t.Helper()
	r := Report{
		Minor: "2.16", StartedAt: "2026-10-20T19:00:00Z", OracleImage: "rancher/rancher:head@sha256:aa",
		Clean: true,
	}
	if tweak != nil {
		tweak(&r)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	require.NoError(t, WriteReport(path, r))
	return path
}

// TestDriftResult asserts how drift_result maps the report and go test's
// status to an exit status and the summary line.
func TestDriftResult(t *testing.T) {
	t.Parallel()
	const image, started = "rancher/rancher:head@sha256:aa", "2026-10-20T19:00:00Z"
	const failed = "error: the drift run failed: "
	run := func(t *testing.T, goStatus int, path string) (stdout, stderr string, code int, calls []string) {
		t.Helper()
		e := newShellEnv(t)
		body := "drift_result " + strconv.Itoa(goStatus) + " " + path + " " + image + " " + started
		program := e.cleanupProgram(1, 0, "")
		program = strings.Replace(program, "trap 'rc=$?", shellFunction(t, "drift_result")+"trap 'rc=$?", 1) + body + "\n"
		return e.run(program)
	}
	notClean := func(r *Report) {
		r.Clean = false
		r.Counts = DriftCounts{
			New: 2, WaitRanOut: 1, Known: 3, KnownNoRuling: 4, NoLongerSeen: 5,
			NotChecked: 6, NotCheckedProbes: 10, Errored: 7, FailedCLITests: 8, SuitesWithNoProbe: 9,
		}
	}

	t.Run("a clean report and go test passed exits 0", func(t *testing.T) {
		t.Parallel()
		path := stubReport(t, nil)
		stdout, stderr, code, calls := run(t, 0, path)
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Equal(t, "drift v2.16: new 0 (wait ran out 0), known 0, known no ruling 0, no longer seen 0, entries not checked 0, probes not checked 0, errored 0, failed CLI tests 0, suites with no probe 0; report "+path+"\n", stdout)
		assert.False(t, dumped(calls))
		assert.Contains(t, stderr, "DRIFT_REPORTED=0")
	})

	t.Run("a report with differences exits 3 and sets DRIFT_REPORTED", func(t *testing.T) {
		t.Parallel()
		path := stubReport(t, notClean)
		stdout, stderr, code, calls := run(t, 1, path)
		assert.Equal(t, 3, code, "stderr: %s", stderr)
		assert.Equal(t, "drift v2.16: new 2 (wait ran out 1), known 3, known no ruling 4, no longer seen 5, entries not checked 6, probes not checked 10, errored 7, failed CLI tests 8, suites with no probe 9; report "+path+"\n", stdout,
			"the summary line is the last and only line, with disjoint counts")
		assert.Contains(t, stderr, "DRIFT_REPORTED=1")
		assert.False(t, dumped(calls), "no diagnostics dump for a reported result")
	})

	t.Run("a helm-check report names its profile on the summary line", func(t *testing.T) {
		t.Parallel()
		path := stubReport(t, func(r *Report) {
			notClean(r)
			r.Profile = ProfileHelmCheck
		})
		stdout, _, code, _ := run(t, 1, path)
		assert.Equal(t, 3, code)
		assert.Equal(t, "drift v2.16 (helm-check): new 2 (wait ran out 1), known 3, known no ruling 4, no longer seen 5, entries not checked 6, probes not checked 10, errored 7, failed CLI tests 8, suites with no probe 9; report "+path+"\n", stdout)
	})

	t.Run("a report with differences exits 3 whatever go test's status", func(t *testing.T) {
		t.Parallel()
		_, _, code, _ := run(t, 0, stubReport(t, notClean))
		assert.Equal(t, 3, code)
	})

	failing := []struct {
		name     string
		goStatus int
		path     func(t *testing.T) string
		message  string
		outcome  string
	}{
		{"no report", 1, func(t *testing.T) string {
			t.Helper()
			return filepath.Join(t.TempDir(), "none.json")
		}, "no report at ", "error: the drift run did not finish; "},
		{"an unreadable report", 1, func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "report.json")
			require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			return path
		}, "is unreadable", "error: the drift run did not finish; "},
		{"an incomplete report", 1, func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "report.json")
			raw, err := json.Marshal(Report{Minor: "2.16", StartedAt: started, OracleImage: image, Clean: true})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			return path
		}, "is not complete", "error: the drift run did not finish; "},
		{"a malformed report", 1, func(t *testing.T) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "report.json")
			raw, err := json.Marshal(map[string]any{"minor": "2.16", "startedAt": started, "oracleImage": image, "complete": true})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			return path
		}, "is malformed", "error: the drift run did not finish; "},
		{"a report of another image", 1, func(t *testing.T) string {
			t.Helper()
			return stubReport(t, func(r *Report) { r.OracleImage = "rancher/rancher:other@sha256:bb" })
		}, "is not from this run", "error: the drift run did not finish; "},
		{"a report of another start time", 1, func(t *testing.T) string {
			t.Helper()
			return stubReport(t, func(r *Report) { r.StartedAt = "2026-10-19T19:00:00Z" })
		}, "is not from this run", "error: the drift run did not finish; "},
		{"a report with a harness failure", 1, func(t *testing.T) string {
			t.Helper()
			return stubReport(t, func(r *Report) {
				notClean(r)
				r.HarnessFailures = []HarnessFailure{{Name: "/rancherversion", Reason: "connection refused"}}
			})
		}, "/rancherversion: connection refused", "error: the drift run failed: "},
		{"a report with a multi-line harness reason", 1, func(t *testing.T) string {
			t.Helper()
			return stubReport(t, func(r *Report) {
				notClean(r)
				r.HarnessFailures = []HarnessFailure{{Name: "known file", Reason: "first line\nsecond line"}, {Name: "TestOracle", Reason: "failed"}}
			})
		}, "known file: first line; second line; TestOracle: failed", "error: the drift run failed: "},
		{"a clean report and a failed go test", 1, func(t *testing.T) string {
			t.Helper()
			return stubReport(t, nil)
		}, "although the drift report is clean", "error: go test exited "},
	}
	for _, c := range failing {
		t.Run(c.name+" exits 1 with the dump", func(t *testing.T) {
			t.Parallel()
			stdout, stderr, code, calls := run(t, c.goStatus, c.path(t))
			assert.Equal(t, 1, code, "stderr: %s", stderr)
			assert.Contains(t, stderr, c.outcome)
			assert.Contains(t, stderr, c.message)
			if c.outcome != failed {
				assert.NotContains(t, stderr, failed)
			}
			assert.True(t, dumped(calls), "diagnostics dump: %v", calls)
			assert.NotContains(t, stdout, "drift v2.16:")
			assert.Contains(t, stderr, "DRIFT_REPORTED=0")
		})
	}
}
