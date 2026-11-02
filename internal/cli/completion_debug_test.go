package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// runCompletionTraced runs one completion request through the
// real root and returns stdout and the captured trace. The trace sink is
// swapped for the call so nothing reaches the test process's stderr.
func runCompletionTraced(t *testing.T, words []string) (stdout, trace string) {
	t.Helper()
	origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
	origOut := completionDebugOut
	var traceBuf bytes.Buffer
	completionDebugOut = &traceBuf
	t.Cleanup(func() {
		rawArgv = origArgv
		rawCurrentWord = origWord
		rawCompleting = origProto
		completionDebugOut = origOut
	})
	scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
	scriptArgs = append(scriptArgs, "--rancher-completion-cur=", "--generate-shell-completion")
	CaptureArgv(scriptArgs)
	root, err := BuildRoot("test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
	return out.String(), traceBuf.String()
}

// TestCompletionDebugTrace asserts the RANCHER_CLI_COMPLETION_DEBUG seam: with
// the variable set, a live request writes one line per phase to the trace
// sink and stdout is byte-identical to the untraced run; with it unset,
// nothing is traced.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestCompletionDebugTrace(t *testing.T) {
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)

	setupIn := func(t *testing.T, dir string, networkOn bool) {
		t.Helper()
		keyring.MockInit()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfigForTest(t, cfg, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv(config.EnvCredentialStore, "file")
	}
	setup := func(t *testing.T, networkOn bool) string {
		t.Helper()
		dir := t.TempDir()
		setupIn(t, dir, networkOn)
		t.Setenv(config.EnvConfigDir, dir)
		return dir
	}

	t.Run("trace off leaves stderr untouched", func(t *testing.T) {
		setup(t, true)
		t.Setenv(config.EnvCompletionDebug, "")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Equal(t, "c-abc:prod\nprod\n", stdout)
		assert.Empty(t, trace)
	})

	t.Run("trace on records every phase and keeps stdout identical", func(t *testing.T) {
		setup(t, true)
		t.Setenv(config.EnvCompletionDebug, "1")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Equal(t, "c-abc:prod\nprod\n", stdout)

		for l := range strings.SplitSeq(strings.TrimSuffix(trace, "\n"), "\n") {
			assert.True(t, strings.HasPrefix(l, "completion +"), "every trace line carries the elapsed-time prefix: %q", l)
		}
		phases := []string{
			"config loaded:",
			"time limit: 500ms",
			"connection: url=" + srv.URL,
			"token: RANCHER_CLI_TOKEN (full capability)",
			"source: claimed by the managementcluster kind source",
			"GET /k8s/clusters/local/apis/management.cattle.io/v3/clusters: HTTP 200",
			"source: 1 requests, 2 names, 1 descriptions",
			"done: 2 candidates",
		}
		for _, p := range phases {
			assert.Contains(t, trace, p)
		}
	})

	t.Run("trace names the reason for an empty result", func(t *testing.T) {
		setup(t, true)
		t.Setenv(config.EnvCompletionDebug, "1")
		t.Setenv(config.EnvToken, "")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Empty(t, stdout)
		assert.Contains(t, trace, `file store skipped: no login token stored for current server "srv"`)
		assert.NotContains(t, trace, "GET ", "no request is issued without a token")
		assert.Contains(t, trace, "stop: no token:")
		assert.Contains(t, trace, "done: 0 candidates")
	})

	t.Run("trace names a directory other than the default in the command it suggests", func(t *testing.T) {
		dir := setup(t, false)
		t.Setenv(config.EnvCompletionDebug, "1")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Empty(t, stdout)
		assert.Contains(t, trace, "live completion off; enable with: rancher --config "+clierr.ShellQuote(remedyDirFor(dir))+" config set-completion --network=true")
		assert.NotContains(t, trace, clierr.RemedyMarker, "no marker is left")
	})

	t.Run("trace names no directory when it is the default", func(t *testing.T) {
		testfs.SkipOnWindows(t, "the default directory comes from APPDATA there")
		directoryRuleEnv(t)
		dir := platformDefaultDir(t)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		setupIn(t, dir, false)
		t.Setenv(config.EnvCompletionDebug, "1")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Empty(t, stdout)
		assert.Contains(t, trace, "live completion off; enable with: rancher config set-completion --network=true")
		assert.NotContains(t, trace, "--config")
		assert.NotContains(t, trace, clierr.RemedyMarker, "no marker is left")
	})

	t.Run("trace keeps its reason when the directory cannot be printed", func(t *testing.T) {
		// A name with a control character is refused on NTFS: the config load
		// fails there and no directory is stored, so the line names none.
		testfs.SkipOnWindows(t, "NTFS refuses a control character in a name")
		keyring.MockInit()
		// A missing file loads as an empty config; the path holds U+0001
		// START OF HEADING, which a terminal cannot show.
		t.Setenv(config.EnvConfigDir, filepath.Join(t.TempDir(), "a\u0001b"))
		t.Setenv(config.EnvCredentialStore, "file")
		t.Setenv(config.EnvCompletionDebug, "1")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Empty(t, stdout)
		assert.Contains(t, trace, "live completion off")
		assert.Contains(t, trace, runtime.UnprintableConfigDirNote)
		assert.NotContains(t, trace, clierr.RemedyMarker, "no marker is left")
		assert.NotContains(t, trace, "config set-completion", "no command is offered")
		assert.NotContains(t, trace, "\u0001")
		var loaded string
		for l := range strings.SplitSeq(trace, "\n") {
			if strings.Contains(l, "config loaded:") {
				loaded = l
			}
		}
		assert.Contains(t, loaded, "ab currentServer=", "the config loaded line holds the path without the control character")
		for _, r := range strings.ReplaceAll(trace, "\n", "") {
			assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "U+%04X reaches the terminal", r)
		}
	})
}

// TestCompletionDebugSilentOutsideCompletion asserts that the trace is bound to
// a completion request: with the variable set but no completion protocol
// active, the helper writes nothing.
func TestCompletionDebugSilentOutsideCompletion(t *testing.T) {
	origProto, origOut := rawCompleting, completionDebugOut
	var traceBuf bytes.Buffer
	completionDebugOut = &traceBuf
	rawCompleting = false
	t.Cleanup(func() {
		rawCompleting = origProto
		completionDebugOut = origOut
	})
	t.Setenv(config.EnvCompletionDebug, "1")
	completionDebugf("must not appear")
	assert.Empty(t, traceBuf.String())

	rawCompleting = true
	completionDebugf("phase %d", 7)
	assert.True(t, strings.HasPrefix(traceBuf.String(), "completion +"))
	assert.True(t, strings.HasSuffix(traceBuf.String(), " phase 7\n"))
}

// TestCompletionTraceAndRefusalForAServerURLWithUserinfo asserts that a
// completion request with a server URL that carries userinfo (the variable,
// or the current server entry) is treated like a missing URL: the trace holds
// no part of the userinfo, the capability is the zero value, no credential is
// read and no request reaches the server. A URL without userinfo is traced as
// scheme and host only.
// NOT parallel: it mutates env globals and the stored config.
func TestCompletionTraceAndRefusalForAServerURLWithUserinfo(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	withUserinfo := "http://u:" + urlCanary + "@" + host + "/pathzq7"

	for _, tc := range []struct {
		name, stored, variable string
	}{
		{"the variable", srv.URL, withUserinfo},
		{"the current server entry", withUserinfo, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests = 0
			keyring.MockInit()
			dir := t.TempDir()
			cfg := &config.Config{
				CurrentServer: "srv",
				Servers:       map[string]config.Server{"srv": {URL: tc.stored}},
				Completion:    config.CompletionConfig{Network: true},
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			setStoredConfigForTest(t, cfg, dir)
			t.Setenv(config.EnvConfigDir, dir)
			t.Setenv(config.EnvToken, "mgmt-token")
			t.Setenv(config.EnvURL, tc.variable)
			t.Setenv(config.EnvCredentialStore, "file")
			t.Setenv(config.EnvCompletionDebug, "1")

			origProto, origOut := rawCompleting, completionDebugOut
			var trace bytes.Buffer
			completionDebugOut, rawCompleting = &trace, true
			t.Cleanup(func() { rawCompleting, completionDebugOut = origProto, origOut })

			got := resolveCompletionToken("c-1")
			assert.Equal(t, TokenCapability{}, got)
			assert.Zero(t, requests, "no request reaches the server")
			lower := strings.ToLower(trace.String())
			assert.NotContains(t, lower, strings.ToLower(urlCanary))
			assert.NotContains(t, lower, "pathzq7")
			assert.Contains(t, lower, "connection: url="+strings.ToLower(srv.URL), "the URL is traced as scheme and host")
		})
	}

	t.Run("an unsupported scheme is traced as scheme and host", func(t *testing.T) {
		origProto, origOut := rawCompleting, completionDebugOut
		var trace bytes.Buffer
		completionDebugOut, rawCompleting = &trace, true
		t.Cleanup(func() { rawCompleting, completionDebugOut = origProto, origOut })
		t.Setenv(config.EnvCompletionDebug, "1")
		_, ok := completionServerURL("ftp://hostzq7/pathzq7?q=1")
		assert.False(t, ok)
		assert.Contains(t, trace.String(), "scheme unsupported: ftp://hostzq7\n")
		assert.NotContains(t, trace.String(), "pathzq7")
	})
}

// traceHostile holds what a path, a typed word or an error text can carry to
// drive a terminal or to look like another line: a clear-screen and a conceal
// sequence, the one-byte sequence introducer, bell, backspace, carriage
// return, a newline, NUL, DEL, tab, a bidi override, a byte order mark, a
// bare U+2063 and the line and paragraph separators.
const traceHostile = "\x1b[2J\x1b[8m\u009b2J\a\b\r\n\x00\x7f\t\u202e\ufeff\u2063\u2028\u2029"

// traceFiltered is traceHostile with every character a terminal line cannot
// show removed.
const traceFiltered = "[2J[8m2J"

// tracedLine calls completionDebugf with the trace on and returns the text
// after the elapsed-time prefix. It asserts that the write is one line with
// one "\n" at its end and no character a terminal line cannot show, and no
// invalid UTF-8 byte, before it.
func tracedLine(t *testing.T, format string, args ...any) string {
	t.Helper()
	origProto, origOut := rawCompleting, completionDebugOut
	var buf bytes.Buffer
	completionDebugOut, rawCompleting = &buf, true
	defer func() { rawCompleting, completionDebugOut = origProto, origOut }()
	completionDebugf(format, args...)
	written := buf.String()
	require.True(t, strings.HasPrefix(written, "completion +"), "the line carries the elapsed-time prefix: %q", written)
	assert.Equal(t, 1, strings.Count(written, "\n"), "one newline: %q", written)
	require.True(t, strings.HasSuffix(written, "\n"), "the newline ends the line: %q", written)
	body := strings.TrimSuffix(written, "\n")
	assert.True(t, utf8.ValidString(body), "no invalid byte reaches the terminal: %q", body)
	for _, r := range body {
		assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029',
			"U+%04X reaches the terminal: %q", r, body)
	}
	_, rest, ok := strings.Cut(strings.TrimPrefix(body, "completion +"), " ")
	require.True(t, ok, "the prefix ends with a space: %q", body)
	return rest
}

// TestCompletionTraceRemovesWhatATerminalCannotShow asserts that each trace
// line is written with every control and Unicode format character, line and
// paragraph separator and newline of its values removed, for the value shapes
// the call sites insert: a path, an error text that holds a path, a typed
// word as %s and as %q, a server URL, a byte that is not valid UTF-8, and a
// remedy command in an error text.
// A remedy command of this process is expanded and every other remedy marker
// is removed; a backslash and a colon of a Windows path stay.
// NOT parallel: it mutates the completion state, the stored config and the
// environment.
func TestCompletionTraceRemovesWhatATerminalCannotShow(t *testing.T) {
	t.Setenv(config.EnvCompletionDebug, "1")
	dir := t.TempDir()
	cfg := &config.Config{CurrentServer: "srv", Servers: map[string]config.Server{"srv": {URL: "https://rancher.example"}}}
	cfg.SetPath(config.FilePath(dir))
	setStoredConfigForTest(t, cfg, dir)
	expandedCommand := "rancher --config " + clierr.ShellQuote(remedyDirFor(dir)) + " config set-completion --network=true"

	t.Run("a path as %s and a server name as %q", func(t *testing.T) {
		got := tracedLine(t, "config loaded: dir=%s currentServer=%q servers=%d", "/p/"+traceHostile+"/q", "srv"+traceHostile, 3)
		assert.Equal(t, "config loaded: dir=/p/"+traceFiltered+"/q currentServer="+strconv.Quote("srv"+traceHostile)+" servers=3", got)
	})

	t.Run("an error text that holds a path", func(t *testing.T) {
		got := tracedLine(t, "config load failed: %v", fmt.Errorf("parsing %s: bad", "/p/"+traceHostile+"/config.yaml"))
		assert.Equal(t, "config load failed: parsing /p/"+traceFiltered+"/config.yaml: bad", got)
	})

	t.Run("a typed word as %s", func(t *testing.T) {
		got := tracedLine(t, "stop: no usable exec-credential cache record for cluster %s", "c-"+traceHostile)
		assert.Equal(t, "stop: no usable exec-credential cache record for cluster c-"+traceFiltered, got)
	})

	t.Run("a typed word as %q", func(t *testing.T) {
		got := tracedLine(t, "stop: --cluster %q is not an id and an exec-credential-only token cannot resolve a display name", traceHostile)
		assert.Equal(t, "stop: --cluster "+strconv.Quote(traceHostile)+" is not an id and an exec-credential-only token cannot resolve a display name", got)
	})

	t.Run("a server URL", func(t *testing.T) {
		got := tracedLine(t, "connection: url=%s ca=%t", "https://host\u202e.example", false)
		assert.Equal(t, "connection: url=https://host.example ca=false", got)
	})

	t.Run("a server URL host that DisplayServerURL shows as it is", func(t *testing.T) {
		host := "https://host\u009b\u202e\u2063.example"
		shown := config.DisplayServerURL(host)
		for _, r := range []string{"\u009b", "\u202e", "\u2063"} {
			require.Contains(t, shown, r, "DisplayServerURL keeps U+%04X", []rune(r)[0])
		}
		got := tracedLine(t, "connection: url=%s ca=%t", shown, true)
		assert.Equal(t, "connection: url=https://host.example ca=true", got)
	})

	t.Run("a remedy command alone is expanded", func(t *testing.T) {
		got := tracedLine(t, "live completion off; enable with: %s config set-completion --network=true", clierr.Rancher)
		assert.Equal(t, "live completion off; enable with: "+expandedCommand, got)
		assert.NotContains(t, got, clierr.RemedyMarker)
	})

	t.Run("a remedy command and hostile text in one error text", func(t *testing.T) {
		err := fmt.Errorf("%s config set-completion --network=true: %s", clierr.Rancher, traceHostile)
		got := tracedLine(t, "config load failed: %v", err)
		assert.Equal(t, "config load failed: "+expandedCommand+": "+traceFiltered, got)
		assert.NotContains(t, got, clierr.RemedyMarker)
	})

	t.Run("a marker from another process is removed", func(t *testing.T) {
		foreign := "\u2063" + strings.Repeat("\u2060\u2061\u2062\u2064", 8)
		require.NotEqual(t, clierr.RemedyMarker, foreign)
		got := tracedLine(t, "stop: %s", foreign+"rancher delete x")
		assert.Equal(t, "stop: rancher delete x", got)
		assert.NotContains(t, got, "--config")
	})

	t.Run("Windows paths and escape-looking text stay as they are", func(t *testing.T) {
		for _, v := range []string{
			`C:\Users\a b\AppData\Roaming\rancher`,
			`\\server\share\rancher`,
			`C:\new\run\tab`,
		} {
			assert.Equal(t, "config loaded: dir="+v+" servers=0", tracedLine(t, "config loaded: dir=%s servers=%d", v, 0))
		}
		assert.Equal(t, "dir=ab", tracedLine(t, "dir=%s", "a\r\nb"))
	})

	t.Run("a raw 0x9b byte becomes U+FFFD", func(t *testing.T) {
		got := tracedLine(t, "dir=%s", "/p/a\x9b2Jb")
		assert.Equal(t, "dir=/p/a\ufffd2Jb", got)
	})

	t.Run("a printable path stays as it is", func(t *testing.T) {
		for _, v := range []string{"/var/lib/rancher", "/home/\u00fcser/\u4e2d\u6587"} {
			assert.Equal(t, "dir="+v, tracedLine(t, "dir=%s", v))
		}
	})
}
