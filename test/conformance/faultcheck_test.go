package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testfs"
)

const (
	faultCheckScript = "scripts/fault-check"
	faultTraceScript = "scripts/fault-trace"
)

// The log lines below have the shapes strace prints with -f -Y -q: every
// line starts with the thread id and the thread name in angle brackets.

func logExec(id, thread, program string) string {
	return fmt.Sprintf(`%s<%s> execve("/w/%s", ["/w/%s"], 0x1 /* 5 vars */) = 0`, id, thread, program, program)
}

func logFailedExec(id, thread, program string) string {
	return fmt.Sprintf(`%s<%s> execve("/w/%s", ["/w/%s"], 0x1 /* 5 vars */) = -1 ENOENT (No such file or directory)`, id, thread, program, program)
}

func logSplitExec(id, thread, program, result string) []string {
	return []string{
		fmt.Sprintf(`%s<%s> execve("/w/%s", ["/w/%s"], 0x1 /* 5 vars */ <unfinished ...>`, id, thread, program, program),
		fmt.Sprintf(`%s<%s> <... execve resumed>) = %s`, id, thread, result),
	}
}

func logClone(id, thread, child, childThread string) string {
	return fmt.Sprintf(`%s<%s> clone(child_stack=NULL, flags=CLONE_VM|CLONE_PIDFD|CLONE_VFORK, parent_tid=[10]) = %s<%s>`, id, thread, child, childThread)
}

func logSplitClone(id, thread, resumedID, child, childThread string) []string {
	return []string{
		fmt.Sprintf(`%s<%s> clone(child_stack=0x35c700518000, flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD|CLONE_SYSVSEM <unfinished ...>`, id, thread),
		fmt.Sprintf(`%s<%s> <... clone resumed>) = %s<%s>`, resumedID, thread, child, childThread),
	}
}

func logExit(id, thread string) string {
	return fmt.Sprintf(`%s<%s> +++ exited with 0 +++`, id, thread)
}

func logFault(id, thread, signal, code string) string {
	return fmt.Sprintf(`%s<%s> --- SIG%s {si_signo=SIG%s, si_code=%s, si_addr=NULL} ---`, id, thread, signal, signal, code)
}

func join(parts ...any) []string {
	var lines []string
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			lines = append(lines, v)
		case []string:
			lines = append(lines, v...)
		}
	}
	return lines
}

// faults returns n recovered nil dereferences taken by the thread id.
func faults(n int, id, thread string) []string {
	var lines []string
	for range n {
		lines = append(lines, logFault(id, thread, "SEGV", "SEGV_MAPERR"))
	}
	return lines
}

// faultLog is one traced run: a log and, unless pkg is empty, its sidecar.
type faultLog struct {
	pkg   string
	lines []string
}

func providerRun(n int) faultLog {
	return faultLog{"internal/client/provider", join(logExec("10", "provider.test", "provider.test"), faults(n, "10", "provider.test"))}
}

// runFaultCheck runs scripts/fault-check without the GITHUB_ACTIONS variable
// of the environment; extraEnv may set it.
func runFaultCheck(t *testing.T, dir, allowlist string, extraEnv ...string) (int, string) {
	t.Helper()
	script, err := filepath.Abs(faultCheckScript)
	require.NoError(t, err)
	list := filepath.Join(t.TempDir(), "allowlist")
	writeTestFile(t, list, allowlist)
	cmd := exec.CommandContext(t.Context(), script)
	env := slices.DeleteFunc(testfs.EnvWithoutGit(), func(v string) bool { return strings.HasPrefix(v, "GITHUB_ACTIONS=") })
	cmd.Env = append(append(env, "FAULT_LOG_DIR="+dir, "FAULT_ALLOWLIST="+list), extraEnv...)
	out, err := cmd.CombinedOutput()
	return exitCodeOf(t, err), string(out)
}

func writeFaultLogs(t *testing.T, logs []faultLog) string {
	t.Helper()
	dir := t.TempDir()
	for i, l := range logs {
		name := filepath.Join(dir, "run."+strconv.Itoa(i))
		writeTestFile(t, name+".log", strings.Join(l.lines, "\n")+"\n")
		if l.pkg != "" {
			writeTestFile(t, name+".pkg", l.pkg+"\n")
		}
	}
	return dir
}

// TestFaultCheckBehaviour asserts what scripts/fault-check counts as a
// fault, which program and package it names, and when it fails, over log
// files in the shapes strace prints. It also asserts that the notes
// scripts/fault-trace writes for repeated runs are printed on every exit of
// scripts/fault-check after the directory check, as GitHub Actions warnings
// too when GITHUB_ACTIONS is set, and that they do not change the exit
// status.
func TestFaultCheckBehaviour(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	const providerRow = "internal/client/provider|provider.test|%d|a recovered fault\n"
	const retryNote = "scripts/fault-trace: strace stopped with an error while tracing the tests of internal/x; the run is repeated once. strace said: strace: ptrace(PTRACE_LISTEN,pid:42,sig:0): Input/output error"
	row := func(n int) string { return fmt.Sprintf(providerRow, n) }
	plainRun := func(program string, lines ...any) faultLog {
		return faultLog{"internal/x", join(append([]any{logExec("16", program, program)}, lines...)...)}
	}

	for _, c := range []struct {
		name        string
		logs        []faultLog
		extraFiles  map[string]string
		allowlist   string
		env         []string
		wantExit    int
		contains    []string
		notContains []string
	}{
		{name: "no log at all", wantExit: 1, contains: []string{"nothing was traced"}},
		{name: "a run with no fault and an empty allowlist", logs: []faultLog{plainRun("a.test")}, contains: []string{"internal/x", "no unexpected faults"}},
		{name: "a fault with no row", logs: []faultLog{providerRun(4)}, wantExit: 1, contains: []string{"internal/client/provider\tprovider.test\t4\t-\tFAIL"}},
		{name: "a fault and an exact row", logs: []faultLog{providerRun(4)}, allowlist: row(4)},
		{name: "a row that allows more faults than the run took", logs: []faultLog{providerRun(4)}, allowlist: row(5), wantExit: 1, contains: []string{"fewer faults than the row allows"}},
		{name: "a row that allows fewer faults than the run took", logs: []faultLog{providerRun(4)}, allowlist: row(3), wantExit: 1, contains: []string{"more faults than the row allows"}},
		{name: "an any row and no fault", logs: []faultLog{plainRun("pwsh")}, allowlist: "internal/x|pwsh|any|a reason\n"},
		{name: "an any row and one fault", logs: []faultLog{plainRun("pwsh", faults(1, "16", "pwsh"))}, allowlist: "internal/x|pwsh|any|a reason\n"},
		{name: "an any row and five faults", logs: []faultLog{plainRun("pwsh", faults(5, "16", "pwsh"))}, allowlist: "internal/x|pwsh|any|a reason\n"},
		{name: "an any row for a package no run covers", logs: []faultLog{plainRun("a.test")}, allowlist: "internal/other|pwsh|any|a reason\n", wantExit: 1, contains: []string{"no traced run covers"}},
		{name: "a row for a program that took no fault", logs: []faultLog{plainRun("a.test")}, allowlist: "internal/x|a.test|1|a reason\n", wantExit: 1, contains: []string{"fewer faults"}},
		{name: "a fault in another package than the row", logs: []faultLog{plainRun("rancher", faults(1, "16", "rancher")), {"internal/y", join(logExec("17", "x", "x.test"))}}, allowlist: "internal/y|rancher|1|a reason\n", wantExit: 1, contains: []string{"internal/x\trancher\t1\t-\tFAIL"}},
		{name: "two programs in one package and a row for one", logs: []faultLog{plainRun("a.test", faults(1, "16", "a.test"), logExec("17", "b", "b.test"), faults(1, "17", "b"))}, allowlist: "internal/x|a.test|1|a reason\n", wantExit: 1, contains: []string{"internal/x\tb.test\t1\t-\tFAIL"}},
		{name: "two runs of one package add up to the row", logs: []faultLog{plainRun("a.test", faults(2, "16", "a.test")), plainRun("a.test", faults(3, "16", "a.test"))}, allowlist: "internal/x|a.test|5|a reason\n"},
		{name: "two runs of one package and a row for the larger run", logs: []faultLog{plainRun("a.test", faults(2, "16", "a.test")), plainRun("a.test", faults(3, "16", "a.test"))}, allowlist: "internal/x|a.test|3|a reason\n", wantExit: 1, contains: []string{"more faults"}},
		{name: "two runs of one package and a row for the smaller run", logs: []faultLog{plainRun("a.test", faults(2, "16", "a.test")), plainRun("a.test", faults(3, "16", "a.test"))}, allowlist: "internal/x|a.test|2|a reason\n", wantExit: 1, contains: []string{"more faults"}},
		{name: "a signal sent with kill and with tgkill", logs: []faultLog{plainRun("a.test", logFault("16", "a.test", "SEGV", "SI_USER"), logFault("16", "a.test", "SEGV", "SI_TKILL"))}, contains: []string{"no unexpected faults"}},
		{name: "a general protection fault", logs: []faultLog{plainRun("a.test", logFault("16", "a.test", "SEGV", "SI_KERNEL"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}},
		{name: "a protection fault", logs: []faultLog{plainRun("a.test", logFault("16", "a.test", "SEGV", "SEGV_ACCERR"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}},
		{name: "a bus error", logs: []faultLog{plainRun("a.test", logFault("16", "a.test", "BUS", "BUS_ADRERR"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}},
		{name: "an arithmetic fault", logs: []faultLog{plainRun("a.test", logFault("16", "a.test", "FPE", "FPE_INTDIV"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}},
		{name: "a detached line", logs: []faultLog{plainRun("a.test", "16<a.test> <detached ...>")}, contains: []string{"no unexpected faults"}},
		{name: "an exit line", logs: []faultLog{plainRun("a.test", logExit("16", "a.test"))}, contains: []string{"no unexpected faults"}},
		{name: "an id used again by another program after its exit", logs: []faultLog{plainRun("a.test",
			logExec("23", "faulter.test", "faulter.test"), logExit("23", "faulter.test"),
			logClone("16", "a.test", "23", "a.test"),
			logExec("23", "other.test", "other.test"), faults(1, "23", "other.test"),
		)}, wantExit: 1, contains: []string{"internal/x\tother.test\t1\t-\tFAIL"}, notContains: []string{"a.test\t1"}},
		{name: "a thread renamed to a name with a space and a dot", logs: []faultLog{plainRun("pwsh", faults(1, "16", ".NET SynchManag"))}, allowlist: "internal/x|pwsh|1|a reason\n", contains: []string{"pwsh\t1\t1\tok"}},
		{name: "a program with a long name", logs: []faultLog{{"internal/commands/telemetrycmd", join(logExec("16", "telemetrycmd.te", "telemetrycmd.test"), faults(1, "16", "telemetrycmd.te"))}}, wantExit: 1, contains: []string{"telemetrycmd.test\t1\t-\tFAIL"}},
		{name: "a program started by a parent", logs: []faultLog{plainRun("parent.test", logClone("16", "parent.test", "22", "parent.test"), logExec("22", "parent.test", "child.test"), faults(1, "22", "child.test"))}, wantExit: 1, contains: []string{"internal/x\tchild.test\t1\t-\tFAIL"}, notContains: []string{"parent.test\t1"}},
		{name: "an exec split over two lines", logs: []faultLog{plainRun("a.test", logSplitExec("16", "a.test", "b.test", "0"), faults(1, "16", "b.test"))}, wantExit: 1, contains: []string{"internal/x\tb.test\t1\t-\tFAIL"}},
		{name: "an exec that failed", logs: []faultLog{plainRun("a.test", logFailedExec("16", "a.test", "b.test"), faults(1, "16", "a.test"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}, notContains: []string{"b.test\t1"}},
		{name: "an exec split over two lines that failed", logs: []faultLog{plainRun("a.test", logSplitExec("16", "a.test", "b.test", "-1 ENOENT (No such file or directory)"), faults(1, "16", "a.test"))}, wantExit: 1, contains: []string{"internal/x\ta.test\t1\t-\tFAIL"}, notContains: []string{"b.test\t1"}},
		{name: "a child whose exec lines come before the parent's clone result", logs: []faultLog{plainRun("spawner.test",
			logSplitClone("16", "spawner.test", "18", "23", "spawner.test")[0],
			logSplitExec("23", "spawner.test", "faulter.test", "0"),
			logSplitClone("16", "spawner.test", "18", "23", "spawner.test")[1],
			faults(1, "23", "spawner.test"),
		)}, wantExit: 1, contains: []string{"internal/x\tfaulter.test\t1\t-\tFAIL"}, notContains: []string{"spawner.test\t1"}},
		{name: "an id that exited and became a thread of another program", logs: []faultLog{plainRun("new.test",
			logExec("23", "old.test", "old.test"), logExit("23", "old.test"),
			logClone("16", "new.test", "23", "new.test"),
			faults(1, "23", "new.test"),
		)}, wantExit: 1, contains: []string{"internal/x\tnew.test\t1\t-\tFAIL"}, notContains: []string{"old.test\t1"}},
		{name: "a fault in an id the log never showed", logs: []faultLog{{"internal/x", faults(1, "99", "x")}}, wantExit: 1, contains: []string{"internal/x\t(unknown)\t1\t-\tFAIL"}},
		{name: "a malformed allowlist row", logs: []faultLog{plainRun("a.test")}, allowlist: "internal/x|a.test|1\n", wantExit: 1, contains: []string{"malformed allowlist row"}},
		{name: "a log and its sidecar in one directory", logs: []faultLog{plainRun("a.test", faults(1, "16", "a.test"))}, allowlist: "internal/x|a.test|1|a reason\n", contains: []string{"traced packages (runs):\n  internal/x\t1\n"}},
		{name: "a log with no sidecar", logs: []faultLog{{"", join(logExec("16", "a.test", "a.test"))}}, wantExit: 1, contains: []string{"has no package file"}},
		{name: "a sidecar with no log", logs: []faultLog{plainRun("a.test")}, extraFiles: map[string]string{"stray.pkg": "internal/x\n"}, wantExit: 1, contains: []string{"stray.pkg has no log"}},
		{name: "a program the wrapper shell of fault-trace started", logs: []faultLog{{"internal/x", join(
			logExec("16", "strace", "bash"),
			logClone("16", "bash", "22", "bash"),
			logExec("22", "bash", "a.test"), faults(1, "22", "a.test"),
		)}}, allowlist: "internal/x|a.test|1|a reason\n", contains: []string{"internal/x\ta.test\t1\t1\tok"}, notContains: []string{"bash"}},
		{name: "a note of a repeated run", logs: []faultLog{plainRun("a.test")}, extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, contains: []string{"no unexpected faults\n" + retryNote + "\n"}, notContains: []string{"::warning::"}},
		{name: "a note of a repeated run under GitHub Actions", logs: []faultLog{plainRun("a.test")}, extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, env: []string{"GITHUB_ACTIONS=true"}, contains: []string{retryNote + "\n::warning::" + retryNote + "\n"}},
		{name: "a note and no log at all", extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, wantExit: 1, contains: []string{"nothing was traced", retryNote}},
		{name: "a note and a log with no sidecar", logs: []faultLog{{"", join(logExec("16", "a.test", "a.test"))}}, extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, wantExit: 1, contains: []string{"has no package file", retryNote}},
		{name: "a note and a sidecar with no log", logs: []faultLog{plainRun("a.test")}, extraFiles: map[string]string{"stray.pkg": "internal/x\n", "run.0.retry": retryNote + "\n"}, wantExit: 1, contains: []string{"stray.pkg has no log", retryNote}},
		{name: "a note and a malformed allowlist row", logs: []faultLog{plainRun("a.test")}, allowlist: "internal/x|a.test|1\n", extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, wantExit: 1, contains: []string{"malformed allowlist row", retryNote}},
		{name: "a note of a repeated run and a fault with no row", logs: []faultLog{providerRun(1)}, extraFiles: map[string]string{"run.0.retry": retryNote + "\n"}, wantExit: 1, contains: []string{"provider.test\t1\t-\tFAIL", retryNote}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := writeFaultLogs(t, c.logs)
			for name, content := range c.extraFiles {
				writeTestFile(t, filepath.Join(dir, name), content)
			}
			code, out := runFaultCheck(t, dir, c.allowlist, c.env...)
			assert.Equal(t, c.wantExit, code, out)
			for _, want := range c.contains {
				assert.Contains(t, out, want)
			}
			for _, not := range c.notContains {
				assert.NotContains(t, out, not)
			}
		})
	}

	t.Run("a note and an allowlist file that does not exist", func(t *testing.T) {
		t.Parallel()
		script, err := filepath.Abs(faultCheckScript)
		require.NoError(t, err)
		dir := writeFaultLogs(t, []faultLog{plainRun("a.test")})
		writeTestFile(t, filepath.Join(dir, "run.0.retry"), retryNote+"\n")
		cmd := exec.CommandContext(t.Context(), script)
		cmd.Env = append(testfs.EnvWithoutGit(), "FAULT_LOG_DIR="+dir, "FAULT_ALLOWLIST="+filepath.Join(t.TempDir(), "missing"))
		out, err := cmd.CombinedOutput()
		assert.Equal(t, 1, exitCodeOf(t, err), string(out))
		assert.Contains(t, string(out), "no allowlist file")
		assert.Contains(t, string(out), retryNote)
	})

	t.Run("a log directory that does not exist", func(t *testing.T) {
		t.Parallel()
		code, out := runFaultCheck(t, filepath.Join(t.TempDir(), "missing"), "")
		assert.Equal(t, 1, code, out)
		assert.Contains(t, out, "FAULT_LOG_DIR must name an existing directory")
	})
}

// TestFaultCheckFailsWhenAwkFails asserts that scripts/fault-check fails when
// an awk it runs fails: a log it could not read must not count as a log with
// no fault; it also asserts that the notes of repeated runs are printed on
// that exit too.
// The allowlist row accepts any count: only the failure of awk can make the
// script exit 1.
func TestFaultCheckFailsWhenAwkFails(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	script, err := filepath.Abs(faultCheckScript)
	require.NoError(t, err)
	realAwk, err := exec.LookPath("awk")
	require.NoError(t, err)

	for _, c := range []struct{ name, failOn string }{
		{"the awk that names the program of each fault fails", "*.log"},
		{"the awk that adds the package to each fault fails", "*/names"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tools := toolWrappers(t, "dirname", "mktemp", "cat", "rm")
			writeTestScript(t, filepath.Join(tools, "awk"),
				"#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in "+c.failOn+") exit 2;; esac; done\nexec '"+realAwk+"' \"$@\"\n")
			dir := writeFaultLogs(t, []faultLog{providerRun(1)})
			writeTestFile(t, filepath.Join(dir, "run.0.retry"), "a note of a repeated run\n")
			list := filepath.Join(t.TempDir(), "allowlist")
			writeTestFile(t, list, "internal/client/provider|provider.test|any|a recovered fault\n")
			cmd := exec.CommandContext(t.Context(), script)
			cmd.Env = []string{"PATH=" + tools, "FAULT_LOG_DIR=" + dir, "FAULT_ALLOWLIST=" + list}
			out, err := cmd.CombinedOutput()
			assert.Equal(t, 1, exitCodeOf(t, err), string(out))
			assert.Contains(t, string(out), "awk failed on")
			assert.Contains(t, string(out), "a note of a repeated run", "the notes are printed on this exit too")
		})
	}
}

// toolWrappers writes into a new directory one tiny script per tool that
// runs the tool found now: a PATH made of this directory shows a script
// exactly the tools it names.
func toolWrappers(t *testing.T, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		found, err := exec.LookPath(tool)
		require.NoError(t, err)
		writeTestScript(t, filepath.Join(dir, tool), "#!/bin/sh\nexec '"+found+"' \"$@\"\n")
	}
	return dir
}

// TestFaultTraceRefusals asserts that scripts/fault-trace refuses to run
// without a usable log directory or a program, refuses on a system that is
// not Linux, never runs the program untraced when strace is missing or when
// it cannot create the log file or write the package file, and exits 2
// without starting the program when it cannot read the tracer or reads a
// value that is empty, not a number or has a leading zero. It runs the
// program directly when an outer strace follows the process, and returns the
// program's own exit status and output. It also asserts that a traced
// benchmark run (an argument that starts with -test.bench=) gets
// asyncpreemptoff=1 appended to its GODEBUG, with or without an outer strace,
// and that any other program gets GODEBUG unchanged.
//
// It asserts the repeat after an strace error. A run is repeated once, with a
// note on stderr, a GitHub Actions warning when GITHUB_ACTIONS is set and a
// <name>.retry file, only when strace exited non-zero, printed a line that
// starts "strace: ptrace(", the program did not end on its own (no
// <name>.status) and the log holds no fault. The repeat leaves one log. A
// second strace error fails with strace's status. It asserts that strace's
// stderr goes to <name>.err and is printed after strace ends, while the
// program's stderr is printed as it runs. It asserts that the program gets
// its arguments unchanged, the script's stdin, no fd 3 and the default
// actions of INT and QUIT. It asserts that INT, TERM, QUIT or HUP sent to the
// script kills an strace that ignores them, starts no repeat and exits with
// 128 plus the signal number (with the real strace the program ends too). It
// asserts that a program or an strace that ends on a signal leaves no bash
// job message in the output, and that the wrapper still reports a program
// that cannot start and a status file it cannot write. It asserts that a
// repeat whose first run's files cannot be removed writes no note.
func TestFaultTraceRefusals(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	script, err := filepath.Abs(faultTraceScript)
	require.NoError(t, err)
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)
	tools := toolWrappers(t, "uname", "awk", "dirname", "mktemp")

	run := func(t *testing.T, pathDir, logDir, workDir string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), script, args...)
		cmd.Dir = workDir
		cmd.Env = []string{"PATH=" + pathDir}
		if logDir != "" {
			cmd.Env = append(cmd.Env, "FAULT_LOG_DIR="+logDir)
		}
		out, err := cmd.CombinedOutput()
		return exitCodeOf(t, err), string(out)
	}
	runWithEnv := func(t *testing.T, pathDir, logDir, workDir string, extraEnv []string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), script, args...)
		cmd.Dir = workDir
		cmd.Env = append([]string{"PATH=" + pathDir, "FAULT_LOG_DIR=" + logDir}, extraEnv...)
		out, err := cmd.CombinedOutput()
		return exitCodeOf(t, err), string(out)
	}
	traceTools := func(t *testing.T) string {
		t.Helper()
		return toolWrappers(t, "uname", "awk", "dirname", "mktemp", "cat", "rm")
	}
	stubStrace := func(t *testing.T) string {
		t.Helper()
		dir := traceTools(t)
		writeTestScript(t, filepath.Join(dir, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
		return dir
	}
	// retryStub writes a stand-in strace that counts its runs in the file
	// attempts beside it, exports ATTEMPT (1, 2, ...) to the program and runs
	// the body first on its first run and later on every other run. In a body,
	// "$@" is the command strace was given (the wrapper shell fault-trace
	// starts, then the program) and $log is the log file.
	retryStub := func(t *testing.T, first, later string) string {
		t.Helper()
		dir := traceTools(t)
		writeTestScript(t, filepath.Join(dir, "strace"), "#!/bin/sh\n"+
			"while [ \"$1\" != -- ]; do\n  if [ \"$1\" = -o ]; then log=$2; fi\n  shift\ndone\nshift\n"+
			"n=1\nif [ -e \"${0%/*}/attempts\" ]; then read -r n <\"${0%/*}/attempts\"; n=$((n + 1)); fi\n"+
			"echo \"$n\" >\"${0%/*}/attempts\"\nexport ATTEMPT=\"$n\"\n"+
			"if [ \"$n\" = 1 ]; then\n"+first+"\nelse\n"+later+"\nfi\n")
		return dir
	}
	attempts := func(t *testing.T, stubDir string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(stubDir, "attempts"))
		require.NoError(t, err)
		return strings.TrimSpace(string(data))
	}
	runTraced := func(t *testing.T, pathDir, logDir, stdin string, extraEnv []string, args ...string) (int, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), script, args...)
		cmd.Dir = t.TempDir()
		cmd.Env = append([]string{"PATH=" + pathDir, "FAULT_LOG_DIR=" + logDir}, extraEnv...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return exitCodeOf(t, err), string(out)
	}
	logFiles := func(t *testing.T, logDir, suffix string) []string {
		t.Helper()
		files, err := filepath.Glob(filepath.Join(logDir, "*"+suffix))
		require.NoError(t, err)
		return files
	}

	for _, c := range []struct {
		name, logDir string
		args         []string
	}{
		{"no log directory variable", "none", []string{sh, "-c", "exit 0"}},
		{"a log directory that is a plain file", "file", []string{sh, "-c", "exit 0"}},
		{"a log directory that does not exist", "missing", []string{sh, "-c", "exit 0"}},
		{"no program", "directory", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			logDir := dir
			switch c.logDir {
			case "none":
				logDir = ""
			case "file":
				logDir = filepath.Join(dir, "file")
				writeTestFile(t, logDir, "x")
			case "missing":
				logDir = filepath.Join(dir, "missing")
			}
			code, out := run(t, tools, logDir, ".", c.args...)
			assert.Equal(t, 2, code, out)
		})
	}

	t.Run("a system that is not Linux", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "linux" {
			t.Skip("the refusal is for the other systems")
		}
		code, out := run(t, tools, t.TempDir(), ".", sh, "-c", "exit 0")
		assert.Equal(t, 2, code, out)
		assert.Contains(t, out, "Linux only")
	})

	t.Run("strace is missing", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it looks for strace")
		}
		marker := filepath.Join(t.TempDir(), "started")
		code, out := run(t, tools, t.TempDir(), ".", sh, "-c", "echo started > '"+marker+"'")
		assert.Equal(t, 127, code, out)
		assert.Contains(t, out, "strace is required")
		assert.NoFileExists(t, marker, "the program must not run untraced")
	})

	t.Run("a log file cannot be created", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it creates a log file")
		}
		tools := toolWrappers(t, "uname", "awk", "dirname")
		writeTestScript(t, filepath.Join(tools, "mktemp"), "#!/bin/sh\nexit 1\n")
		writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
		work := t.TempDir()
		marker := filepath.Join(t.TempDir(), "started")
		code, out := run(t, tools, t.TempDir(), work, sh, "-c", "echo started > '"+marker+"'")
		assert.Equal(t, 2, code, out)
		assert.Contains(t, out, "cannot create a log file")
		assert.NoFileExists(t, marker, "the program must not run untraced")
		entries, err := os.ReadDir(work)
		require.NoError(t, err)
		assert.Empty(t, entries, "nothing may be written into the package directory")
	})

	t.Run("a sidecar file cannot be written", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it creates a log file")
		}
		realMktemp, err := exec.LookPath("mktemp")
		require.NoError(t, err)
		realMkdir, err := exec.LookPath("mkdir")
		require.NoError(t, err)
		tools := toolWrappers(t, "uname", "awk", "dirname")
		writeTestScript(t, filepath.Join(tools, "mktemp"),
			"#!/bin/sh\nf=$('"+realMktemp+"' \"$@\") || exit 1\n'"+realMkdir+"' \"$f.pkg\" || exit 1\nprintf '%s\n' \"$f\"\n")
		writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
		marker := filepath.Join(t.TempDir(), "started")
		code, out := run(t, tools, t.TempDir(), t.TempDir(), sh, "-c", "echo started > '"+marker+"'")
		assert.Equal(t, 2, code, out)
		assert.Contains(t, out, "cannot write")
		assert.NoFileExists(t, marker, "the program must not run untraced")
	})

	for _, c := range []struct {
		name, awkScript string
		wantCode        int
		wantStarted     bool
		wantOut         string
	}{
		{"awk fails while it reads the tracer", "exit 1", 2, false, "cannot read /proc/self/status"},
		{"the tracer value is not a number", "echo unknown", 2, false, "unexpected TracerPid value"},
		{"the tracer value is empty", "exit 0", 2, false, "unexpected TracerPid value"},
		{"the tracer value has a leading zero", "echo 00", 2, false, "unexpected TracerPid value"},
		{"an outer strace already follows the process", "echo 4242", 0, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it reads the tracer")
			}
			tools := toolWrappers(t, "uname", "dirname", "mktemp")
			writeTestScript(t, filepath.Join(tools, "awk"), "#!/bin/sh\n"+c.awkScript+"\n")
			writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
			marker := filepath.Join(t.TempDir(), "started")
			logDir := t.TempDir()
			code, out := run(t, tools, logDir, t.TempDir(), sh, "-c", "echo started > '"+marker+"'")
			assert.Equal(t, c.wantCode, code, out)
			assert.Contains(t, out, c.wantOut)
			if c.wantStarted {
				assert.FileExists(t, marker)
			} else {
				assert.NoFileExists(t, marker, "the program must not run untraced")
			}
			entries, err := os.ReadDir(logDir)
			require.NoError(t, err)
			assert.Empty(t, entries, "no log is written before the tracer is known")
		})
	}

	t.Run("the program's exit status", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		code, out := run(t, stubStrace(t), t.TempDir(), ".", sh, "-c", "exit 3")
		assert.Equal(t, 3, code, out)
	})

	t.Run("the program's output", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		code, out := run(t, stubStrace(t), t.TempDir(), ".", sh, "-c", "echo hello from the program")
		assert.Equal(t, 0, code, out)
		assert.Contains(t, out, "hello from the program")
	})

	t.Run("the package of the traced run", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		logDir := t.TempDir()
		root, err := os.Getwd()
		require.NoError(t, err)
		code, out := run(t, stubStrace(t), logDir, filepath.Join(root, "internal", "client", "provider"), sh, "-c", "exit 0")
		require.Equal(t, 0, code, out)
		sidecars, err := filepath.Glob(filepath.Join(logDir, "*.pkg"))
		require.NoError(t, err)
		require.Len(t, sidecars, 1)
		data, err := os.ReadFile(sidecars[0])
		require.NoError(t, err)
		assert.Equal(t, "internal/client/provider\n", string(data))
	})

	const printGODEBUG = `printf '%s\n' "$GODEBUG"`
	for _, c := range []struct {
		name       string
		args       []string
		env        []string
		outerTrace bool
		want       string
	}{
		{"a benchmark run turns async preemption off", []string{"-test.bench=BenchmarkX$"}, nil, false, "asyncpreemptoff=1"},
		{"a benchmark run keeps the caller's GODEBUG", []string{"-test.bench=BenchmarkX$"}, []string{"GODEBUG=foo=1"}, false, "foo=1,asyncpreemptoff=1"},
		{"a benchmark run overrides the caller's asyncpreemptoff", []string{"-test.bench=BenchmarkX$"}, []string{"GODEBUG=asyncpreemptoff=0"}, false, "asyncpreemptoff=0,asyncpreemptoff=1"},
		{"a benchmark run with an empty GODEBUG", []string{"-test.bench=BenchmarkX$"}, []string{"GODEBUG="}, false, "asyncpreemptoff=1"},
		{"a benchmark argument between other test arguments", []string{"-test.run=^$", "-test.bench=.", "-test.count=10"}, nil, false, "asyncpreemptoff=1"},
		{"a test run leaves GODEBUG unchanged", []string{"-test.run=TestX", "-test.count=1"}, nil, false, ""},
		{"a test run keeps the caller's GODEBUG", []string{"-test.run=TestX"}, []string{"GODEBUG=foo=1"}, false, "foo=1"},
		{"a benchtime without a benchmark leaves GODEBUG unchanged", []string{"-test.benchtime=200x", "-test.benchmem"}, nil, false, ""},
		{"a benchmark run under an outer strace", []string{"-test.bench=."}, nil, true, "asyncpreemptoff=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			pathDir := stubStrace(t)
			if c.outerTrace {
				pathDir = toolWrappers(t, "uname", "dirname", "mktemp")
				writeTestScript(t, filepath.Join(pathDir, "awk"), "#!/bin/sh\necho 4242\n")
			}
			args := append([]string{sh, "-c", printGODEBUG, "sh"}, c.args...)
			code, out := runWithEnv(t, pathDir, t.TempDir(), t.TempDir(), c.env, args...)
			assert.Equal(t, 0, code, out)
			assert.Equal(t, c.want+"\n", out)
		})
	}

	const (
		ptraceError = "strace: ptrace(PTRACE_LISTEN,pid:42,sig:0): Input/output error"
		notePrefix  = "scripts/fault-trace: strace stopped with an error while tracing the tests of "
		repeated    = "the run is repeated once. strace said: " + ptraceError
		// A program that kills its wrapper on the first traced run, as the
		// kernel kills every traced program when strace stops on an strace
		// error, and ends on its own on every later run.
		killFirst = `echo "program run $ATTEMPT"; if [ "$ATTEMPT" = 1 ]; then kill -KILL $PPID; fi`
		killEvery = `echo "program run $ATTEMPT"; kill -KILL $PPID`
		logEntry  = `echo "attempt $ATTEMPT" >>"$log"`
		passBody  = `"$@"; s=$?; ` + logEntry + `; exit $s`
		faultBody = `"$@"; ` + logEntry + `; echo '1<a.test> --- SIGSEGV {si_signo=SIGSEGV, si_code=SEGV_MAPERR, si_addr=NULL} ---' >>"$log"; echo '` + ptraceError + `' >&2; exit 1`
	)
	const noLogBody = `"$@"; echo '` + ptraceError + `' >&2; exit 1`
	failBody := func(line string) string { return `"$@"; ` + logEntry + `; echo '` + line + `' >&2; exit 1` }
	for _, c := range []struct {
		name, first, later, program string
		env                         []string
		wantCode                    int
		wantAttempts                string
		repeat                      bool
		contains, notContains       []string
	}{
		{"an strace error before the program ends is repeated once", failBody(ptraceError), passBody, killFirst, nil, 0, "2", true,
			[]string{"program run 1", "program run 2", ptraceError + "\n", notePrefix, repeated}, []string{"::warning::"}},
		{"a repeated run under GitHub Actions", failBody(ptraceError), passBody, killFirst, []string{"GITHUB_ACTIONS=true"}, 0, "2", true,
			[]string{"::warning::" + notePrefix, repeated}, nil},
		{"a fault in the first run is not repeated", faultBody, passBody, killFirst, nil, 1, "1", false,
			[]string{ptraceError}, []string{"program run 2", repeated}},
		{"a second strace error is not repeated", failBody(ptraceError), failBody(ptraceError), killEvery, nil, 1, "2", true,
			[]string{"program run 2", repeated}, nil},
		{"an strace error after the program ended is not repeated", failBody(ptraceError), passBody, `echo "program run $ATTEMPT"`, nil, 1, "1", false,
			[]string{ptraceError}, []string{"program run 2", repeated}},
		{"an strace error and no log is not repeated", noLogBody, passBody, killFirst, nil, 1, "1", false,
			[]string{ptraceError}, []string{"program run 2", "is repeated"}},
		{"an strace line that names no ptrace request is not repeated", failBody("strace: exec: No such file or directory"), passBody, killFirst, nil, 1, "1", false,
			[]string{"strace: exec: No such file or directory"}, []string{"program run 2", "is repeated"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			stub := retryStub(t, c.first, c.later)
			logDir := t.TempDir()
			code, out := runTraced(t, stub, logDir, "", c.env, sh, "-c", c.program)
			assert.Equal(t, c.wantCode, code, out)
			assert.Equal(t, c.wantAttempts, attempts(t, stub), out)
			for _, want := range c.contains {
				assert.Contains(t, out, want)
			}
			for _, not := range c.notContains {
				assert.NotContains(t, out, not)
			}
			notes := logFiles(t, logDir, ".retry")
			if !c.repeat {
				assert.Empty(t, notes, "a run that is not repeated leaves no note")
				return
			}
			require.Len(t, notes, 1)
			note, err := os.ReadFile(notes[0])
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(string(note), notePrefix), string(note))
			assert.True(t, strings.HasSuffix(string(note), repeated+"\n"), string(note))
			assert.Equal(t, 1, strings.Count(string(note), "\n"), "one note line")
			logs := logFiles(t, logDir, ".log")
			require.Len(t, logs, 1, "the first run's log is removed")
			assert.Len(t, logFiles(t, logDir, ".pkg"), 1)
			data, err := os.ReadFile(logs[0])
			require.NoError(t, err)
			assert.Equal(t, "attempt 2\n", string(data), "the log holds the repeated run only")
		})
	}

	t.Run("strace's stderr and the program's stderr", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		tools := traceTools(t)
		writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\necho 'strace: a line of strace' >&2\n\"$@\"\n")
		logDir := t.TempDir()
		code, out := runTraced(t, tools, logDir, "", nil, sh, "-c", "echo a line of the program >&2")
		require.Equal(t, 0, code, out)
		assert.Equal(t, "a line of the program\nstrace: a line of strace\n", out, "the program's line is printed as it runs, strace's after strace ends")
		errs := logFiles(t, logDir, ".err")
		require.Len(t, errs, 1)
		data, err := os.ReadFile(errs[0])
		require.NoError(t, err)
		assert.Equal(t, "strace: a line of strace\n", string(data))
	})

	t.Run("a repeat whose first run's files cannot be removed leaves no note", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		stub := retryStub(t, failBody(ptraceError), passBody)
		writeTestScript(t, filepath.Join(stub, "rm"), "#!/bin/sh\nexit 1\n")
		logDir := t.TempDir()
		code, out := runTraced(t, stub, logDir, "", nil, sh, "-c", killFirst)
		assert.Equal(t, 1, code, out)
		assert.Equal(t, "1", attempts(t, stub), out)
		assert.Contains(t, out, "cannot remove the files of the first traced run")
		assert.NotContains(t, out, "is repeated")
		assert.Empty(t, logFiles(t, logDir, ".retry"))
	})

	for _, c := range []struct{ name, strace, program string }{
		{"a program that ends on SIGSEGV", "", "kill -SEGV $$"},
		{"a program that ends on SIGKILL", "", "kill -KILL $$"},
		{"an strace that ends on SIGKILL", "kill -KILL $$", "true"},
	} {
		t.Run(c.name+" prints no job message", func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			tools := stubStrace(t)
			if c.strace != "" {
				writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\n"+c.strace+"\n")
			}
			code, out := runTraced(t, tools, t.TempDir(), "", nil, sh, "-c", c.program)
			assert.Contains(t, []int{128 + int(syscall.SIGSEGV), 128 + int(syscall.SIGKILL)}, code, out)
			assert.Empty(t, out, "the exit status is the only report")
		})
	}

	t.Run("a program that cannot start is reported", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		code, out := runTraced(t, stubStrace(t), t.TempDir(), "", nil, filepath.Join(t.TempDir(), "missing"))
		assert.Equal(t, 127, code, out)
		assert.Contains(t, out, "No such file or directory")
	})

	t.Run("a status file that cannot be written is reported", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		tools := toolWrappers(t, "uname", "awk", "dirname", "mktemp", "cat", "rm", "mkdir")
		writeTestScript(t, filepath.Join(tools, "strace"), "#!/bin/sh\n"+
			"while [ \"$1\" != -- ]; do\n  if [ \"$1\" = -o ]; then log=$2; fi\n  shift\ndone\nshift\n"+
			"mkdir \"${log%.log}.status\"\n\"$@\"\n")
		code, out := runTraced(t, tools, t.TempDir(), "", nil, sh, "-c", "true")
		assert.Equal(t, 0, code, out)
		assert.Contains(t, out, "Is a directory")
	})

	for _, c := range []struct{ name, stdin, program, want string }{
		{"an argument with a space, a quote and a dollar sign", "", `printf '%s\n' "$1"`, "a b\"c$d'e\n"},
		{"the program reads the script's stdin", "a line\n", `read -r x; echo "read: $x"`, "read: a line\n"},
		{"the program gets no fd 3", "", `if (true >&3) 2>/dev/null; then echo fd 3 is open; else echo fd 3 is closed; fi`, "fd 3 is closed\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			code, out := runTraced(t, stubStrace(t), t.TempDir(), c.stdin, nil, sh, "-c", c.program, "sh", `a b"c$d'e`)
			assert.Equal(t, 0, code, out)
			assert.Equal(t, c.want, out)
		})
	}

	// ignored reports whether this process ignores the signal: a program
	// inherits an ignored signal, and fault-trace keeps it ignored.
	ignored := func(t *testing.T, sig syscall.Signal) bool {
		t.Helper()
		data, err := os.ReadFile("/proc/self/status")
		require.NoError(t, err)
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "SigIgn:"); ok {
				mask, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
				require.NoError(t, err)
				return mask&(1<<(uint(sig)-1)) != 0
			}
		}
		require.Fail(t, "no SigIgn line in /proc/self/status")
		return false
	}
	signals := []struct {
		name string
		sig  syscall.Signal
	}{{"HUP", syscall.SIGHUP}, {"INT", syscall.SIGINT}, {"QUIT", syscall.SIGQUIT}, {"TERM", syscall.SIGTERM}}

	for _, c := range signals {
		if c.sig != syscall.SIGINT && c.sig != syscall.SIGQUIT {
			continue
		}
		t.Run("the program gets the default action of "+c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			if ignored(t, c.sig) {
				t.Skip("this test process ignores SIG" + c.name + " and the script inherits that")
			}
			code, out := runTraced(t, stubStrace(t), t.TempDir(), "", nil, sh, "-c", "kill -"+c.name+" $$; echo the program was not stopped")
			assert.Equal(t, 128+int(c.sig), code, out)
			assert.NotContains(t, out, "the program was not stopped")
		})
	}

	// signalRun starts the script with a program that waits on its stdin,
	// sends the signal to the script once the file ready exists, and returns
	// the script's exit status and output, the write end of the program's
	// stdin and the log directory.
	signalRun := func(t *testing.T, pathDir, ready string, sig syscall.Signal, program string) (int, string, *os.File, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		stdinR, stdinW, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = stdinW.Close() })
		outPath := filepath.Join(t.TempDir(), "out")
		outFile, err := os.Create(outPath)
		require.NoError(t, err)
		defer func() { _ = outFile.Close() }()
		logDir := t.TempDir()
		cmd := exec.CommandContext(ctx, script, sh, "-c", program)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + pathDir, "FAULT_LOG_DIR=" + logDir}
		cmd.Stdin = stdinR
		cmd.Stdout = outFile
		cmd.Stderr = outFile
		require.NoError(t, cmd.Start())
		require.NoError(t, stdinR.Close())
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(ready)
			return err == nil && len(data) > 0
		}, 20*time.Second, 10*time.Millisecond, "the program did not start")
		require.NoError(t, cmd.Process.Signal(sig))
		code := exitCodeOf(t, cmd.Wait())
		out, err := os.ReadFile(outPath)
		require.NoError(t, err)
		return code, string(out), stdinW, logDir
	}

	for _, c := range signals {
		t.Run(c.name+" to the script kills strace and starts no repeat", func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS != "linux" {
				t.Skip("the script refuses before it runs strace")
			}
			ready := filepath.Join(t.TempDir(), "ready")
			// A stand-in strace that ignores the four signals, as strace does
			// while it traces, and leaves every condition of a repeat in place.
			body := `trap '' HUP INT QUIT TERM; echo '` + ptraceError + `' >&2; : >"$log"; echo up >'` + ready + `'; "$@"`
			stub := retryStub(t, body, body)
			code, out, stdinW, logDir := signalRun(t, stub, ready, c.sig, "read -r x")
			assert.Equal(t, 128+int(c.sig), code, out)
			assert.Equal(t, "1", attempts(t, stub), out)
			assert.Contains(t, out, ptraceError, "strace's stderr is printed")
			assert.NotContains(t, out, "is repeated")
			// The wrapper and the program outlive the stand-in strace. The
			// program ends at the end of its stdin; the wrapper then writes
			// the status file, its last write into the test's directories.
			require.NoError(t, stdinW.Close())
			assert.Eventually(t, func() bool {
				files, err := filepath.Glob(filepath.Join(logDir, "*.status"))
				return err == nil && len(files) == 1
			}, 20*time.Second, 10*time.Millisecond, "the wrapper did not end")
		})
	}

	t.Run("TERM to the script ends the program under the real strace", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS != "linux" {
			t.Skip("the script refuses before it runs strace")
		}
		realStrace, err := exec.LookPath("strace")
		if err != nil {
			t.Skip("strace is not installed")
		}
		probe := exec.CommandContext(t.Context(), realStrace, "--seccomp-bpf", "-f", "-o", os.DevNull, "--", sh, "-c", "exit 0")
		probe.Env = []string{"PATH=" + filepath.Dir(sh)}
		if out, err := probe.CombinedOutput(); err != nil {
			t.Skipf("strace cannot trace here: %v: %s", err, out)
		}
		tools := toolWrappers(t, "uname", "awk", "dirname", "mktemp", "cat", "rm", "strace")
		ready := filepath.Join(t.TempDir(), "ready")
		code, out, stdinW, _ := signalRun(t, tools, ready, syscall.SIGTERM, "echo $$ >'"+ready+"'; read -r x")
		assert.Equal(t, 128+int(syscall.SIGTERM), code, out)
		// The script, strace, the wrapper and the program were the only
		// holders of the read end of the program's stdin.
		assert.Eventually(t, func() bool {
			_, err := stdinW.Write([]byte("x"))
			return errors.Is(err, syscall.EPIPE)
		}, 20*time.Second, 20*time.Millisecond, "the program is still running")
	})
}
