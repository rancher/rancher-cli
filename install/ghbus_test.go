package install_test

import (
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

// overrideAddress is the D-Bus address install.sh gives gh when the host has
// no session bus.
const overrideAddress = "disabled:"

// recordingGh builds a dir with a gh that answers the four calls install.sh
// makes and appends one line per call to the returned log: every argument in
// brackets, a bar and the DBUS_SESSION_BUS_ADDRESS it saw (UNSET when absent).
// gh auth token finds no token, so the install goes on anonymously.
// RANCHER_TEST_GH_STATUS_RC and RANCHER_TEST_GH_VERIFY_RC set the exit status
// of gh auth status and of a gh attestation verify run on a file (default 0).
func recordingGh(t *testing.T) (dir, callLog string) {
	t.Helper()
	dir = t.TempDir()
	callLog = filepath.Join(t.TempDir(), "gh-calls")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '[%s]' \"$a\" >> \"$RANCHER_TEST_GH_CALL_LOG\"; done\n" +
		"printf '|%s\\n' \"${DBUS_SESSION_BUS_ADDRESS-UNSET}\" >> \"$RANCHER_TEST_GH_CALL_LOG\"\n" +
		"case \"$1 $2\" in\n" +
		"  'auth token') exit 1 ;;\n" +
		"  'auth status') exit \"${RANCHER_TEST_GH_STATUS_RC:-0}\" ;;\n" +
		"  'attestation verify')\n" +
		"    if [ \"$3\" = --help ]; then echo '--signer-workflow'; exit 0; fi\n" +
		"    exit \"${RANCHER_TEST_GH_VERIFY_RC:-0}\" ;;\n" +
		"esac\nexit 0\n"
	testfs.WriteExecutable(t, filepath.Join(dir, "gh"), []byte(script))
	return dir, callLog
}

// busRecordingCurl builds a dir with a curl that appends the
// DBUS_SESSION_BUS_ADDRESS it saw to the log named by RANCHER_TEST_CURL_BUS_LOG
// and then runs the real curl (RANCHER_TEST_REAL_CURL).
func busRecordingCurl(t *testing.T) (dir string, env []string, busLog string) {
	t.Helper()
	realCurl, err := exec.LookPath("curl")
	require.NoError(t, err)
	dir = t.TempDir()
	busLog = filepath.Join(t.TempDir(), "curl-bus")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"${DBUS_SESSION_BUS_ADDRESS-UNSET}\" >> \"$RANCHER_TEST_CURL_BUS_LOG\"\n" +
		"exec \"$RANCHER_TEST_REAL_CURL\" \"$@\"\n"
	testfs.WriteExecutable(t, filepath.Join(dir, "curl"), []byte(script))
	env = []string{
		"RANCHER_TEST_REAL_CURL=" + realCurl,
		"RANCHER_TEST_CURL_BUS_LOG=" + busLog,
	}
	return dir, env, busLog
}

// mvForFakeKernel builds a dir with an mv that turns the -T and -h flags
// install.sh picks from uname into the flag the host's mv supports, so a full
// install under a fake kernel name runs on any host.
func mvForFakeKernel(t *testing.T) (dir string, env []string) {
	t.Helper()
	realMv, err := exec.LookPath("mv")
	require.NoError(t, err)
	flag := "-T"
	if runtime.GOOS == "darwin" {
		flag = "-h"
	}
	dir = t.TempDir()
	script := "#!/bin/sh\ncase \"${1:-}\" in\n  -T|-h) shift; exec \"$REAL_MV\" \"$RANCHER_TEST_MV_FLAG\" \"$@\" ;;\nesac\nexec \"$REAL_MV\" \"$@\"\n"
	testfs.WriteExecutable(t, filepath.Join(dir, "mv"), []byte(script))
	return dir, []string{"REAL_MV=" + realMv, "RANCHER_TEST_MV_FLAG=" + flag}
}

// registerBusRelease registers v1.2.3 whose binaries append the
// DBUS_SESSION_BUS_ADDRESS they see to the log named by
// RANCHER_TEST_BIN_BUS_LOG when run (the installer runs rancher --version).
func registerBusRelease(s *stub) {
	body := []byte("#!/bin/sh\n" +
		"printf '%s\\n' \"${DBUS_SESSION_BUS_ADDRESS-UNSET}\" >> \"$RANCHER_TEST_BIN_BUS_LOG\"\n" +
		"echo \"rancher version v1.2.3\"\n")
	var assets []stubAsset
	for _, name := range []string{"rancher", "rancher-credential-vault"} {
		for _, plat := range installerPlatforms {
			assets = append(assets, stubAsset{name: name + "-" + plat, body: body})
		}
	}
	registerRelease(s, "v1.2.3", false, assets)
	s.setLatest("v1.2.3")
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// failingID returns a dir whose id prints nothing and exits 1.
func failingID(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testfs.WriteExecutable(t, filepath.Join(dir, "id"), []byte("#!/bin/sh\nexit 1\n"))
	return dir
}

// busRun describes one installer run for the session bus tests.
type busRun struct {
	kernel  string  // what uname -s prints
	idKind  string  // "": a fake id printing a uid with no /run/user directory; "empty": prints nothing; "fails": exits 1
	noUname bool    // leave uname off PATH
	address *string // DBUS_SESSION_BUS_ADDRESS; nil leaves it unset
	xdgBus  bool    // XDG_RUNTIME_DIR names a dir holding a bus file
	env     []string
	args    []string
}

// busResult is what one run recorded.
type busResult struct {
	out, errOut string
	code        int
	callerValue string // the address the caller passed, UNSET when absent
	calls       []string
	curlLines   []string
	binLines    []string
}

// runBusInstall runs a full install with a recording gh, curl and installed
// binary, and returns what they recorded. A mktemp shim puts the work
// directory under a directory with a space in its name (BSD mktemp ignores
// TMPDIR), so an argument that loses its quoting shows in the recorded gh
// arguments.
func runBusInstall(t *testing.T, r busRun) busResult {
	t.Helper()
	s := newStub(t)
	registerBusRelease(s)
	ghDir, ghLog := recordingGh(t)
	curlDir, curlEnv, curlLog := busRecordingCurl(t)
	mvDir, mvEnv := mvForFakeKernel(t)
	binLog := filepath.Join(t.TempDir(), "bin-bus")
	spaceDir := filepath.Join(t.TempDir(), "with space")
	require.NoError(t, os.MkdirAll(spaceDir, 0o755))
	realMktemp, err := exec.LookPath("mktemp")
	require.NoError(t, err)
	mktempDir := t.TempDir()
	testfs.WriteExecutable(t, filepath.Join(mktempDir, "mktemp"), []byte("#!/bin/sh\n"+
		"if [ \"$#\" = 1 ] && [ \"$1\" = -d ]; then exec \"$RANCHER_TEST_REAL_MKTEMP\" -d \"$RANCHER_TEST_SPACE_DIR/tmp.XXXXXX\"; fi\n"+
		"exec \"$RANCHER_TEST_REAL_MKTEMP\" \"$@\"\n"))

	var idDir string
	switch r.idKind {
	case "":
		idDir = fakeID(t, "4242424242")
	case "empty":
		idDir = fakeID(t, "")
	case "fails":
		idDir = failingID(t)
	}
	tools := baseTools
	dirs := []string{ghDir, curlDir, mvDir, mktempDir, idDir}
	if r.noUname {
		tools = slices.DeleteFunc(slices.Clone(baseTools), func(s string) bool { return s == "uname" })
	} else {
		dirs = append(dirs, fakeUname(t, r.kernel, "x86_64"))
	}
	dirs = append(dirs, toolPath(t, tools...))
	env := slices.Concat([]string{
		"PATH=" + strings.Join(dirs, string(os.PathListSeparator)),
		"RANCHER_TEST_REAL_MKTEMP=" + realMktemp,
		"RANCHER_TEST_SPACE_DIR=" + spaceDir,
		"RANCHER_TEST_GH_CALL_LOG=" + ghLog,
		"RANCHER_TEST_BIN_BUS_LOG=" + binLog,
	}, curlEnv, mvEnv, r.env)
	res := busResult{callerValue: "UNSET"}
	if r.address != nil {
		env = append(env, "DBUS_SESSION_BUS_ADDRESS="+*r.address)
		res.callerValue = *r.address
	}
	if r.xdgBus {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bus"), nil, 0o600))
		env = append(env, "XDG_RUNTIME_DIR="+dir)
	}

	res.out, res.errOut, res.code = runInstall(t, s, env, r.args...)
	res.calls = readLinesIfExists(t, ghLog)
	res.curlLines = readLinesIfExists(t, curlLog)
	res.binLines = readLinesIfExists(t, binLog)
	return res
}

func readLinesIfExists(t *testing.T, path string) []string {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return readLines(t, path)
}

// splitCall splits a recorded gh call into its bracketed arguments and the
// address gh saw.
func splitCall(t *testing.T, call string) (argv, seen string) {
	t.Helper()
	i := strings.LastIndex(call, "|")
	require.GreaterOrEqual(t, i, 0, "no recorded address: %q", call)
	return call[:i], call[i+1:]
}

// TestGhRunSessionBusRows asserts, over a full install that makes every gh
// call (token lookup, both capability probes, one verification per binary),
// that gh runs with an address that cannot start a bus when a Linux host has
// no session bus, and with the caller's environment unchanged otherwise. It
// also asserts that the override never reaches curl or the installed binary,
// and that every gh argument arrives whole. XDG_RUNTIME_DIR does not count as
// a bus: the D-Bus library never reads it. A failing id, an id that prints
// nothing and a missing uname each leave gh with the caller's environment.
// The uid comes from a fake id, so no row depends on the host's /run/user; a
// bus at /run/user/<uid>/bus cannot be created by a test.
func TestGhRunSessionBusRows(t *testing.T) {
	t.Parallel()
	const realLooking = "unix:path=/run/user/1000/bus"
	rows := []struct {
		name string
		run  busRun
		want string // what gh must see; "" means the caller's value
	}{
		{name: "unset", run: busRun{kernel: "Linux"}, want: overrideAddress},
		{name: "empty", run: busRun{kernel: "Linux", address: new("")}, want: overrideAddress},
		{name: "autolaunch", run: busRun{kernel: "Linux", address: new("autolaunch:")}, want: overrideAddress},
		{name: "runtime dir with a bus file", run: busRun{kernel: "Linux", xdgBus: true}, want: overrideAddress},
		{name: "real looking address", run: busRun{kernel: "Linux", address: new(realLooking)}},
		{name: "other kernel", run: busRun{kernel: "Darwin"}},
		{name: "id prints nothing", run: busRun{kernel: "Linux", idKind: "empty"}},
		{name: "id fails", run: busRun{kernel: "Linux", idKind: "fails"}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := runBusInstall(t, tc.run)
			require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
			want := tc.want
			if want == "" {
				want = res.callerValue
			}

			prefixes := []string{
				"[auth][token][--hostname][127.0.0.1]|",
				"[attestation][verify][--help]|",
				"[auth][status]|",
				"[attestation][verify][",
				"[attestation][verify][",
			}
			require.Len(t, res.calls, len(prefixes), "gh calls: %q", res.calls)
			for i, call := range res.calls {
				assert.Truef(t, strings.HasPrefix(call, prefixes[i]), "call %d: %q", i, call)
				argv, seen := splitCall(t, call)
				assert.Equalf(t, want, seen, "address gh saw for %q", argv)
				if i >= 3 {
					assert.Contains(t, argv, "with space/", "the file argument must arrive whole")
					assert.Truef(t, strings.HasSuffix(argv,
						"][--repo][rancher/rancher-cli][--signer-workflow][rancher/rancher-cli/.github/workflows/release.yaml]"),
						"verify arguments: %q", argv)
				}
			}

			for _, line := range res.curlLines {
				assert.Equal(t, res.callerValue, line, "curl must see the caller's address")
			}
			assert.Equal(t, []string{res.callerValue}, res.binLines,
				"the installed binary must see the caller's address")
		})
	}
}

// TestGhRunMissingUname asserts that with no uname on PATH the token lookup
// still runs gh once with the caller's environment, and the installer then
// stops at its own tool check rather than at a shell error.
func TestGhRunMissingUname(t *testing.T) {
	t.Parallel()
	res := runBusInstall(t, busRun{noUname: true})
	assert.NotEqual(t, 0, res.code)
	assert.Contains(t, res.errOut, "required tool not found: uname")
	require.Len(t, res.calls, 1, "gh calls: %q", res.calls)
	argv, seen := splitCall(t, res.calls[0])
	assert.Equal(t, "[auth][token][--hostname][127.0.0.1]", argv)
	assert.Equal(t, "UNSET", seen)
}

// TestGhRunPassesGhExitStatus asserts that gh's exit status reaches the
// capability check and the verification: a gh that is not logged in skips the
// attestation (or fails it under --verify-attestation), and a failed
// verification stops the install.
func TestGhRunPassesGhExitStatus(t *testing.T) {
	t.Parallel()
	t.Run("not logged in", func(t *testing.T) {
		t.Parallel()
		res := runBusInstall(t, busRun{kernel: "Linux", env: []string{"RANCHER_TEST_GH_STATUS_RC=1"}})
		require.Equalf(t, 0, res.code, "stderr: %s", res.errOut)
		assert.Contains(t, res.out+res.errOut, "notice: attestation skipped")
		assert.NotContains(t, res.out+res.errOut, "build provenance attestation verified")
		assert.Len(t, res.calls, 3, "no verification runs: %q", res.calls)
	})
	t.Run("not logged in with the flag", func(t *testing.T) {
		t.Parallel()
		res := runBusInstall(t, busRun{kernel: "Linux", env: []string{"RANCHER_TEST_GH_STATUS_RC=1"},
			args: []string{"--verify-attestation"}})
		require.NotEqual(t, 0, res.code)
		assert.Contains(t, res.errOut, "--verify-attestation needs a gh CLI")
	})
	t.Run("verification fails", func(t *testing.T) {
		t.Parallel()
		res := runBusInstall(t, busRun{kernel: "Linux", env: []string{"RANCHER_TEST_GH_VERIFY_RC=1"}})
		require.NotEqual(t, 0, res.code)
		assert.Contains(t, res.errOut, "build provenance attestation failed")
	})
}

// stripShellForScan returns the shell source with comments, quoted strings
// and quoted-delimiter here-document bodies removed. The contents of command
// substitutions and backticks stay, wherever they sit, including inside
// double quotes and unquoted here-document bodies.
func stripShellForScan(t *testing.T, src string) string {
	t.Helper()
	out, rest := stripShell(src, false)
	require.Equal(t, "n", rest, "the scan ended inside an unterminated quote or substitution")
	return out
}

type pendingHeredoc struct {
	delim  string
	quoted bool
	strip  bool
}

// stripShell scans src. In body mode the text starts inside an unquoted
// here-document, where a double quote is an ordinary character. It returns the
// kept text and the final context stack, one letter per level: n normal, d
// double quote, s single quote, ( command substitution or group, ` backticks.
func stripShell(src string, body bool) (string, string) {
	var out strings.Builder
	stack := []byte{'n'}
	if body {
		stack = []byte{'n', 'd'}
	}
	var pending []pendingHeredoc
	for i := 0; i < len(src); i++ {
		c := src[i]
		top := stack[len(stack)-1]
		switch top {
		case 's':
			if c == '\'' {
				stack = stack[:len(stack)-1]
				out.WriteByte(' ')
			}
		case 'd':
			switch {
			case c == '\\':
				i++
			case c == '"' && !body:
				stack = stack[:len(stack)-1]
				out.WriteByte(' ')
			case c == '$' && i+1 < len(src) && src[i+1] == '(':
				stack = append(stack, '(')
				out.WriteByte(' ')
				i++
			case c == '`':
				stack = append(stack, '`')
				out.WriteByte(' ')
			}
		default:
			switch {
			case c == '\\':
				out.WriteByte(' ')
				i++
			case c == '\'':
				stack = append(stack, 's')
			case c == '"':
				stack = append(stack, 'd')
			case c == '`':
				if top == '`' {
					stack = stack[:len(stack)-1]
				} else {
					stack = append(stack, '`')
				}
				out.WriteByte(' ')
			case c == '(':
				stack = append(stack, '(')
				out.WriteByte(c)
			case c == ')':
				if top == '(' {
					stack = stack[:len(stack)-1]
				}
				out.WriteByte(c)
			case c == '#' && (i == 0 || strings.IndexByte(" \t\n;(&|", src[i-1]) >= 0):
				for i < len(src) && src[i] != '\n' {
					i++
				}
				i--
			case c == '<' && i+2 < len(src) && src[i+1] == '<' && src[i+2] != '<':
				j := i + 2
				h := pendingHeredoc{}
				if src[j] == '-' {
					h.strip = true
					j++
				}
				for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
					j++
				}
				start := j
				if src[j] == '\'' || src[j] == '"' {
					h.quoted = true
					q := src[j]
					j++
					start = j
					for j < len(src) && src[j] != q {
						j++
					}
					h.delim = src[start:j]
					j++
				} else {
					for j < len(src) && strings.IndexByte(" \t\n;&|)<>", src[j]) < 0 {
						j++
					}
					h.delim = src[start:j]
				}
				pending = append(pending, h)
				i = j - 1
			case c == '\n' && len(pending) > 0:
				out.WriteByte('\n')
				i++
				for _, h := range pending {
					start := i
					for i < len(src) {
						end := strings.IndexByte(src[i:], '\n')
						line := src[i:]
						if end >= 0 {
							line = src[i : i+end]
						}
						check := line
						if h.strip {
							check = strings.TrimLeft(line, "\t")
						}
						if check == h.delim {
							break
						}
						i += len(line)
						if end >= 0 {
							i++
						}
					}
					if !h.quoted {
						kept, _ := stripShell(src[start:i], true)
						out.WriteString(kept)
					}
					for i < len(src) && src[i] != '\n' {
						i++
					}
					if i < len(src) {
						i++
					}
				}
				pending = nil
				i--
			default:
				out.WriteByte(c)
			}
		}
	}
	return out.String(), string(stack)
}

var (
	ghWrapperBody = regexp.MustCompile(`(?s)\ngh_run\(\) \{\n.*?\n\}\n`)
	ghWord        = regexp.MustCompile(`\bgh\b`)
	ghProbe       = regexp.MustCompile(`\bcommand -v gh >/dev/null 2>&1`)
	ghProbeAny    = regexp.MustCompile(`\bcommand -v gh\b`)
	busAddress    = regexp.MustCompile(`DBUS_SESSION_BUS_ADDRESS`)
	ghIndirect    = regexp.MustCompile(`\b(env|exec|eval|xargs)\b`)
	commandWord   = regexp.MustCompile(`\bcommand\b`)
	commandProbe  = regexp.MustCompile(`\bcommand -v\b`)
)

// ghScanViolations returns what is wrong with how an installer source runs gh:
// the only call is the one inside gh_run, `command -v gh >/dev/null 2>&1` is the
// only other use of the word (its output is never captured, so gh is never
// called through a variable), no wrapper command (env, exec, eval, xargs,
// command) can name gh through a string, and DBUS_SESSION_BUS_ADDRESS appears
// only in gh_run.
func ghScanViolations(t *testing.T, src string) []string {
	t.Helper()
	code := stripShellForScan(t, src)
	loc := ghWrapperBody.FindStringIndex(code)
	if loc == nil {
		return []string{"no gh_run function"}
	}
	body, outside := code[loc[0]:loc[1]], code[:loc[0]]+code[loc[1]:]
	var found []string
	if got := len(ghWord.FindAllString(body, -1)); got != 1 {
		found = append(found, "gh_run must hold exactly one gh word, has "+strconv.Itoa(got))
	}
	rest := ghProbeAny.ReplaceAllString(outside, " ")
	for _, m := range ghProbeAny.FindAllStringIndex(outside, -1) {
		if !ghProbe.MatchString(outside[m[0]:min(len(outside), m[0]+len("command -v gh >/dev/null 2>&1"))]) {
			found = append(found, "command -v gh output is used: "+strings.TrimSpace(lineAt(outside, m[0])))
		}
	}
	for _, m := range ghWord.FindAllStringIndex(rest, -1) {
		found = append(found, "gh word outside gh_run: "+strings.TrimSpace(lineAt(rest, m[0])))
	}
	for _, m := range ghIndirect.FindAllStringIndex(code, -1) {
		found = append(found, "indirect command word: "+strings.TrimSpace(lineAt(code, m[0])))
	}
	if len(commandWord.FindAllString(code, -1)) != len(commandProbe.FindAllString(code, -1)) {
		found = append(found, "command is used for something other than command -v")
	}
	for _, m := range busAddress.FindAllStringIndex(outside, -1) {
		found = append(found, "DBUS_SESSION_BUS_ADDRESS outside gh_run: "+strings.TrimSpace(lineAt(outside, m[0])))
	}
	return found
}

func lineAt(s string, pos int) string {
	start := strings.LastIndexByte(s[:pos], '\n') + 1
	end := strings.IndexByte(s[pos:], '\n')
	if end < 0 {
		return s[start:]
	}
	return s[start : pos+end]
}

// TestInstallRunsGhOnlyThroughGhRun asserts that every gh call in install.sh
// goes through gh_run, and that each rule of the scan catches a mutation of the
// source: a direct call, a call behind command, a call inside a command
// substitution in double quotes, a call behind env, a call named by a string
// (eval, and command with a quoted name), a call through a variable that holds
// the output of command -v gh, and the address set outside gh_run. The last
// four rows have a mutation that no other rule catches.
func TestInstallRunsGhOnlyThroughGhRun(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../install.sh")
	require.NoError(t, err)
	src := string(raw)
	assert.Empty(t, ghScanViolations(t, src))

	const site = "  gh_run auth status >/dev/null 2>&1 || return 1"
	for _, tc := range []struct {
		name, from, to, want string
		only                 bool // no other rule may report it
	}{
		{"direct call", site, "  gh auth status >/dev/null 2>&1 || return 1", "gh word outside gh_run", false},
		{"command prefix", site, "  command gh auth status >/dev/null 2>&1 || return 1", "gh word outside gh_run", false},
		{"in a substitution", `"$(gh_run auth token --hostname`, `"$(gh auth token --hostname`, "gh word outside gh_run", false},
		{"in an env wrapper", site, "  env gh auth status >/dev/null 2>&1 || return 1", "indirect command word", false},
		{"through eval", site, "  eval 'gh auth status' >/dev/null 2>&1 || return 1", "indirect command word", true},
		{"command with a quoted name", site, `  command "gh" auth status >/dev/null 2>&1 || return 1`, "command is used for something other", true},
		{"through a variable", site, `  _gh="$(command -v gh)"; "$_gh" auth status >/dev/null 2>&1 || return 1`, "command -v gh output is used", true},
		{"address set outside", "gh_capable() {\n", "gh_capable() {\n  DBUS_SESSION_BUS_ADDRESS=x\n", "DBUS_SESSION_BUS_ADDRESS outside gh_run", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, src, tc.from, "the mutation site must exist")
			found := ghScanViolations(t, strings.Replace(src, tc.from, tc.to, 1))
			assert.Truef(t, slices.ContainsFunc(found, func(v string) bool { return strings.HasPrefix(v, tc.want) }),
				"want a violation starting %q, got %q", tc.want, found)
			if tc.only {
				assert.Lenf(t, found, 1, "only one rule should report it: %q", found)
			}
		})
	}
}
