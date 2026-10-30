package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
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

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		keyring.MockInit()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfigForTest(t, cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv(config.EnvCredentialStore, "file")
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

	t.Run("trace reports the network gate", func(t *testing.T) {
		setup(t, false)
		t.Setenv(config.EnvCompletionDebug, "1")
		stdout, trace := runCompletionTraced(t, []string{"get", "cluster"})
		assert.Empty(t, stdout)
		assert.Contains(t, trace, "live completion off: enable with: rancher config set-completion --network=true")
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
