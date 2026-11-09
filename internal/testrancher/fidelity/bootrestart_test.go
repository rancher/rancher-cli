package fidelity

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The oracle container's output in the cases that restart it: the FATAL line
// of an earlier run, a line, the line that says why the server exited last
// with a token value that the redaction filter must remove, and a later line.
// The restart prints the last FATAL line of the output, never the earlier one.
const (
	bootTokenValue = "bootsecret-7d41"
	bootOlderFatal = "earlier failure"
	bootFatalLines = "2026-11-09T05:20:10Z [FATAL] " + bootOlderFatal + "\n" +
		"2026-11-09T05:26:02Z [INFO] starting\n" +
		"2026-11-09T05:26:46Z [FATAL] start failed token=" + bootTokenValue + "\n" +
		"2026-11-09T05:26:50Z [INFO] starting again\n"
	bootFatalPrinted = "[rancher] 2026-11-09T05:26:46Z [FATAL] start failed token=[redacted]"
)

// newBootRun is a fresh boot of the slot that ends at the read of the CA: the
// CA read gets a 503 and no time to wait, so a run that passes both boot waits
// and the policy change exits 1 there. A change that comes after the CA read
// never runs, and a case that needs it fails. In suite mode the run has no
// E2E_FIDELITY_KEEP; kept, it has it and no state file, so it boots a new pair.
func newBootRun(t *testing.T, slot keepSlot, kept bool) *keepRun {
	t.Helper()
	k := newKeepRun(t, slot)
	k.noState = true
	k.noKeep = !kept
	delete(k.s.env, "STUB_FAIL_RUN")
	k.canned("/cacerts", "503", "")
	k.editScript("\nCA_LIMIT_SECONDS=300\n", "\nCA_LIMIT_SECONDS=0\n")
	return k
}

// passWaits makes the answers of both boot waits come at the first read.
func passWaits(k *keepRun) {
	k.ping = "pong"
	k.s.helm.files["webhook_ready"] = "1"
}

// finalPolicy is the restart policy of the container at the end of the log:
// the value of --restart on its docker run (none means no), then every later
// docker update of it.
func finalPolicy(log []string, container string) string {
	policy := "no"
	for _, line := range log {
		fields := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "run -d --name "+container+" "):
			policy = "no"
			if i := slices.Index(fields, "--restart"); i >= 0 && i+1 < len(fields) {
				policy = fields[i+1]
			}
		case len(fields) == 4 && fields[0] == "update" && fields[1] == "--restart" && fields[3] == container:
			policy = fields[2]
		}
	}
	return policy
}

// runLine is the docker run of the container, or empty.
func runLine(log []string, container string) string {
	for _, line := range log {
		if strings.HasPrefix(line, "run -d --name "+container+" ") {
			return line
		}
	}
	return ""
}

// updates are the docker update calls of the log.
func updates(log []string) []string {
	var out []string
	for _, line := range log {
		if strings.HasPrefix(line, "update ") {
			out = append(out, line)
		}
	}
	return out
}

// pingReads is the number of reads of /ping the curl stub saw.
func pingReads(k *keepRun) int {
	n := 0
	for _, line := range k.s.curlLog() {
		if strings.HasSuffix(line, "/ping") {
			n++
		}
	}
	return n
}

func restartLine(container string, count int) string {
	return "==> the oracle container " + container + " restarted (restart count " + strconv.Itoa(count) + "); the last FATAL line in the container output:\n"
}

// TestEveryBootEndsWithItsRestartPolicy asserts, on every slot and every boot,
// the policy the oracle container ends with once its boot checks pass. A run
// without E2E_FIDELITY_KEEP boots a container slot with --restart on-failure:3
// and turns the policy off with one docker update before the CA read; a helm
// boot gives its node container no policy and makes no update. A kept
// container and its node container keep --restart unless-stopped and no
// update is made. Every run restarts the container once during the boot: the
// restart prints its count and the last FATAL line of the container's output,
// with the token value removed.
//
// The guard cannot see: what Docker does with a restart policy (on-failure
// restarts only a non-zero exit, at most the given number of times; a
// container with a restart pending shows as restarting, never as exited;
// a restarted container's output holds its earlier runs, and nothing in it
// marks where one run ends, so the restart prints the last FATAL line of the
// whole output; docker update on a running container takes effect without a
// restart and can be refused on a restarting one; the restart count is a
// field of docker inspect); a reused kept pair (the forbidden update call of
// its test asserts a change that does not exist today); a Rancher exit after
// the update; Rancher pod restarts under helm; a pin switched to helm; and the
// downstream container of a suite run, which no row reaches. A runtime that
// reports exited before it restarts the container is not supported: the waits
// stop at that read.
func TestEveryBootEndsWithItsRestartPolicy(t *testing.T) {
	t.Parallel()
	requireKeepSlotsMatchTheScript(t)
	for _, slot := range keepSlots {
		for _, kept := range []bool{false, true} {
			mode := "suite"
			if kept {
				mode = "kept"
			}
			t.Run(slot.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				k := newBootRun(t, slot, kept)
				passWaits(k)
				k.s.helm.files["restart_seq"] = "running 1\n"
				k.s.helm.files["logs"] = bootFatalLines
				res := k.run()

				require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
				assert.Contains(t, res.stderr, "/cacerts did not return a certificate", "the run reached the CA read")
				assert.Equal(t, 1, strings.Count(res.stdout, "restarted (restart count"), "one restart line: %s", res.stdout)
				assert.Contains(t, res.stdout, restartLine(slot.container, 1)+bootFatalPrinted+"\n", "the line right after the restart header is the last FATAL line, not the earlier %q", bootOlderFatal)
				assert.NotContains(t, res.stdout, bootTokenValue)
				assert.NotContains(t, res.stderr, bootTokenValue)

				run := runLine(res.log, slot.container)
				require.NotEmptyf(t, run, "the oracle container is started: %v", res.log)
				switch {
				case kept:
					assert.Contains(t, run, " --restart unless-stopped ")
					assert.Empty(t, updates(res.log), "a kept container keeps its policy")
					assert.Equal(t, "unless-stopped", finalPolicy(res.log, slot.container))
				case slot.boot == "container":
					assert.Contains(t, run, " --restart on-failure:3 ")
					assert.Equal(t, []string{"update --restart no " + slot.container}, updates(res.log))
					assert.Equal(t, "no", finalPolicy(res.log, slot.container))
				default:
					assert.NotContains(t, run, "--restart", "a node container gets no policy")
					assert.Empty(t, updates(res.log), "a node container has no policy to turn off")
					assert.Equal(t, "no", finalPolicy(res.log, slot.container))
				}
			})
		}
	}
}

// TestBootWaitsReadTheOracleContainer asserts that each pass of the two boot
// waits reads the oracle container: an exited container ends the wait at once
// with the report of a stopped oracle (exit 5), in both waits and both boots;
// more than 3 restarts of a kept container end it the same way; each restart
// prints its count; and the count is not read from the environment, a failed or
// unreadable read ends nothing, and a failed update is a failure of the run
// (exit 5 only when the container is restarting).
func TestBootWaitsReadTheOracleContainer(t *testing.T) {
	t.Parallel()
	requireKeepSlotsMatchTheScript(t)
	slotNamed := func(name string) keepSlot {
		i := slices.IndexFunc(keepSlots, func(s keepSlot) bool { return s.name == name })
		require.GreaterOrEqual(t, i, 0, name)
		return keepSlots[i]
	}

	for _, slot := range keepSlots {
		t.Run("keeps exiting/"+slot.name, func(t *testing.T) {
			t.Parallel()
			k := newBootRun(t, slot, false)
			k.ping = "no reply"
			k.s.helm.files["restart_seq"] = "restarting 1\nrunning 2\nrunning 3\nexited 3\n"
			k.s.helm.files["node_state"] = "exited"
			k.s.helm.files["logs"] = bootFatalLines
			res := k.run()

			require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" stopped during the boot (status exited, restart count 3)")
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" is not running (status exited, exit code 0, OOMKilled false)")
			for count := 1; count <= 3; count++ {
				assert.Contains(t, res.stdout, restartLine(k.slot.container, count)+bootFatalPrinted+"\n")
			}
			assert.Equal(t, 3, pingReads(k), "the wait stops at the pass that finds the container exited")
			assert.Empty(t, updates(res.log))
		})
	}

	for _, slot := range keepSlots {
		t.Run("stops in the webhook wait/"+slot.name, func(t *testing.T) {
			t.Parallel()
			k := newBootRun(t, slot, false)
			k.ping = "pong"
			k.s.helm.files["webhook_ready"] = ""
			// A helm slot stops on a dead container, the others on an exited
			// one; the wait for /ping covers exited on every slot.
			status := "exited"
			if slot.boot == "helm" {
				status = "dead"
			}
			k.s.helm.files["restart_seq"] = "running 0\n" + status + " 0\n"
			k.s.helm.files["node_state"] = status
			res := k.run()

			require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" stopped during the boot (status "+status+", restart count 0)")
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" is not running (status "+status+", ")
			assert.NotContains(t, res.stderr, "rancher-webhook not ready")
			assert.False(t, hasCall(res.log, "exec "+k.slot.container+" kubectl -n cattle-system get deploy rancher-webhook"), "no webhook read: %v", res.log)
			assert.Empty(t, updates(res.log))
		})
	}

	for _, slot := range keepSlots {
		t.Run("past 3 restarts/"+slot.name, func(t *testing.T) {
			t.Parallel()
			k := newBootRun(t, slot, true)
			k.ping = "no reply"
			k.s.helm.files["restart_seq"] = "running 1\nrunning 2\nrunning 3\nrunning 4\n"
			k.s.helm.files["node_state"] = "running"
			k.s.helm.files["logs"] = bootFatalLines
			res := k.run()

			require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" restarted 4 times during the boot\n")
			assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" restarted 4 times during the boot (status running, exit code 0, OOMKilled false); its state and last log lines follow\n")
			assert.Contains(t, res.stderr, "error: a kept container restarts until it is removed; remove it, then run again:\nerror:   docker rm -f -v "+k.slot.container+"\n")
			assert.Equal(t, 3, pingReads(k))
			assert.Empty(t, updates(res.log))
		})
	}

	t.Run("past 3 restarts and restarting at the report/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), true)
		k.ping = "no reply"
		k.s.helm.files["restart_seq"] = "running 1\nrunning 2\nrunning 3\nrunning 4\n"
		k.s.helm.files["node_state"] = "restarting"
		res := k.run()

		require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" is not running (status restarting, exit code 0, OOMKilled false); its state and last log lines follow\n")
		assert.Contains(t, res.stderr, "error: a kept container restarts until it is removed; remove it, then run again:\nerror:   docker rm -f -v "+k.slot.container+"\n")
		assert.NotContains(t, res.stderr, "restarted 4 times during the boot (status")
	})

	for _, name := range []string{"community", "helm-check"} {
		t.Run("a restart in the last pass is printed/"+name, func(t *testing.T) {
			t.Parallel()
			k := newBootRun(t, slotNamed(name), false)
			passWaits(k)
			k.s.helm.files["restart_seq"] = "running 0\nrunning 0\nrunning 1\n"
			k.s.helm.files["logs"] = bootFatalLines
			res := k.run()

			require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
			assert.Equal(t, 1, strings.Count(res.stdout, "restarted (restart count"), "one restart line: %s", res.stdout)
			assert.Contains(t, res.stdout, restartLine(k.slot.container, 1)+bootFatalPrinted+"\n")
			assert.Contains(t, res.stderr, "/cacerts did not return a certificate", "the run goes on to the CA read")
		})
	}

	t.Run("an exit in the last pass stops the run/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), false)
		passWaits(k)
		k.s.helm.files["restart_seq"] = "running 0\nrunning 0\nexited 0\n"
		k.s.helm.files["node_state"] = "exited"
		res := k.run()

		require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" stopped during the boot (status exited, restart count 0)")
		assert.Empty(t, updates(res.log))
	})

	t.Run("the environment cannot set the count/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), false)
		k.ping = "no reply"
		k.s.env["ORACLE_RESTARTS"] = "9"
		k.s.helm.files["restart_seq"] = "running 0\n"
		res := k.run()

		require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Equal(t, 90, pingReads(k))
		assert.Contains(t, res.stderr, "error: rancher did not answer /ping within ~7.5 minutes")
		assert.NotContains(t, res.stderr, "restarted")
	})

	t.Run("an unreadable state ends nothing/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), false)
		k.ping = "no reply"
		k.s.helm.files["restart_seq"] = "fail\njunk\n"
		res := k.run()

		require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Equal(t, 90, pingReads(k))
		assert.NotContains(t, res.stdout, "restarted (restart count")
		assert.NotContains(t, res.stderr, "stopped during the boot")
		assert.Equal(t, "error: rancher did not answer /ping within ~7.5 minutes\n", res.stderr, "the shell raised no error on the unreadable count")
	})

	t.Run("a failed update fails the run/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), false)
		passWaits(k)
		k.s.helm.files["node_state"] = "running"
		k.s.helm.files["update_fail"] = "1"
		res := k.run()

		require.Equalf(t, 1, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Equal(t, []string{"update --restart no " + k.slot.container}, updates(res.log))
		for _, line := range k.s.curlLog() {
			assert.NotContains(t, line, "/cacerts", "the run stops before the CA read")
		}
	})

	t.Run("a failed update on a restarting container exits 5/community", func(t *testing.T) {
		t.Parallel()
		k := newBootRun(t, slotNamed("community"), false)
		passWaits(k)
		k.s.helm.files["node_state"] = "restarting"
		k.s.helm.files["update_fail"] = "1"
		res := k.run()

		require.Equalf(t, 5, res.code, "stdout: %s\nstderr: %s", res.stdout, res.stderr)
		assert.Equal(t, []string{"update --restart no " + k.slot.container}, updates(res.log))
		assert.Contains(t, res.stderr, "error: the oracle container "+k.slot.container+" is not running (status restarting")
	})
}

// TestRestartRemedyNamesTheContainerAsTheShellQuotesIt asserts that the command
// the report of an oracle container that restarted more than 3 times prints
// is valid shell and runs exactly docker rm -f -v with the container name as
// one word, whatever characters the name holds.
func TestRestartRemedyNamesTheContainerAsTheShellQuotesIt(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	for label, name := range map[string]string{
		"plain":       "plain",
		"a space":     "name with space",
		"quotes":      `quote'd "name"`,
		"expansions":  "dollar$HOME and `tick`",
		"a backslash": `back\slash`,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			e := newShellEnv(t)
			ctl := filepath.Join(e.dir, "ctl")
			require.NoError(t, os.MkdirAll(ctl, 0o700))
			testfs.WriteExecutable(t, filepath.Join(e.dir, "docker"), []byte(dumpDocker(ctl)))
			body := "CONTAINER=" + shellWord(t, name) + "\nORACLE_BOOTED=1\nORACLE_RESTARTS=4\nexit 1"
			_, stderr, code, _ := e.run(e.cleanupProgram(0, 0, body))
			require.Equal(t, 5, code, "stderr: %s", stderr)

			var command string
			for line := range strings.SplitSeq(stderr, "\n") {
				if rest, ok := strings.CutPrefix(line, "error:   docker rm "); ok {
					command = "docker rm " + rest
				}
			}
			require.NotEmpty(t, command, "the report prints the command: %s", stderr)

			require.NoError(t, exec.CommandContext(t.Context(), "bash", "-n", "-c", command).Run(), "the command is valid shell: %s", command)
			words, err := exec.CommandContext(t.Context(), "bash", "-c", `docker() { printf '%s\n' "$@"; }; eval "$1"`, "_", command).Output()
			require.NoError(t, err)
			assert.Equal(t, "rm\n-f\n-v\n"+name+"\n", string(words), "the words of the command: %s", command)
		})
	}
}
