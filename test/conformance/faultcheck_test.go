package conformance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

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

func runFaultCheck(t *testing.T, dir, allowlist string) (int, string) {
	t.Helper()
	script, err := filepath.Abs(faultCheckScript)
	require.NoError(t, err)
	list := filepath.Join(t.TempDir(), "allowlist")
	writeTestFile(t, list, allowlist)
	cmd := exec.CommandContext(t.Context(), script)
	cmd.Env = append(os.Environ(), "FAULT_LOG_DIR="+dir, "FAULT_ALLOWLIST="+list)
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
// files in the shapes strace prints.
func TestFaultCheckBehaviour(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, posixOnlyReason)
	const providerRow = "internal/client/provider|provider.test|%d|a recovered fault\n"
	row := func(n int) string { return fmt.Sprintf(providerRow, n) }
	plainRun := func(program string, lines ...any) faultLog {
		return faultLog{"internal/x", join(append([]any{logExec("16", program, program)}, lines...)...)}
	}

	for _, c := range []struct {
		name        string
		logs        []faultLog
		extraFiles  map[string]string
		allowlist   string
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
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := writeFaultLogs(t, c.logs)
			for name, content := range c.extraFiles {
				writeTestFile(t, filepath.Join(dir, name), content)
			}
			code, out := runFaultCheck(t, dir, c.allowlist)
			assert.Equal(t, c.wantExit, code, out)
			for _, want := range c.contains {
				assert.Contains(t, out, want)
			}
			for _, not := range c.notContains {
				assert.NotContains(t, out, not)
			}
		})
	}

	t.Run("a log directory that does not exist", func(t *testing.T) {
		t.Parallel()
		code, out := runFaultCheck(t, filepath.Join(t.TempDir(), "missing"), "")
		assert.Equal(t, 1, code, out)
		assert.Contains(t, out, "FAULT_LOG_DIR must name an existing directory")
	})
}

// TestFaultCheckFailsWhenAwkFails asserts that scripts/fault-check fails when
// an awk it runs fails: a log it could not read must not count as a log with
// no fault. The allowlist row accepts any count: only the failure of awk can
// make the script exit 1.
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
			list := filepath.Join(t.TempDir(), "allowlist")
			writeTestFile(t, list, "internal/client/provider|provider.test|any|a recovered fault\n")
			cmd := exec.CommandContext(t.Context(), script)
			cmd.Env = []string{"PATH=" + tools, "FAULT_LOG_DIR=" + dir, "FAULT_ALLOWLIST=" + list}
			out, err := cmd.CombinedOutput()
			assert.Equal(t, 1, exitCodeOf(t, err), string(out))
			assert.Contains(t, string(out), "awk failed on")
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
	stubStrace := func(t *testing.T) string {
		t.Helper()
		dir := toolWrappers(t, "uname", "awk", "dirname", "mktemp")
		writeTestScript(t, filepath.Join(dir, "strace"), "#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n")
		return dir
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
}
