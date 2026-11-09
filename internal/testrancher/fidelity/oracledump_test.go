package fidelity

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dumpSecrets are the values the dump tests plant in the logs and the
// environment of the script; none of them may reach the output.
type dumpSecrets struct {
	bootstrap, token54, bearer, login, loginAfter, admin, adminAfter, environment string
}

func newDumpSecrets() dumpSecrets {
	alphabet := "bcdfghjklmnpqrstvwxz2456789"
	return dumpSecrets{
		bootstrap:   "Pw9xKz3LmQ7vTn2RbY8a",
		token54:     alphabet + alphabet,
		bearer:      "bearerValue123",
		login:       "token-qwert:" + strings.Repeat("wxz2456789", 5) + "wxzb",
		loginAfter:  strings.Repeat("wxz2456789", 5) + "wxzb",
		admin:       "token-asdfg:AdminSecretValue1234567890abcdefghijkl",
		adminAfter:  "AdminSecretValue1234567890abcdefghijkl",
		environment: "EnvBootstrapSentinel9f2c",
	}
}

func (d dumpSecrets) all() []string {
	return []string{d.bootstrap, d.token54, d.bearer, d.login, d.loginAfter, d.admin, d.adminAfter, d.environment}
}

// logText is what the docker stub prints for every log read: lines Rancher or
// a client can print, with the planted values in them.
func (d dumpSecrets) logText() string {
	return strings.Join([]string{
		"2026-11-07T04:46:30Z [INFO] Bootstrap Password: " + d.bootstrap,
		"Use https://172.30.1.2/dashboard/?setup=" + d.bootstrap + " to complete setup in the UI",
		"registered token-abcde:" + d.token54,
		"request header Authorization: Bearer " + d.bearer,
		"login returned " + d.login + " and admin " + d.admin,
		"plain log line kept",
	}, "\n") + "\n"
}

// dumpDocker builds the docker stub of the dump tests. ctl is a directory:
// the file state.<name> holds what docker inspect -f answers for a container
// (no file: the read fails), bare holds the answer of a docker inspect
// without a template, and logs holds what docker logs and a kubectl logs
// through docker exec print.
func dumpDocker(ctl string) string {
	return `#!/bin/sh
printf '%s\n' "$*" >> "$STUB_LOG"
CTL=` + ctl + `
case "$1" in
inspect)
  if [ "$2" = "-f" ]; then
    if [ -f "$CTL/state.$4" ]; then cat "$CTL/state.$4"; exit 0; fi
    exit 1
  fi
  if [ -f "$CTL/bare" ]; then cat "$CTL/bare"; exit 0; fi
  exit 1 ;;
logs)
  if [ -f "$CTL/logs" ]; then cat "$CTL/logs"; exit 0; fi
  exit 1 ;;
exec)
  case "$*" in
  *" logs "*) if [ -f "$CTL/logs" ]; then cat "$CTL/logs"; exit 0; fi ;;
  esac
  exit 1 ;;
esac
exit 97
`
}

const stateExited = "status=exited exitCode=137 oomKilled=true finishedAt=2026-11-07T04:46:33Z"

// TestCleanupReportsAStoppedOracle asserts the diagnostics dump of cleanup
// when the oracle container stopped: exit 5 once the run reached its own
// container, the container states first, the host's free memory, the last log
// lines of both containers with every planted secret redacted, and no change
// of an exit code in any other case. It also asserts that the dump goes on when
// every docker call fails under errexit, as the exit trap of the script does.
func TestCleanupReportsAStoppedOracle(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	secrets := newDumpSecrets()
	const running = "status=running exitCode=0 oomKilled=false finishedAt=0001-01-01T00:00:00Z"

	cases := []struct {
		name            string
		drift, reported int
		exit            int
		booted          bool
		oracle, k3s     string
		helm            bool
		memoryFails     bool
		restarts        int
		code            int
		notRunning      bool
		restarted       bool
		hinted          bool
		dump            bool
		oracleLine      string
	}{
		{name: "exit 1 and the oracle exited", exit: 1, booted: true, oracle: stateExited, k3s: running, code: 5, notRunning: true, dump: true, oracleLine: "[rancher] " + stateExited},
		{name: "exit 1 and the oracle running", exit: 1, booted: true, oracle: running, k3s: running, code: 1, dump: true, oracleLine: "[rancher] " + running},
		{name: "exit 1 and the oracle restarting", exit: 1, booted: true, oracle: "status=restarting exitCode=1 oomKilled=false finishedAt=x", k3s: running, code: 5, notRunning: true, dump: true},
		{name: "exit 1 and the oracle dead", exit: 1, booted: true, oracle: "status=dead exitCode=0 oomKilled=false finishedAt=x", k3s: running, code: 5, notRunning: true, dump: true},
		{name: "exit 4 and the oracle exited", exit: 4, booted: true, oracle: stateExited, k3s: running, code: 4, dump: true, oracleLine: "[rancher] " + stateExited},
		{name: "a reported drift 3 and the oracle exited", drift: 1, reported: 1, exit: 3, booted: true, oracle: stateExited, k3s: running, code: 5, notRunning: true, dump: true},
		{name: "a reported drift 3 and the oracle running", drift: 1, reported: 1, exit: 3, booted: true, oracle: running, k3s: running, code: 3},
		{name: "a command's own 5 and the oracle running", exit: 5, booted: true, oracle: running, k3s: running, code: 1, dump: true},
		{name: "a command's own 5 and the oracle exited", exit: 5, booted: true, oracle: stateExited, k3s: running, code: 5, notRunning: true, dump: true},
		{name: "no container", exit: 1, booted: true, code: 1, dump: true, oracleLine: "[rancher] state unknown"},
		{name: "a read with no status field", exit: 1, booted: true, oracle: "no such object", k3s: running, code: 1, dump: true, oracleLine: "[rancher] state unknown"},
		{name: "the run did not reach its container", exit: 1, oracle: stateExited, k3s: running, code: 1, dump: true, oracleLine: "[rancher] " + stateExited},
		{name: "the memory reading fails", exit: 1, booted: true, oracle: running, k3s: running, memoryFails: true, code: 1, dump: true},
		{name: "a helm boot", exit: 1, booted: true, oracle: stateExited, k3s: running, helm: true, code: 5, notRunning: true, dump: true},
		{name: "success", exit: 0, booted: true, oracle: stateExited, k3s: running, code: 0},
		{name: "exit 1 and the oracle running after 4 restarts", exit: 1, booted: true, oracle: running, k3s: running, restarts: 4, code: 5, restarted: true, hinted: true, dump: true, oracleLine: "[rancher] " + running},
		{name: "exit 1 and the oracle restarting after 4 restarts", exit: 1, booted: true, oracle: "status=restarting exitCode=1 oomKilled=false finishedAt=x", k3s: running, restarts: 4, code: 5, notRunning: true, hinted: true, dump: true},
		{name: "exit 1 and the oracle restarting after 3 restarts", exit: 1, booted: true, oracle: "status=restarting exitCode=1 oomKilled=false finishedAt=x", k3s: running, restarts: 3, code: 5, notRunning: true, dump: true},
		{name: "exit 1 and the oracle exited after 4 restarts", exit: 1, booted: true, oracle: stateExited, k3s: running, restarts: 4, code: 5, notRunning: true, dump: true},
		{name: "exit 1 and the oracle dead after 4 restarts", exit: 1, booted: true, oracle: "status=dead exitCode=0 oomKilled=false finishedAt=x", k3s: running, restarts: 4, code: 5, notRunning: true, dump: true},
		{name: "exit 1 and the oracle running after 3 restarts", exit: 1, booted: true, oracle: running, k3s: running, restarts: 3, code: 1, dump: true},
		{name: "exit 4 and the oracle running after 4 restarts", exit: 4, booted: true, oracle: running, k3s: running, restarts: 4, code: 4, dump: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newShellEnv(t)
			ctl := filepath.Join(e.dir, "ctl")
			require.NoError(t, os.MkdirAll(ctl, 0o700))
			testfs.WriteExecutable(t, filepath.Join(e.dir, "docker"), []byte(dumpDocker(ctl)))
			files := map[string]string{
				"logs": secrets.logText(),
				"bare": "[{\"Config\":{\"Env\":[\"CATTLE_BOOTSTRAP_PASSWORD=" + secrets.environment + "\"]}}]\n",
			}
			if c.oracle != "" {
				files["state.c"] = c.oracle + "\n"
			}
			if c.k3s != "" {
				files["state.c-k3s"] = c.k3s + "\n"
			}
			for name, body := range files {
				require.NoError(t, os.WriteFile(filepath.Join(ctl, name), []byte(body), 0o600))
			}
			if c.memoryFails {
				testfs.WriteExecutable(t, filepath.Join(e.dir, "memory_pressure"), []byte("#!/bin/sh\nexit 1\n"))
			}
			body := "BOOTSTRAP_PW=" + secrets.bootstrap + "\nLOGIN_TOKEN=" + secrets.login + "\nADMIN_TOKEN=" + secrets.admin + "\n"
			if c.booted {
				body += "ORACLE_BOOTED=1\n"
			}
			if c.helm {
				body += "BOOT=helm\n"
			}
			if c.restarts > 0 {
				body += fmt.Sprintf("ORACLE_RESTARTS=%d\n", c.restarts)
			}
			body += fmt.Sprintf("exit %d", c.exit)
			stdout, stderr, code, calls := e.run(e.cleanupProgram(c.drift, c.reported, body))

			assert.Equal(t, c.code, code, "stdout: %s\nstderr: %s", stdout, stderr)
			for _, secret := range secrets.all() {
				assert.NotContains(t, stdout, secret, "a planted value reached stdout")
				assert.NotContains(t, stderr, secret, "a planted value reached stderr")
			}
			assert.Equal(t, c.notRunning, strings.Contains(stderr, "error: the oracle container c is not running (status "), "stderr: %s", stderr)
			assert.Equal(t, c.dump, strings.Contains(stdout, "==> container state:\n[rancher] "), "stdout: %s", stdout)
			if !c.dump {
				return
			}
			if c.oracleLine != "" {
				assert.Contains(t, stdout, "==> container state:\n"+c.oracleLine+"\n[k3s] ")
			}
			if c.notRunning {
				assert.Contains(t, stderr, "; its state and last log lines follow")
			}
			assert.Equal(t, c.restarted, strings.Contains(stderr, "error: the oracle container c restarted 4 times during the boot (status running, exit code 0, OOMKilled false); its state and last log lines follow\n"), "stderr: %s", stderr)
			assert.Equal(t, c.hinted, strings.Contains(stderr, "error: a kept container restarts until it is removed; remove it, then run again:\nerror:   docker rm -f -v c\n"), "stderr: %s", stderr)
			if c.memoryFails {
				assert.Contains(t, stdout, "[host] memory free: unknown\n")
			} else {
				assert.Contains(t, stdout, "[host] memory free: 37% (memory_pressure)\n")
			}
			assert.Contains(t, calls, "logs --tail 200 c")
			assert.Contains(t, calls, "logs --tail 50 c-k3s")
			for _, prefix := range []string{"[rancher] ", "[k3s] "} {
				assert.Contains(t, stdout, prefix+"2026-11-07T04:46:30Z [INFO] Bootstrap Password: [redacted]\n")
				assert.Contains(t, stdout, prefix+"Use https://172.30.1.2/dashboard/?setup=[redacted] to complete setup in the UI\n")
				assert.Contains(t, stdout, prefix+"registered token-abcde:[redacted]\n")
				assert.Contains(t, stdout, prefix+"request header Authorization: [redacted]\n")
				assert.Contains(t, stdout, prefix+"plain log line kept\n")
			}
			assert.Contains(t, stdout, " and admin [redacted]\n")
			if c.helm {
				assert.Contains(t, stdout, "[helm] 2026-11-07T04:46:30Z [INFO] Bootstrap Password: [redacted]\n")
				assert.Contains(t, stdout, "==> Rancher pod log:\n[rancher] ")
			}
			for _, call := range calls {
				assert.NotEqual(t, "inspect c", call, "a bare docker inspect")
				assert.NotContains(t, call, "{{json", "the whole inspect")
			}
		})
	}

	t.Run("the dump goes on when every docker call fails, under errexit", func(t *testing.T) {
		t.Parallel()
		e := newShellEnv(t)
		ctl := filepath.Join(e.dir, "ctl")
		require.NoError(t, os.MkdirAll(ctl, 0o700))
		testfs.WriteExecutable(t, filepath.Join(e.dir, "docker"), []byte(dumpDocker(ctl)))
		program := e.cleanupDefinitions(0, 0) + "trap cleanup EXIT\nORACLE_BOOTED=1\nexit 1\n"
		stdout, stderr, code, _ := e.run(program)
		assert.Equal(t, 1, code, "stderr: %s", stderr)
		assert.Contains(t, stdout, "[rancher] state unknown\n[k3s] state unknown\n")
		assert.Contains(t, stdout, "[host] memory free: 37% (memory_pressure)\n")
		assert.Contains(t, stdout, "==> downstream agent log lines:\n", "the dump reached its last step")
		assert.NotContains(t, stderr, "is not running")
	})
}

// TestHostMemoryFree asserts the one-line memory reading of the dump: from
// MemAvailable and MemTotal when the meminfo file holds both, else from
// memory_pressure, else unknown; that it prints unknown for a reading it
// cannot trust, stops a memory_pressure that does not answer within its limit,
// never fails and leaves nothing under TMPDIR.
func TestHostMemoryFree(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	const absent = "memory-pressure-is-not-installed"
	pressure := func(body string) string { return "#!/bin/sh\n" + body + "\n" }
	cases := []struct {
		name     string
		meminfo  string
		hasInfo  bool
		stub     string
		command  string
		limit    int
		want     string
		maxTakes time.Duration
	}{
		{name: "meminfo", meminfo: "MemTotal:        1000 kB\nMemFree: 1 kB\nMemAvailable:    250 kB\n", hasInfo: true, command: absent, limit: 5,
			want: "[host] memory free: 25% (MemAvailable of MemTotal)"},
		{name: "meminfo with a total of 0", meminfo: "MemTotal: 0 kB\nMemAvailable: 0 kB\n", hasInfo: true, command: absent, limit: 5, want: "[host] memory free: unknown"},
		{name: "meminfo with a total that is no number", meminfo: "MemTotal: lots kB\nMemAvailable: 5 kB\n", hasInfo: true, command: absent, limit: 5, want: "[host] memory free: unknown"},
		{name: "meminfo with an available above the total", meminfo: "MemTotal: 10 kB\nMemAvailable: 50 kB\n", hasInfo: true, command: absent, limit: 5, want: "[host] memory free: unknown"},
		{name: "memory_pressure when meminfo is missing", stub: pressure(`echo "System-wide memory free percentage: 41%"`), command: "memory_pressure", limit: 5,
			want: "[host] memory free: 41% (memory_pressure)"},
		{name: "memory_pressure when meminfo lacks MemAvailable", meminfo: "MemTotal: 1000 kB\n", hasInfo: true, stub: pressure(`echo "System-wide memory free percentage: 41%"`), command: "memory_pressure", limit: 5,
			want: "[host] memory free: 41% (memory_pressure)"},
		{name: "memory_pressure with 0 percent", stub: pressure(`echo "System-wide memory free percentage: 0%"`), command: "memory_pressure", limit: 5,
			want: "[host] memory free: 0% (memory_pressure)"},
		{name: "memory_pressure that fails", stub: pressure("exit 1"), command: "memory_pressure", limit: 5, want: "[host] memory free: unknown"},
		{name: "memory_pressure that prints no line", stub: pressure("echo nothing useful"), command: "memory_pressure", limit: 5, want: "[host] memory free: unknown"},
		{name: "memory_pressure that prints a value above 100", stub: pressure(`echo "System-wide memory free percentage: 142%"`), command: "memory_pressure", limit: 5,
			want: "[host] memory free: unknown"},
		{name: "memory_pressure that prints a value that is no number", stub: pressure(`echo "System-wide memory free percentage: ?%"`), command: "memory_pressure", limit: 5,
			want: "[host] memory free: unknown"},
		{name: "memory_pressure that does not answer", stub: pressure("sleep 30"), command: "memory_pressure", limit: 1, want: "[host] memory free: unknown", maxTakes: 10 * time.Second},
		{name: "neither source", command: absent, limit: 5, want: "[host] memory free: unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.maxTakes == 0 {
				t.Parallel()
			}
			e := newShellEnv(t)
			tmp := filepath.Join(e.dir, "tmp")
			require.NoError(t, os.MkdirAll(tmp, 0o700))
			info := filepath.Join(e.dir, "meminfo")
			if c.hasInfo {
				require.NoError(t, os.WriteFile(info, []byte(c.meminfo), 0o600))
			}
			if c.stub != "" {
				testfs.WriteExecutable(t, filepath.Join(e.dir, "memory_pressure"), []byte(c.stub))
			}
			program := "set -euo pipefail\nexport TMPDIR=" + tmp + "\n" +
				"MEMINFO_FILE=" + info + " MEMORY_PRESSURE_CMD=" + c.command + fmt.Sprintf(" MEMORY_READ_LIMIT_SECONDS=%d\n", c.limit) +
				shellFunction(t, "host_memory_free") + "host_memory_free\n"
			start := time.Now()
			stdout, stderr, code, _ := e.run(program)
			assert.Equal(t, 0, code, "stderr: %s", stderr)
			assert.Equal(t, c.want+"\n", stdout)
			if c.maxTakes > 0 {
				assert.Less(t, time.Since(start), c.maxTakes, "the reading is stopped at its limit")
			}
			left, err := os.ReadDir(tmp)
			require.NoError(t, err)
			assert.Empty(t, left, "the reading leaves nothing under TMPDIR")
		})
	}
}

// TestRedactLog asserts what redact_log replaces and keeps: each rule redacts
// its value and keeps the line and its prefix, secrets the script holds are
// redacted wherever they occur, text without a secret passes unchanged and
// fast, and a filter that fails prints one line and no log line.
func TestRedactLog(t *testing.T) {
	testfs.SkipOnWindows(t, "scripts/e2e-fidelity is a bash script")
	alphabet := "bcdfghjklmnpqrstvwxz2456789"
	token54 := alphabet + alphabet
	digest := "sha256:" + strings.Repeat("a1b2c3d4", 8)
	const prefix = "[x] "
	cases := []struct {
		name   string
		input  string
		secret string
		want   string
	}{
		{name: "authorization header", input: "2026 Authorization: Bearer x", want: "2026 Authorization: [redacted]"},
		{name: "bearer in any case", input: "sent BEARER abc123 to the server", want: "sent BEARER [redacted]"},
		{name: "bootstrap password", input: "INFO Bootstrap Password: abcDEF123", want: "INFO Bootstrap Password: [redacted]"},
		{name: "setup value", input: "Use https://h/dashboard/?setup=abcDEF123&x=1 to complete", want: "Use https://h/dashboard/?setup=[redacted]&x=1 to complete"},
		{name: "password assignment", input: "PASSWORD=hunter2 next", want: "PASSWORD=[redacted] next"},
		{name: "quoted secret", input: `secret = "a b" tail`, want: "secret = [redacted] tail"},
		{name: "token with a colon", input: "token: abc.def", want: "token: [redacted]"},
		{name: "json password", input: `{"password":"p@ss w0rd","user":"x"}`, want: `{"password":[redacted],"user":"x"}`},
		{name: "a bearer with its name", input: "token-abcde:" + token54, want: "token-abcde:[redacted]"},
		{name: "a run of 54 next to an underscore", input: "x_" + token54 + "_y", want: "x_[redacted]_y"},
		{name: "a run of 54 behind a letter", input: "a" + token54, want: "a" + token54},
		{name: "a run of 55", input: token54 + "b", want: token54 + "b"},
		{name: "the bootstrap password anywhere", input: "login with Zx9Qw3PlmN7rT2vKb8Ya failed", secret: "BOOTSTRAP_PW=Zx9Qw3PlmN7rT2vKb8Ya", want: "login with [redacted] failed"},
		{name: "the text of a token after its colon", input: "used SecretPartOfTheToken123 here", secret: "LOGIN_TOKEN=token-abcde:SecretPartOfTheToken123", want: "used [redacted] here"},
		{name: "the admin token", input: "used token-zzzzz:OtherSecretValue here", secret: "ADMIN_TOKEN=token-zzzzz:OtherSecretValue", want: "used [redacted] here"},
		{name: "a plain line", input: "plain log line with level=info msg=\"token expired\" and tokens are fine", want: "plain log line with level=info msg=\"token expired\" and tokens are fine"},
		{name: "a sha256 digest", input: "image " + digest + " pulled", want: "image " + digest + " pulled"},
		{name: "a byte that is not UTF-8", input: "bad \xff byte", want: "bad \ufffd byte"},
		{name: "the agent's first line", input: `time="2026-11-07T00:00:00Z" level=info msg="Connecting to wss://127.0.0.1:8443/v3/connect with token starting with abcd1234efgh"`,
			want: `time="2026-11-07T00:00:00Z" level=info msg="Connecting to wss://127.0.0.1:8443/v3/connect with token starting with [redacted]"`},
		{name: "the agent's first line as text keeps its closing quote", input: `msg="Connecting to wss://x/v3/connect with token starting with abcd1234efgh"`,
			want: `msg="Connecting to wss://x/v3/connect with token starting with [redacted]"`},
		{name: "the agent's first line as JSON keeps the rest of the object", input: `{"msg":"Connecting to wss://x/v3/connect with token starting with abc123","level":"info","time":"t"}`,
			want: `{"msg":"Connecting to wss://x/v3/connect with token starting with [redacted]","level":"info","time":"t"}`},
		{name: "two lines", input: "first\nsecond Bearer s\n", want: "first\n" + prefix + "second Bearer [redacted]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newShellEnv(t)
			input := filepath.Join(e.dir, "input")
			require.NoError(t, os.WriteFile(input, []byte(c.input), 0o600))
			program := "set -euo pipefail\n" + shellFunction(t, "redact_log") + c.secret + "\nredact_log '" + prefix + "' < " + input + "\n"
			stdout, stderr, code, _ := e.run(program)
			assert.Equal(t, 0, code, "stderr: %s", stderr)
			assert.Equal(t, prefix+c.want+"\n", stdout)
		})
	}

	t.Run("no secret variable is set, under set -u", func(t *testing.T) {
		e := newShellEnv(t)
		input := filepath.Join(e.dir, "input")
		require.NoError(t, os.WriteFile(input, []byte("a line\n"), 0o600))
		stdout, stderr, code, _ := e.run("set -euo pipefail\n" + shellFunction(t, "redact_log") + "redact_log '" + prefix + "' < " + input + "\n")
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Equal(t, prefix+"a line\n", stdout)
	})

	t.Run("an empty log prints nothing", func(t *testing.T) {
		e := newShellEnv(t)
		stdout, stderr, code, _ := e.run("set -euo pipefail\n" + shellFunction(t, "redact_log") + "redact_log '" + prefix + "' < /dev/null\n")
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Empty(t, stdout)
	})

	t.Run("a filter that fails prints one line and never a log line", func(t *testing.T) {
		e := newShellEnv(t)
		testfs.WriteExecutable(t, filepath.Join(e.dir, "python3"), []byte("#!/bin/sh\ncat\nexit 1\n"))
		input := filepath.Join(e.dir, "input")
		require.NoError(t, os.WriteFile(input, []byte("Bootstrap Password: secretvalue\nsecond\n"), 0o600))
		stdout, stderr, code, _ := e.run("set -euo pipefail\n" + shellFunction(t, "redact_log") + "redact_log '" + prefix + "' < " + input + "\n")
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Equal(t, prefix+"[log lines left out: the redaction filter failed]\n", stdout)
	})

	t.Run("a log over 64 KB is filtered when no temporary file can be written", func(t *testing.T) {
		e := newShellEnv(t)
		const count = 3000
		input := filepath.Join(e.dir, "input")
		require.NoError(t, os.WriteFile(input, []byte(strings.Repeat("INFO Bootstrap Password: abcDEF123\n", count)), 0o600))
		program := "set -euo pipefail\n" + shellFunction(t, "redact_log") + "trap '' XFSZ\nulimit -f 1\nredact_log '" + prefix + "' < " + input + "\n"
		stdout, stderr, code, _ := e.run(program)
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.NotContains(t, stderr, "temp file")
		assert.Equal(t, strings.Repeat(prefix+"INFO Bootstrap Password: [redacted]\n", count), stdout)
	})

	t.Run("a line of 100000 characters passes unchanged and fast", func(t *testing.T) {
		e := newShellEnv(t)
		line := strings.Repeat("a", 100000)
		input := filepath.Join(e.dir, "input")
		require.NoError(t, os.WriteFile(input, []byte(line+"\n"), 0o600))
		start := time.Now()
		stdout, stderr, code, _ := e.run("set -euo pipefail\n" + shellFunction(t, "redact_log") + "redact_log '" + prefix + "' < " + input + "\n")
		assert.Equal(t, 0, code, "stderr: %s", stderr)
		assert.Equal(t, prefix+line+"\n", stdout)
		assert.Less(t, time.Since(start), 2*time.Second)
	})
}

// joinContinuations returns the lines with each backslash continuation joined
// to the line that follows it, in order, and a trailing \r removed. A line
// whose trimmed text starts with # is a comment, which bash ends at the
// newline: it is never joined with the next line. A last line that ends in a
// backslash is kept, with the backslash replaced by a space as in a join.
func joinContinuations(lines []string) []string {
	var out []string
	current := ""
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if current == "" && strings.HasPrefix(strings.TrimSpace(line), "#") {
			out = append(out, line)
			continue
		}
		if body, ok := strings.CutSuffix(line, `\`); ok {
			current += body + " "
			continue
		}
		out = append(out, current+line)
		current = ""
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

// joinedScriptLines is the script's lines with each backslash continuation
// joined to the line that follows it.
func joinedScriptLines(t *testing.T) []string {
	t.Helper()
	return joinContinuations(scriptLines(t))
}

var (
	inspectCommand  = regexp.MustCompile(`\bdocker\s+(?:[^|;&'"$()<>]*?\s)?(?:(?:container|image|network)\s+)?inspect\b`)
	inspectFlag     = regexp.MustCompile(`(?:^|\s)(?:-f|--format)(?:\s|=)`)
	inspectTemplate = regexp.MustCompile(`(?:^|\s)(?:-f|--format)(?:\s+|=)('[^']*'|"[^"]*")`)
	inspectDiscard  = regexp.MustCompile(`(?:^|[^0-9>&])>\s*/dev/null|\b1>\s*/dev/null|&>\s*/dev/null`)
	templateField   = regexp.MustCompile(`\.[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)
	templateWhole   = regexp.MustCompile(`\bjson\b|\bprintf\b|\{\{-?\s*[.$]\s*-?\}\}|(?:^|[\s(])[.$](?:[\s")}]|$)|\$(?:[^A-Za-z]|$)`)

	// allowedInspectFields are the only fields a docker inspect template of the
	// script may name. A field outside the list needs a reviewed row: the
	// container's Config holds the bootstrap password.
	allowedInspectFields = []string{
		".Name", ".Id", ".HostConfig.PortBindings", ".NetworkSettings.Ports", ".IPAM.Config", ".Subnet", ".HostPort", ".HostIp",
		".State.Status", ".State.ExitCode", ".State.OOMKilled", ".State.FinishedAt", ".RestartCount",
	}
)

// inspectViolations returns the commands of a script that run docker inspect
// without a template or an output sent to /dev/null, or whose template names a
// field that is not allowed, and the number of commands it checked. A command
// is the text from one docker inspect to the next one on the same line or the
// end of the line. Comment lines are skipped.
func inspectViolations(lines []string) (violations []string, checked int) {
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") {
			continue
		}
		starts := inspectCommand.FindAllStringIndex(text, -1)
		for i, start := range starts {
			end := len(text)
			if i+1 < len(starts) {
				end = starts[i+1][0]
			}
			command := text[start[0]:end]
			checked++
			if !inspectFlag.MatchString(command) {
				if !inspectDiscard.MatchString(command) {
					violations = append(violations, "no template and the output is not discarded: "+command)
				}
				continue
			}
			templates := inspectTemplate.FindAllStringSubmatch(command, -1)
			if len(templates) == 0 {
				violations = append(violations, "a template that is not quoted: "+command)
			}
			for _, m := range templates {
				if templateWhole.MatchString(m[1]) {
					violations = append(violations, "a template that reads a whole object: "+command)
				}
				for _, field := range templateField.FindAllString(m[1], -1) {
					if !slices.Contains(allowedInspectFields, field) {
						violations = append(violations, "the field "+field+" is not allowed: "+command)
					}
				}
			}
		}
	}
	return violations, checked
}

// TestScriptReadsOnlyNamedFieldsFromDockerInspect asserts that every docker
// inspect of the script either discards its output or names its fields in a
// template from a short list: the whole object holds the container's
// environment, with the bootstrap password in it. It also asserts that every
// line of the script (continuation lines joined) that holds the word inspect
// equals one entry of inspectLines, and that every entry equals a line, so a
// new line that holds the word inspect needs a reviewed entry, and that the
// field check runs on every entry and finds no field outside the list. A
// command word built from quotes, escapes, variables or command substitution,
// and a line of a multi-line string that starts with #, are not seen. The scans
// themselves are asserted against lines they must flag and lines they must
// pass.
func TestScriptReadsOnlyNamedFieldsFromDockerInspect(t *testing.T) {
	violations, checked := inspectViolations(joinedScriptLines(t))
	assert.Empty(t, violations)
	assert.GreaterOrEqual(t, checked, 12, "the scan found the inspect lines")

	unlisted, stale := exactLineViolations(joinedScriptLines(t), inspectWord, listedInspectLines())
	assert.Empty(t, unlisted, "a line that holds the word inspect and is not in inspectLines")
	assert.Empty(t, stale, "an entry of inspectLines that no line of the script equals")
	for _, entry := range inspectLines {
		entryViolations, entryChecked := inspectViolations([]string{entry})
		assert.Empty(t, entryViolations, "every entry of inspectLines discards its output or names only listed fields")
		assert.Equalf(t, len(inspectWord.FindAllString(entry, -1)), entryChecked,
			"the field check parses every inspect of this entry: %s", entry)
	}
	twoInspects := `x="$(docker inspect -f '{{.Id}}' "${CONTAINER}"; docker --context "${CTX}" inspect "${CONTAINER}")"`
	_, twoChecked := inspectViolations([]string{twoInspects})
	assert.NotEqual(t, len(inspectWord.FindAllString(twoInspects, -1)), twoChecked,
		"an entry with an inspect the field check does not parse fails the entry check")
	unparsed := `out="$(docker --context "${CTX}" inspect -f '{{json .Config}}' "${CONTAINER}")"`
	_, unparsedChecked := inspectViolations([]string{unparsed})
	assert.Zero(t, unparsedChecked, "the field check does not parse a global flag with a quoted value: a line of that form fails the entry check")
	for name, physical := range map[string][]string{
		"an inspect after a comment that ends in a backslash": {`# a comment about inspect \`, `docker inspect "${CONTAINER}"`},
	} {
		got, _ := exactLineViolations(joinContinuations(physical), inspectWord, listedInspectLines())
		assert.NotEmptyf(t, got, "the exact-line rule passes %s", name)
	}
	for _, line := range flaggedInspectLines {
		got, _ := exactLineViolations([]string{line}, inspectWord, listedInspectLines())
		assert.NotEmptyf(t, got, "the exact-line rule passes %s", line)
	}

	flagged := []string{
		`out="$(docker inspect "${CONTAINER}")"`,
		`docker  inspect   c`,
		`docker inspect -f '{{json .}}' c`,
		`docker inspect -f '{{.}}' c`,
		`docker inspect --format '{{.Config.Env}}' c`,
		`docker inspect -f '{{.State.Error}}' c`,
		`docker inspect -f '{{printf "%v" .State}}' c`,
		`docker container inspect -f '{{.Config.Env}}' c`,
		`docker network inspect -f '{{$}}' n`,
		`docker inspect -f '{{index . "Config"}}' c`,
		`docker --context x inspect c`,
		`docker inspect -f {{.Name}} c`,
		`docker inspect -f '{{.State.Status}}' c ; docker inspect c`,
		`out="$(docker inspect "${CONTAINER}" 2>/dev/null)"`,
	}
	for _, line := range flagged {
		got, _ := inspectViolations([]string{line})
		assert.NotEmptyf(t, got, "the scan passes %s", line)
	}
	passed := []string{
		`docker inspect c >/dev/null 2>&1`,
		`docker network inspect "${NETWORK}" >/dev/null 2>&1 && return 0`,
		`docker network inspect "${old}-net" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null`,
		`docker inspect -f 'status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} finishedAt={{.State.FinishedAt}}' c`,
		`docker inspect --format '{{(index (index .NetworkSettings.Ports "443/tcp") 0).HostIp}}' c`,
		`# docker inspect c`,
	}
	for _, line := range passed {
		got, _ := inspectViolations([]string{line})
		assert.Emptyf(t, got, "the scan flags %s", line)
	}
}

var (
	logRead      = regexp.MustCompile(`\bdocker\s+(?:[^|;&'"$()<>]*?\s)?(?:container\s+)?logs\b|\bkubectl\b[^|]*\blogs\b|\bcrictl\b[^|]*\blogs\b|\blastState\b`)
	firstPipe    = regexp.MustCompile(`(?:^|[^|])\|(?:[^|]|$)`)
	redactedPipe = regexp.MustCompile(`^\s*redact_log\b`)
)

// redactsEveryRead reports whether, for each log read on the line, the first
// pipe after the read goes into redact_log: a read piped into another command
// first, or not piped at all, is not redacted.
func redactsEveryRead(text string) bool {
	for _, loc := range logRead.FindAllStringIndex(text, -1) {
		rest := text[loc[0]:]
		pipe := firstPipe.FindStringIndex(rest)
		if pipe == nil {
			return false
		}
		after := rest[pipe[0]:]
		after = after[strings.Index(after, "|")+1:]
		if !redactedPipe.MatchString(after) {
			return false
		}
	}
	return true
}

// unredactedLogReads returns the lines that read a container or pod log and do
// not pipe it into redact_log, except the lines whose start is a key of
// printed (text that only names such a read); it also returns the keys of
// printed that matched no line.
func unredactedLogReads(lines []string, printed map[string]string) (violations, unused []string) {
	used := map[string]bool{}
	for _, line := range lines {
		text := strings.TrimSpace(line)
		if strings.HasPrefix(text, "#") || !logRead.MatchString(text) || redactsEveryRead(text) {
			continue
		}
		matched := false
		for key := range printed {
			if strings.HasPrefix(text, key) {
				used[key] = true
				matched = true
			}
		}
		if !matched {
			violations = append(violations, text)
		}
	}
	for key := range printed {
		if !used[key] {
			unused = append(unused, key)
		}
	}
	return violations, unused
}

// logReadLines are the lines of scripts/e2e-fidelity that hold the word logs
// and read a container or pod log, each piping the read straight into
// redact_log with its prefix and nothing between the read and the pipe.
var logReadLines = []string{
	`docker logs --tail 200 "${CONTAINER}" 2>&1 | redact_log "[rancher] " || true`,
	`docker logs --tail 50 "${K3S_CONTAINER}" 2>&1 | redact_log "[k3s] " || true`,
	`docker exec "${K3S_CONTAINER}" kubectl -n cattle-system logs deploy/cattle-cluster-agent --tail 50 2>&1 | redact_log "[agent] " || true`,
	`docker exec "${CONTAINER}" kubectl -n kube-system logs job/helm-install-rancher --tail 200 2>&1 | redact_log "[helm] " || true`,
	`docker exec "${CONTAINER}" kubectl -n cattle-system logs deploy/rancher --tail 200 2>&1 | redact_log "[rancher] " || true`,
	`docker logs --tail 20 "${CONTAINER}" 2>&1 | redact_log "" >&2 || true`,
	`docker logs --tail 1000 "${CONTAINER}" 2>&1 | redact_log "[rancher] " || true`,
}

// logWordLines are the lines that hold the word log or logs without reading a
// log, each with the reason it is allowed.
var logWordLines = map[string]string{
	`printf '%s[log lines left out: the redaction filter failed]\n' "${prefix}"`:                                                                                                  "the message printed in place of a log when the filter fails",
	`echo "error: the oracle container ${CONTAINER} is not running (status ${oracle_status}, exit code ${exit_code}, OOMKilled ${oom}); its state and last log lines follow" >&2`: "the error line for a stopped oracle container",
	`echo "==> last 200 container log lines:"`:           "a heading of the diagnostics dump",
	`echo "==> last 50 downstream container log lines:"`: "a heading of the diagnostics dump",
	`echo "==> downstream agent log lines:"`:             "a heading of the diagnostics dump",
	`echo "==> Rancher chart install job log:"`:          "a heading of the diagnostics dump",
	`echo "==> Rancher pod log:"`:                        "a heading of the diagnostics dump",
	`echo "error: the oracle container ${CONTAINER} restarted ${ORACLE_RESTARTS} times during the boot (status ${oracle_status}, exit code ${exit_code}, OOMKilled ${oom}); its state and last log lines follow" >&2`:                                                                                                                           "the error line for an oracle container that restarted more than 3 times",
	`echo "error: the node container stopped (exit code $(docker inspect --format '{{.State.ExitCode}}' "${CONTAINER}" 2>/dev/null)); its last log lines:" >&2`:                                                                                                                                                                                 "the error line for a stopped node container; its only docker inspect reads one State field",
	`echo "error: with E2E_FIDELITY_KEEP=1 a kept pair whose container, container id, bind address, /ping, admin token, state file, k3s container, k3s container id or downstream cluster check fails is never removed. Find out why (docker ps -a --filter name=${named_container}; docker logs --tail 50 ${named_container}) and run again."`: "a message that names the command which shows the log",
	`echo "error: the last read of the downstream cluster got no HTTP answer; also find out why the oracle did not answer (docker ps -a --filter name=${CONTAINER}; docker logs --tail 50 ${CONTAINER})"`:                                                                                                                                       "a message that names the command which shows the log",
}

// inspectLines are the lines of scripts/e2e-fidelity that hold the word
// inspect: every docker inspect the script runs, each with a template of named
// fields or its output discarded.
var inspectLines = []string{
	`container_ports="$(docker ps -aq | xargs -r docker inspect -f '{{.Name}} {{range $p, $b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}} {{end}}{{end}}')" ||`,
	`networks="$(docker network ls -q | xargs -r docker network inspect -f '{{.Name}} {{range .IPAM.Config}}{{.Subnet}} {{end}}')" ||`,
	`running="$(docker inspect --format '{{.Id}}' "${CONTAINER}" 2>/dev/null)" || return 1`,
	`docker image inspect "$1" >/dev/null 2>&1 ||`,
	`docker image inspect "${image_ref}" >/dev/null 2>&1 ||`,
	`oracle_state="$(docker inspect -f 'status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} finishedAt={{.State.FinishedAt}}' "${CONTAINER}" 2>/dev/null)" || oracle_state=""`,
	`k3s_state="$(docker inspect -f 'status={{.State.Status}} exitCode={{.State.ExitCode}} oomKilled={{.State.OOMKilled}} finishedAt={{.State.FinishedAt}}' "${K3S_CONTAINER}" 2>/dev/null)" || k3s_state=""`,
	`docker network inspect "${NETWORK}" >/dev/null 2>&1 && return 0`,
	`docker container inspect "${c}" >/dev/null 2>&1 && found="${found} container ${c}"`,
	`if [ "$(docker network inspect "${old}-net" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null)" = "${SUBNET}" ]; then`,
	`running="$(docker inspect --format '{{.Id}}' "${CONTAINER}" 2>/dev/null)" || {`,
	`bound_ip="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "443/tcp") 0).HostIp}}' "${CONTAINER}" 2>/dev/null)" || {`,
	`k3s_running="$(docker inspect --format '{{.Id}}' "${K3S_CONTAINER}" 2>/dev/null)" || {`,
	`cid="$(docker inspect --format '{{.Id}}' "${CONTAINER}")"`,
	`k3s_cid="$(docker inspect --format '{{.Id}}' "${K3S_CONTAINER}")"`,
	`if [ "$(docker inspect --format '{{.State.Status}}' "${CONTAINER}" 2>/dev/null)" != "running" ]; then`,
	`echo "error: the node container stopped (exit code $(docker inspect --format '{{.State.ExitCode}}' "${CONTAINER}" 2>/dev/null)); its last log lines:" >&2`,
	`state="$(docker inspect --format '{{.State.Status}} {{.RestartCount}}' "${CONTAINER}" 2>/dev/null)" || return 0`,
}

var (
	logsWord    = regexp.MustCompile(`\blogs?\b`)
	inspectWord = regexp.MustCompile(`\binspect\b`)

	// logReadShape is the form of every entry of logReadLines: a docker logs,
	// or a kubectl logs run through docker exec, with the output of both
	// streams piped straight into redact_log and nothing between the read and
	// that pipe.
	logReadShape = regexp.MustCompile(`^(?:docker logs [^|;&<>()#]*|docker exec "\$\{[A-Z0-9_]+\}" kubectl [^|;&<>()#]*) 2>&1 \| redact_log "[^"]*"(?: >&2)? \|\| true$`)
)

// exactLineViolations returns the non-comment lines that hold the word and
// equal no entry of listed (compared after the leading white space is removed;
// the lines are the joined lines of the script), and the entries that no line
// equals.
func exactLineViolations(lines []string, word *regexp.Regexp, listed map[string]string) (unlisted, stale []string) {
	used := map[string]bool{}
	for _, line := range lines {
		text := strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
		if strings.HasPrefix(text, "#") || !word.MatchString(text) {
			continue
		}
		if _, ok := listed[text]; ok {
			used[text] = true
			continue
		}
		unlisted = append(unlisted, text)
	}
	for entry := range listed {
		if !used[entry] {
			stale = append(stale, entry)
		}
	}
	return unlisted, stale
}

// listedLogLines is logReadLines and logWordLines as one table; a read has an
// empty reason.
func listedLogLines() map[string]string {
	listed := map[string]string{}
	for _, line := range logReadLines {
		listed[line] = ""
	}
	maps.Copy(listed, logWordLines)
	return listed
}

// listedInspectLines is inspectLines as a table with empty reasons.
func listedInspectLines() map[string]string {
	listed := map[string]string{}
	for _, line := range inspectLines {
		listed[line] = ""
	}
	return listed
}

// flaggedLogLines are lines that hold the word log or logs and must fail the
// exact-line rule:
// a read followed by another command, a comment or a redirection before its
// pipe, a read with a global flag whose value is quoted or a variable, and a
// read that is not piped into redact_log.
var flaggedLogLines = []string{
	`docker logs c 2>&1 || echo failed | redact_log ""`,
	`docker logs c 2>&1; echo x | redact_log ""`,
	`docker logs c 2>&1 && echo x | redact_log ""`,
	`docker logs c 2>&1 # | redact_log ""`,
	`docker logs c 2>&1; echo "| redact_log"`,
	`docker logs c > raw.log 2>&1; cat raw.log | redact_log ""`,
	`docker logs c 2>&1 > raw.log | redact_log ""`,
	`docker logs c 2> raw.log | redact_log ""`,
	`tee raw.log < <(docker logs c 2>&1) | redact_log ""`,
	`docker --context "${CTX}" logs c 2>&1 | sed x`,
	`docker -H "${DOCKER_HOST}" logs c 2>&1`,
	`docker --context=$CTX logs c 2>&1`,
	`docker logs --tail 50 c 2>&1 | sed 's/^/[k3s] /'`,
	`docker logs c 2>&1 | tee raw.log | redact_log "[k3s] " || true`,
	`docker exec "${K3S_CONTAINER}" sh -c 'tail -n 50 /var/log/pods/*/*/*.log'`,
	`kubectl get --raw /api/v1/namespaces/cattle-system/pods/p/log`,
}

// flaggedInspectLines are lines that name inspect and must fail the
// exact-line rule.
var flaggedInspectLines = []string{
	`docker --context "${CTX}" inspect "${CONTAINER}"`,
	`docker inspect "${CONTAINER}"`,
	`docker inspect -f '{{.Config.Env}}' "${CONTAINER}"`,
	`out="$(docker inspect -f '{{.State.Status}}' c; docker inspect c)"`,
}

// TestEveryLogReadInTheScriptIsRedacted asserts that every read of a container
// or pod log in the script pipes into redact_log, that redact_log and the
// memory reading are defined before the exit trap is set, and that the scan
// flags a read without the filter. It also asserts that every line of the
// script (continuation lines joined) that holds the word log or logs equals
// one entry of logReadLines, which pipes the read straight into redact_log with
// nothing between them, or of logWordLines, which names a log without reading
// one and gives its reason; and that every entry equals a line, so a new line
// that holds the word needs a reviewed entry. A command word built from
// quotes, escapes, variables or command substitution, and a line of a
// multi-line string that starts with #, are not seen.
func TestEveryLogReadInTheScriptIsRedacted(t *testing.T) {
	lines := joinedScriptLines(t)
	// printed lists the lines that name a log read in text only.
	printed := map[string]string{
		`echo "error: with E2E_FIDELITY_KEEP=1 a kept pair whose container`: "the refusal prints the command that shows the log",
		`echo "error: the last read of the downstream cluster`:              "the refusal prints the command that shows the log of the oracle container",
	}
	violations, unused := unredactedLogReads(lines, printed)
	assert.Empty(t, violations, "a log read that is not piped into redact_log")
	assert.Empty(t, unused, "a printed line with no match in the script; remove it from the list")

	unlisted, stale := exactLineViolations(lines, logsWord, listedLogLines())
	assert.Empty(t, unlisted, "a line that holds the word log or logs and is not in logReadLines or logWordLines")
	assert.Empty(t, stale, "an entry of logReadLines or logWordLines that no line of the script equals")
	for _, line := range logReadLines {
		assert.Regexp(t, logReadShape, line, "a log read pipes straight into redact_log")
	}
	for line, reason := range logWordLines {
		assert.NotEmpty(t, reason, "a line that holds the word log or logs without reading a log needs its reason: %s", line)
	}
	for _, line := range flaggedLogLines {
		got, _ := exactLineViolations([]string{line}, logsWord, listedLogLines())
		assert.NotEmptyf(t, got, "the exact-line rule passes %s", line)
	}
	for name, physical := range map[string][]string{
		"a read with a redirection on its continuation line": {`docker logs c 2>&1 \`, `  > raw.log | redact_log ""`},
		"the same with CRLF line endings":                    {"docker logs c 2>&1 \\\r", "  > raw.log | redact_log \"\"\r"},
		"a read after a comment that ends in a backslash":    {`# keep the tail short \`, `docker logs --tail 50 "${K3S_CONTAINER}" 2>&1`},
		"a last line that ends in a backslash":               {`docker logs c 2>&1 \`},
	} {
		got, _ := exactLineViolations(joinContinuations(physical), logsWord, listedLogLines())
		assert.NotEmptyf(t, got, "the exact-line rule passes %s", name)
	}
	assert.Equal(t, []string{"# keep the tail short \\", "docker logs c"}, joinContinuations([]string{"# keep the tail short \\", "docker logs c"}),
		"a comment line is never joined with the next line")
	assert.Equal(t, []string{"docker logs c 2>&1    > raw.log"}, joinContinuations([]string{"docker logs c 2>&1 \\", "  > raw.log"}),
		"a continuation is joined")
	assert.Equal(t, []string{"docker logs c 2>&1    > raw.log"}, joinContinuations([]string{"docker logs c 2>&1 \\\r", "  > raw.log\r"}),
		"a continuation with CRLF line endings is joined")
	assert.Equal(t, []string{"docker logs c 2>&1  "}, joinContinuations([]string{"docker logs c 2>&1 \\"}),
		"a last line that ends in a backslash is kept")

	index := func(prefix string) int {
		for i, line := range lines {
			if strings.HasPrefix(line, prefix) {
				return i
			}
		}
		return -1
	}
	trap := index("trap cleanup EXIT")
	require.GreaterOrEqual(t, trap, 0)
	for _, definition := range []string{"redact_log() {", "host_memory_free() {", "MEMINFO_FILE=", "MEMORY_PRESSURE_CMD=", "MEMORY_READ_LIMIT_SECONDS=", "cleanup() {"} {
		at := index(definition)
		require.GreaterOrEqualf(t, at, 0, "%s is not defined", definition)
		assert.Lessf(t, at, trap, "%s is defined after the exit trap is set", definition)
	}

	reads := 0
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && logRead.MatchString(line) {
			reads++
		}
	}
	assert.GreaterOrEqual(t, reads, 7, "the scan found the log reads of cleanup, the node container read and the printed line")

	for _, bad := range []string{
		`docker logs --tail 200 "${CONTAINER}" 2>&1 | sed 's/^/[rancher] /' || true`,
		`docker exec c kubectl -n cattle-system logs deploy/rancher --tail 200 2>&1 | cat`,
		`docker logs --tail 20 "${CONTAINER}" >&2 2>&1 || true`,
		`docker container logs --tail 50 c 2>&1 | sed 's/^/[k3s] /'`,
		`docker exec c crictl logs abc 2>&1`,
		`docker --context x logs --tail 50 c 2>&1`,
		`docker -H unix:///var/run/docker.sock logs c 2>&1 | sed 's/^/[k3s] /'`,
		`docker logs c 2>&1 | tee raw.log | redact_log "[k3s] " || true`,
		`docker exec c kubectl get pods -o jsonpath='{.lastState}' 2>&1`,
	} {
		got, _ := unredactedLogReads([]string{bad}, nil)
		assert.NotEmptyf(t, got, "the scan passes %s", bad)
	}
	good, _ := unredactedLogReads([]string{`docker logs --tail 50 c 2>&1 | redact_log "[k3s] " || true`}, nil)
	assert.Empty(t, good)
}

// TestOracleBootedIsSetOnlyAtTheTwoSites asserts that ORACLE_BOOTED, which
// makes a stopped container count as the failure of the run, is set only where
// the run has reached its own container: to 0 at the top level next to
// DRIFT_REPORTED, to 1 as the first statement of the reuse branch, and to 1 in
// the fresh boot after the refusal call and the removal of the state file and
// before the container starts.
func TestOracleBootedIsSetOnlyAtTheTwoSites(t *testing.T) {
	lines := scriptLines(t)
	var code []int
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "ORACLE_BOOTED=") {
			code = append(code, i)
		}
	}
	var zero, one []int
	for _, i := range code {
		switch strings.TrimSpace(lines[i]) {
		case "ORACLE_BOOTED=1":
			one = append(one, i)
		case "ORACLE_BOOTED=0":
			require.Equal(t, "ORACLE_BOOTED=0", lines[i], "the initial value is a top-level line")
			zero = append(zero, i)
		}
	}
	require.Len(t, code, len(zero)+len(one), "no other line sets ORACLE_BOOTED")
	require.Len(t, zero, 1, "ORACLE_BOOTED=0 appears once")
	assert.True(t, lines[zero[0]-1] == "DRIFT_REPORTED=0" || lines[zero[0]+1] == "DRIFT_REPORTED=0", "ORACLE_BOOTED=0 is next to DRIFT_REPORTED=0")
	require.Len(t, one, 2, "ORACLE_BOOTED=1 appears twice, each as a line of its own")

	reuseIf := slices.Index(lines, `if [ "${E2E_FIDELITY_KEEP:-}" = "1" ] && reuse_state; then`)
	require.GreaterOrEqual(t, reuseIf, 0, "the reuse test changed shape")
	elseLine := slices.Index(lines[reuseIf:], "else") + reuseIf
	fiLine := slices.Index(lines[reuseIf:], "fi") + reuseIf
	require.Less(t, reuseIf, elseLine)
	require.Less(t, elseLine, fiLine)

	first := ""
	firstAt := -1
	for i := reuseIf + 1; i < elseLine; i++ {
		text := strings.TrimSpace(lines[i])
		if text != "" && !strings.HasPrefix(text, "#") {
			first, firstAt = text, i
			break
		}
	}
	assert.Equal(t, "ORACLE_BOOTED=1", first, "the reuse branch starts with the flag")
	assert.Equal(t, firstAt, one[0])

	refuse, removeState, boot, run := -1, -1, -1, -1
	for i := elseLine + 1; i < fiLine; i++ {
		text := strings.TrimSpace(lines[i])
		switch {
		case text == "refuse_reuse":
			refuse = i
		case text == `rm -f "${STATE_FILE}"` && removeState < 0:
			removeState = i
		case text == "boot_helm" && boot < 0:
			boot = i
		case strings.HasPrefix(text, `docker run -d --name "${CONTAINER}"`) && run < 0:
			run = i
		}
	}
	for name, at := range map[string]int{"refuse_reuse": refuse, "the removal of the state file": removeState, "boot_helm": boot, "docker run -d": run} {
		require.GreaterOrEqualf(t, at, 0, "%s is not in the fresh boot", name)
	}
	flag := one[1]
	assert.Greater(t, flag, refuse, "the flag comes after the refusal call")
	assert.Greater(t, flag, removeState, "the flag comes after the removal of the state file")
	assert.Less(t, flag, boot, "the flag comes before the helm boot")
	assert.Less(t, flag, run, "the flag comes before the container starts")
	assert.Greater(t, flag, elseLine)
	assert.Less(t, flag, fiLine)
}
